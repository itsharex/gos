package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	userdomain "gos/internal/domain/user"
)

const (
	MCPTokenPrefix      = "gos_mcp_"
	DefaultMCPTokenDays = 90
	MaxMCPTokenDays     = 365
)

// MCPTokenOperations exposes low-cardinality lifecycle counters through expvar.
// The server may mount expvar.Handler() on a protected metrics route.
var MCPTokenOperations = expvar.NewMap("gos_mcp_token_operations_total")

var mcpTokenLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// MCPTokenManager keeps MCP credentials independent of replace-on-login sessions.
type MCPTokenManager struct {
	tokens userdomain.MCPTokenRepository
	users  interface {
		GetUserByID(context.Context, string) (userdomain.User, error)
	}
	now func() time.Time
}

type CreateMCPTokenInput struct {
	Label         string `json:"label"`
	ExpiresInDays int    `json:"expires_in_days"`
}

type IssuedMCPToken struct {
	Token string
	Item  userdomain.MCPToken
}

func NewMCPTokenManager(tokens userdomain.MCPTokenRepository, users interface {
	GetUserByID(context.Context, string) (userdomain.User, error)
}) *MCPTokenManager {
	return &MCPTokenManager{tokens: tokens, users: users, now: func() time.Time { return time.Now().UTC() }}
}

func (m *MCPTokenManager) Create(ctx context.Context, userID string, input CreateMCPTokenInput) (IssuedMCPToken, error) {
	label := strings.TrimSpace(input.Label)
	if label == "" || utf8.RuneCountInString(label) > 100 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return IssuedMCPToken{}, fmt.Errorf("%w: label must have 1-100 printable characters", ErrInvalidInput)
	}
	days := input.ExpiresInDays
	if days == 0 {
		days = DefaultMCPTokenDays
	}
	if days < 1 || days > MaxMCPTokenDays {
		return IssuedMCPToken{}, fmt.Errorf("%w: expires_in_days must be 1-%d", ErrInvalidInput, MaxMCPTokenDays)
	}
	owner, err := m.activeUser(ctx, userID)
	if err != nil {
		return IssuedMCPToken{}, err
	}

	// Read all 32 bytes from the OS CSPRNG. The plaintext is returned exactly once.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		mcpTokenAudit(ctx, "create", "error", owner.ID, "", err)
		return IssuedMCPToken{}, err
	}
	raw := MCPTokenPrefix + hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(raw))
	now := m.now().UTC()
	item := userdomain.MCPToken{
		ID:          generateID("mcp"),
		UserID:      owner.ID,
		Label:       label,
		TokenPrefix: raw[:len(MCPTokenPrefix)+8],
		TokenHash:   hex.EncodeToString(hash[:]),
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Duration(days) * 24 * time.Hour),
	}
	if err := m.tokens.CreateMCPToken(ctx, item); err != nil {
		mcpTokenAudit(ctx, "create", "error", owner.ID, item.ID, err)
		return IssuedMCPToken{}, err
	}
	MCPTokenOperations.Add("created", 1)
	mcpTokenAudit(ctx, "create", "success", owner.ID, item.ID, nil)
	// Do not expose the hash in either the response or any later read.
	item.TokenHash = ""
	return IssuedMCPToken{Token: raw, Item: item}, nil
}

func (m *MCPTokenManager) List(ctx context.Context, userID string) ([]userdomain.MCPToken, error) {
	owner, err := m.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	items, err := m.tokens.ListMCPTokensByUser(ctx, owner.ID)
	if err != nil {
		mcpTokenAudit(ctx, "list", "error", owner.ID, "", err)
		return nil, err
	}
	return items, nil
}

func (m *MCPTokenManager) Revoke(ctx context.Context, userID, tokenID string) error {
	if strings.TrimSpace(tokenID) == "" {
		return ErrInvalidID
	}
	owner, err := m.activeUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := m.tokens.RevokeMCPToken(ctx, owner.ID, strings.TrimSpace(tokenID), m.now().UTC()); err != nil {
		if errors.Is(err, userdomain.ErrMCPTokenNotFound) {
			MCPTokenOperations.Add("revoke_missing", 1)
			mcpTokenAudit(ctx, "revoke", "denied", owner.ID, "", nil)
		} else {
			mcpTokenAudit(ctx, "revoke", "error", owner.ID, tokenID, err)
		}
		return err
	}
	MCPTokenOperations.Add("revoked", 1)
	mcpTokenAudit(ctx, "revoke", "success", owner.ID, tokenID, nil)
	return nil
}

// ResolveMCPToken satisfies the MCP endpoint's dedicated token resolver.
func (m *MCPTokenManager) ResolveMCPToken(ctx context.Context, raw string) (userdomain.User, error) {
	return m.ResolveUserByToken(ctx, raw)
}

// ResolveUserByToken rechecks the token and its owner on every call. Never
// cache this result: revocation, user deletion and deactivation must take effect
// immediately without a login-session lookup.
func (m *MCPTokenManager) ResolveUserByToken(ctx context.Context, raw string) (userdomain.User, error) {
	if !validMCPToken(raw) {
		MCPTokenOperations.Add("resolve_denied", 1)
		mcpTokenAudit(ctx, "resolve", "denied", "", "", nil)
		return userdomain.User{}, userdomain.ErrMCPTokenNotFound
	}
	hash := sha256.Sum256([]byte(raw))
	item, err := m.tokens.GetMCPTokenByHash(ctx, hex.EncodeToString(hash[:]))
	if err != nil {
		if errors.Is(err, userdomain.ErrMCPTokenNotFound) {
			MCPTokenOperations.Add("resolve_denied", 1)
			mcpTokenAudit(ctx, "resolve", "denied", "", "", nil)
			return userdomain.User{}, userdomain.ErrMCPTokenNotFound
		}
		mcpTokenAudit(ctx, "resolve", "error", "", "", err)
		return userdomain.User{}, err
	}
	if item.RevokedAt != nil || !item.ExpiresAt.After(m.now().UTC()) {
		MCPTokenOperations.Add("resolve_denied", 1)
		mcpTokenAudit(ctx, "resolve", "denied", item.UserID, item.ID, nil)
		return userdomain.User{}, userdomain.ErrMCPTokenNotFound
	}
	owner, err := m.activeUser(ctx, item.UserID)
	if err != nil {
		if errors.Is(err, userdomain.ErrUserNotFound) || errors.Is(err, userdomain.ErrMCPTokenNotFound) {
			MCPTokenOperations.Add("resolve_denied", 1)
			mcpTokenAudit(ctx, "resolve", "denied", item.UserID, item.ID, nil)
			return userdomain.User{}, userdomain.ErrMCPTokenNotFound
		}
		mcpTokenAudit(ctx, "resolve", "error", item.UserID, item.ID, err)
		return userdomain.User{}, err
	}
	MCPTokenOperations.Add("resolve_success", 1)
	return owner, nil
}

func (m *MCPTokenManager) activeUser(ctx context.Context, userID string) (userdomain.User, error) {
	if strings.TrimSpace(userID) == "" {
		return userdomain.User{}, userdomain.ErrMCPTokenNotFound
	}
	owner, err := m.users.GetUserByID(ctx, strings.TrimSpace(userID))
	if err != nil {
		return userdomain.User{}, err
	}
	if owner.Status != userdomain.StatusActive {
		return userdomain.User{}, userdomain.ErrMCPTokenNotFound
	}
	return owner, nil
}

func validMCPToken(raw string) bool {
	if len(raw) != len(MCPTokenPrefix)+64 || !strings.HasPrefix(raw, MCPTokenPrefix) {
		return false
	}
	for _, ch := range raw[len(MCPTokenPrefix):] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// mcpTokenAudit never receives token plaintext or token hashes. A trace ID is
// included even when no upstream tracing middleware has been installed yet.
func mcpTokenAudit(ctx context.Context, action, result, userID, tokenID string, err error) {
	attrs := []slog.Attr{
		slog.String("component", "mcp_token"),
		slog.String("action", action),
		slog.String("result", result),
		slog.String("trace_id", mcpTokenTraceID(ctx)),
		slog.String("user_id", userID),
		slog.String("token_id", tokenID),
	}
	if err != nil {
		// Driver errors can include bound values or connection details. Keep the
		// audit event useful without logging token hashes or infrastructure secrets.
		attrs = append(attrs, slog.String("error_type", fmt.Sprintf("%T", err)))
		mcpTokenLogger.LogAttrs(ctx, slog.LevelError, "mcp_token_operation", attrs...)
		return
	}
	mcpTokenLogger.LogAttrs(ctx, slog.LevelInfo, "mcp_token_operation", attrs...)
}

type mcpTokenTraceContextKey struct{}

// WithMCPTokenTraceID lets an HTTP adapter attach an existing request trace ID.
func WithMCPTokenTraceID(ctx context.Context, traceID string) context.Context {
	if len(traceID) != 32 {
		return ctx
	}
	for _, ch := range traceID {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ctx
		}
	}
	return context.WithValue(ctx, mcpTokenTraceContextKey{}, traceID)
}

func mcpTokenTraceID(ctx context.Context) string {
	if value, ok := ctx.Value(mcpTokenTraceContextKey{}).(string); ok {
		return value
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	return "unavailable"
}
