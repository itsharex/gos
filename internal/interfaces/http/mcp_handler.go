package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"expvar"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gos/internal/application/usecase"
	domain "gos/internal/domain/release"
	userdomain "gos/internal/domain/user"
)

// MCPTokenResolver validates a dedicated MCP bearer token on every HTTP request.
// It must reject expired, revoked and disabled credentials; it must not trust a
// user identity or role supplied by the MCP client.
type MCPTokenResolver interface {
	ResolveMCPToken(ctx context.Context, token string) (userdomain.User, error)
}

// MCPServices supplies the same use cases and permission checker as the REST API.
// All fields except Settings are required. Settings powers list_release_environments.
type MCPServices struct {
	TokenResolver MCPTokenResolver
	Release       *ReleaseOrderHandler
	Applications  *ApplicationHandler
	Templates     *ReleaseTemplateHandler
	Settings      *usecase.QueryReleaseSettings
}

type mcpPrincipalKey struct{}
type mcpTraceKey struct{}

// MCPToolCalls is an expvar counter map for successful, denied and failed tool
// invocations. Mount expvar.Handler() only on an internal monitoring endpoint.
var MCPToolCalls = expvar.NewMap("gos_mcp_tool_calls_total")

type mcpAPI struct {
	MCPServices
	logger *slog.Logger
}

// NewMCPHandler returns a stateless Streamable HTTP MCP endpoint. Mount it on
// an isolated POST route outside the existing session auth middleware. The
// supplied token resolver and RequestAuthorizer are checked on every call.
func NewMCPHandler(services MCPServices) http.Handler {
	api := &mcpAPI{MCPServices: services, logger: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	server := mcp.NewServer(&mcp.Implementation{Name: "gos-release", Version: "1.0.0"}, nil)
	api.addReadTools(server)
	api.addWriteTools(server)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.TrimSpace(r.Header.Get("Origin")) != "" {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.URL.Query().Has("access_token") {
			http.Error(w, "query credentials are not allowed", http.StatusBadRequest)
			return
		}
		if services.TokenResolver == nil || services.Release == nil || services.Release.manager == nil || services.Release.authz == nil ||
			services.Applications == nil || services.Applications.query == nil || services.Applications.authz == nil ||
			services.Templates == nil || services.Templates.manager == nil || services.Templates.authz == nil {
			http.Error(w, "MCP services are not configured", http.StatusServiceUnavailable)
			return
		}
		token := extractBearerToken(r.Header.Get("Authorization"))
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := services.TokenResolver.ResolveMCPToken(r.Context(), token)
		if err != nil || strings.TrimSpace(user.ID) == "" || user.Status != userdomain.StatusActive {
			// Never disclose whether a token, user or account status caused rejection.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		traceID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if traceID == "" {
			traceID = strings.TrimSpace(r.Header.Get("Traceparent"))
		}
		if traceID == "" {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err != nil {
				http.Error(w, "unable to initialize request", http.StatusServiceUnavailable)
				return
			}
			traceID = hex.EncodeToString(raw[:])
		}
		ctx := context.WithValue(r.Context(), mcpPrincipalKey{}, user)
		ctx = context.WithValue(ctx, mcpTraceKey{}, traceID)
		start := time.Now()
		api.logger.Info("mcp_request", "trace_id", traceID, "user_id", user.ID, "method", r.Method)
		transport.ServeHTTP(w, r.WithContext(ctx))
		api.logger.Info("mcp_request_done", "trace_id", traceID, "user_id", user.ID, "duration_ms", time.Since(start).Milliseconds())
	})
}

var errMCPForbidden = errors.New("forbidden: permission denied")
var errMCPUnauthorized = errors.New("unauthorized")
var errMCPNotConfigured = errors.New("service is not configured")

func mcpUser(ctx context.Context) (userdomain.User, error) {
	user, ok := ctx.Value(mcpPrincipalKey{}).(userdomain.User)
	if !ok || strings.TrimSpace(user.ID) == "" || user.Status != userdomain.StatusActive {
		return userdomain.User{}, errMCPUnauthorized
	}
	return user, nil
}

func (a *mcpAPI) permission(ctx context.Context, code, appID, env string) error {
	user, err := mcpUser(ctx)
	if err != nil {
		return err
	}
	if a.Release == nil || a.Release.authz == nil {
		return errMCPNotConfigured
	}
	scope, value := "", ""
	if appID != "" {
		scope, value = "application", strings.TrimSpace(appID)
		if env != "" {
			scope, value = "application_env", buildApplicationEnvScopeValue(appID, env)
		}
	}
	allowed, err := a.Release.authz.HasPermission(ctx, user, code, scope, value)
	if err != nil {
		return errors.New("permission check failed")
	}
	if !allowed {
		return errMCPForbidden
	}
	return nil
}

func (a *mcpAPI) visibleOrder(ctx context.Context, id string) (releaseOrderView, error) {
	user, err := mcpUser(ctx)
	if err != nil {
		return releaseOrderView{}, err
	}
	if strings.TrimSpace(id) == "" {
		return releaseOrderView{}, errors.New("order_id is required")
	}
	order, err := a.Release.manager.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return releaseOrderView{}, mcpSafeError(err)
	}
	allowed, err := a.canSeeOrder(ctx, user, order)
	if err != nil {
		return releaseOrderView{}, err
	}
	if !allowed {
		return releaseOrderView{}, errMCPForbidden
	}
	return releaseOrderView{order}, nil
}

// canSeeOrder evaluates access to the exact environment. The shared SQL list
// scope merges env grants into app IDs, so it cannot authorize individual rows.
func (a *mcpAPI) canSeeOrder(ctx context.Context, user userdomain.User, order domain.ReleaseOrder) (bool, error) {
	if user.Role == userdomain.RoleAdmin || mcpIsCreator(user, order) || containsString(order.ApprovalApproverIDs, user.ID) {
		return true, nil
	}
	allowed, err := a.Release.authz.HasPermission(ctx, user, "application.manage", "", "")
	if err != nil {
		return false, errors.New("permission check failed")
	}
	if allowed {
		return true, nil
	}
	for _, code := range []string{"release.view", "release.create", "release.execute", "release.cancel"} {
		allowed, err = a.Release.authz.HasPermission(ctx, user, code, "application_env", buildApplicationEnvScopeValue(order.ApplicationID, order.EnvCode))
		if err != nil {
			return false, errors.New("permission check failed")
		}
		if allowed {
			return true, nil
		}
	}
	return false, nil
}

func (a *mcpAPI) releaseListScope(ctx context.Context) (bool, []string, []domain.ApplicationEnvScope, string, error) {
	user, err := mcpUser(ctx)
	if err != nil {
		return false, nil, nil, "", err
	}
	if user.Role == userdomain.RoleAdmin {
		return true, nil, nil, "", nil
	}
	manage, err := a.Release.authz.HasPermission(ctx, user, "application.manage", "", "")
	if err != nil {
		return false, nil, nil, "", errors.New("permission check failed")
	}
	if manage {
		return true, nil, nil, "", nil
	}
	permissions, err := a.Release.authz.ListEffectivePermissions(ctx, user)
	if err != nil {
		return false, nil, nil, "", errors.New("permission check failed")
	}
	ids, scopes := collectApplicationScopesFromPermissions(permissions, map[string]struct{}{
		"release.view": {}, "release.create": {}, "release.execute": {}, "release.cancel": {},
	})
	// Preserve application grants separately from environment grants in the
	// repository filter. Merging scopes into app IDs would overcount hidden rows.
	return false, ids, scopes, user.ID, nil
}

// mcpSafeError avoids returning raw infrastructure errors (which may contain
// signed URLs, credentials or connection strings) to a tool caller.
func mcpSafeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, usecase.ErrInvalidInput) || errors.Is(err, usecase.ErrInvalidID) || errors.Is(err, usecase.ErrInvalidStatus) {
		return errors.New("invalid input or release order state")
	}
	return errors.New("operation failed; consult server logs with the trace ID")
}

func mcpResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	// JSON conversion makes response boundaries explicit; no domain objects escape.
	data, err := json.Marshal(value)
	if err != nil {
		return nil, nil, errors.New("failed to encode tool response")
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, errors.New("failed to encode tool response")
	}
	return nil, payload, nil
}

func mcpRegister[In any](s *mcp.Server, name, description string, readOnly bool, f func(context.Context, In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
			if _, err := mcpUser(ctx); err != nil {
				return nil, nil, err
			}
			started := time.Now()
			value, err := f(ctx, input)
			status := "success"
			if errors.Is(err, errMCPForbidden) || errors.Is(err, errMCPUnauthorized) {
				status = "denied"
			} else if err != nil {
				status = "error"
			}
			MCPToolCalls.Add(name+"_"+status, 1)
			slog.Info("mcp_tool_call", "trace_id", ctx.Value(mcpTraceKey{}), "tool", name, "status", status, "duration_ms", time.Since(started).Milliseconds())
			return mcpResult(value, err)
		})
}
