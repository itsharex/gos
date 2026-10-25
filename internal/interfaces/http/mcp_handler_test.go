package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gos/internal/application/usecase"
	domain "gos/internal/domain/release"
	userdomain "gos/internal/domain/user"
	"gos/internal/infrastructure/persistence/sqlrepo"
)

type mcpResolverTest struct {
	users map[string]userdomain.User
	calls int
}

func (r *mcpResolverTest) ResolveMCPToken(_ context.Context, token string) (userdomain.User, error) {
	r.calls++
	user, ok := r.users[token]
	if !ok {
		return userdomain.User{}, errors.New("invalid token")
	}
	return user, nil
}

type mcpTestResponse struct {
	Result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		IsError           bool `json:"isError"`
		StructuredContent any  `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

func mcpTestCall(t *testing.T, h http.Handler, token, method string, arguments any) (int, mcpTestResponse, string) {
	t.Helper()
	params := any(map[string]any{})
	if method == "tools/call" {
		params = arguments
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	var result mcpTestResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode response: %v, body=%s", err, recorder.Body.String())
		}
	}
	return recorder.Code, result, recorder.Body.String()
}
func mcpTestFixture(t *testing.T) (http.Handler, *mcpResolverTest) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE sys_user (
		id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
		email TEXT NOT NULL DEFAULT '', phone TEXT NOT NULL DEFAULT '', role TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active', password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	repo := sqlrepo.NewReleaseRepository(db, "sqlite")
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sqlrepo.NewApplicationRepository(db, "sqlite").InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sqlrepo.NewProjectRepository(db, "sqlite").InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO applications
		(id, name, app_key, repo_url, description, owner, status, artifact_type, language, created_at, updated_at)
		VALUES ('app-1', 'App 1', 'app-1', '', '', '', 'active', 'jar', 'java', ?, ?)`, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, env, creator, remark string }{
		{"order-dev", "dev", "someone", ""},
		{"order-prod", "prod", "someone", "secret-password-should-not-escape"},
		{"order-own", "prod", "reader", "secret-password-should-not-escape"},
		{"order-task", "prod", "someone", ""},
	} {
		order := releaseOrderHandlerTestOrder(item.id, item.id, now)
		order.EnvCode, order.CreatorUserID, order.Remark = item.env, item.creator, item.remark
		if err := repo.Create(ctx, order, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if item.id == "order-dev" {
			if _, err := repo.UpsertArtifactMetadata(ctx, domain.ReleaseOrderArtifactMetadata{
				ID: "artifact-dev", ReleaseOrderID: item.id, ArtifactName: "app.jar", ArtifactURL: "https://u:secret@artifact.invalid/?token=secret", MetadataJSON: `{"password":"secret"}`, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := repo.CreateApprovalFlowInstance(ctx, domain.ReleaseOrderApprovalFlowInstance{
		ID: "instance-task", ReleaseOrderID: "order-task", FlowDefinitionID: "flow-1", FlowName: "review",
		Status: domain.ApprovalFlowInstanceStatusPendingApproval, CurrentTaskID: "task-reader", CurrentNodeCode: "review",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApprovalFlowTask(ctx, domain.ReleaseOrderApprovalFlowTask{
		ID: "task-reader", InstanceID: "instance-task", ReleaseOrderID: "order-task", NodeCode: "review", NodeName: "review",
		NodeType: domain.ApprovalFlowNodeTypeApproval, ApproverIDs: []string{"reader"}, Status: domain.ApprovalFlowTaskStatusPending,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateTemplate(ctx, domain.ReleaseTemplate{
		ID: "tpl-prod", Name: "prod-template", ApplicationID: "app-1", ApplicationName: "App 1",
		Status: domain.TemplateStatusActive, Remark: "secret-password-should-not-escape", CreatedAt: now, UpdatedAt: now,
	}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	manager := usecase.NewReleaseOrderManager(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	permissions := releaseVisibilityAuthorizer{permissions: []userdomain.UserPermission{{PermissionCode: "release.view", ScopeType: "application_env", ScopeValue: "app-1::dev", Enabled: true}}}
	resolver := &mcpResolverTest{users: map[string]userdomain.User{
		"reader-token":   {ID: "reader", Role: userdomain.RoleNormal, Status: userdomain.StatusActive},
		"admin-token":    {ID: "admin", Role: userdomain.RoleAdmin, Status: userdomain.StatusActive},
		"disabled-token": {ID: "disabled", Role: userdomain.RoleAdmin, Status: userdomain.StatusInactive},
	}}
	handler := NewMCPHandler(MCPServices{
		TokenResolver: resolver,
		Release:       NewReleaseOrderHandler(manager, nil, permissions, nil),
		Applications:  &ApplicationHandler{query: usecase.NewQueryApplication(sqlrepo.NewApplicationRepository(db, "sqlite")), authz: permissions},
		Templates:     NewReleaseTemplateHandler(usecase.NewReleaseTemplateManager(repo, nil, nil, nil, nil, nil, nil, nil, nil), permissions),
	})
	return handler, resolver
}
func TestMCPProtocolStatelessJSONAndAuthentication(t *testing.T) {
	h, resolver := mcpTestFixture(t)
	for _, token := range []string{"", "bad", "disabled-token"} {
		status, _, _ := mcpTestCall(t, h, token, "initialize", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("token %q status=%d", token, status)
		}
	}
	status, result, body := mcpTestCall(t, h, "reader-token", "initialize", nil)
	if status != http.StatusOK || len(result.Error) != 0 || !strings.Contains(body, `"protocolVersion"`) {
		t.Fatalf("initialize status=%d response=%s", status, body)
	}
	status, result, body = mcpTestCall(t, h, "reader-token", "tools/list", nil)
	if status != http.StatusOK || len(result.Error) != 0 || len(result.Result.Tools) < 25 {
		t.Fatalf("tools/list status=%d response=%s", status, body)
	}
	for _, name := range []string{"create_release_order", "batch_execute_release_orders", "list_release_stages", "get_release_log", "confirm_release_live", "approve_release_task"} {
		found := false
		for _, tool := range result.Result.Tools {
			if tool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool %q missing", name)
		}
	}
	if resolver.calls != 4 {
		t.Fatalf("expected per-request token resolution; calls=%d", resolver.calls)
	}
	for _, test := range []struct {
		name   string
		path   string
		origin string
		status int
	}{
		{name: "browser origin", path: "/mcp", origin: "https://example.com", status: http.StatusForbidden},
		{name: "query credential", path: "/mcp?access_token=secret", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
			request.Header.Set("Authorization", "Bearer reader-token")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status=%d, want %d", recorder.Code, test.status)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer reader-token")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", recorder.Code)
	}
}
func TestMCPToolsVisibilityRedactionAndWriteDenial(t *testing.T) {
	h, _ := mcpTestFixture(t)
	call := func(token, name string, args map[string]any) mcpTestResponse {
		t.Helper()
		status, result, body := mcpTestCall(t, h, token, "tools/call", map[string]any{"name": name, "arguments": args})
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", name, status, body)
		}
		if strings.Contains(body, "secret-password-should-not-escape") || strings.Contains(body, "artifact.invalid") || strings.Contains(body, `"password":"secret"`) {
			t.Fatalf("%s leaked sensitive output: %s", name, body)
		}
		return result
	}
	result := call("reader-token", "get_release_order", map[string]any{"order_id": "order-dev"})
	if result.Result.IsError || result.Result.StructuredContent == nil {
		t.Fatalf("visible order failed: %+v", result)
	}
	result = call("reader-token", "get_release_approval_flow", map[string]any{"order_id": "order-prod"})
	if !result.Result.IsError {
		t.Fatalf("absent flow revealed hidden order: %+v", result)
	}
	result = call("reader-token", "get_release_order", map[string]any{"order_id": "order-prod"})
	if !result.Result.IsError || !strings.Contains(result.Result.Content[0].Text, "forbidden") {
		t.Fatalf("cross-env read allowed: %+v", result)
	}
	result = call("reader-token", "get_release_order", map[string]any{"order_id": "order-own"})
	if result.Result.IsError {
		t.Fatalf("creator should see own order: %+v", result)
	}
	result = call("reader-token", "list_applications", map[string]any{})
	if result.Result.IsError {
		t.Fatalf("environment grant should discover its application: %+v", result)
	}
	apps, _ := json.Marshal(result.Result.StructuredContent)
	if !strings.Contains(string(apps), "app-1") {
		t.Fatalf("environment-scoped application missing from discovery: %s", apps)
	}
	result = call("reader-token", "get_application", map[string]any{"application_id": "app-1"})
	if result.Result.IsError {
		t.Fatalf("environment-scoped application detail failed: %+v", result)
	}
	result = call("reader-token", "list_release_orders", map[string]any{})
	if result.Result.IsError {
		t.Fatalf("order list failed: %+v", result)
	}
	listed, _ := json.Marshal(result.Result.StructuredContent)
	if strings.Contains(string(listed), "order-prod") || !strings.Contains(string(listed), "order-dev") || !strings.Contains(string(listed), "order-own") {
		t.Fatalf("cross-env order list leaked or excluded owned order: %s", listed)
	}
	result = call("reader-token", "get_release_template", map[string]any{"template_id": "tpl-prod"})
	if !result.Result.IsError {
		t.Fatalf("template accessible without create permission: %+v", result)
	}
	result = call("admin-token", "get_release_template", map[string]any{"template_id": "tpl-prod"})
	if result.Result.IsError {
		t.Fatalf("administrator template read failed: %+v", result)
	}
	result = call("reader-token", "batch_create_release_orders", map[string]any{"orders": []map[string]any{
		{"application_id": "app-1", "env_code": "dev", "template_id": "tpl-prod"},
		{"application_id": "app-1", "env_code": "prod", "template_id": "tpl-prod"},
	}})
	if !result.Result.IsError {
		t.Fatalf("unauthorized batch create accepted: %+v", result)
	}
	result = call("reader-token", "list_release_artifacts", map[string]any{"order_id": "order-dev"})
	if result.Result.IsError {
		t.Fatalf("artifact read failed: %+v", result)
	}
	result = call("reader-token", "get_release_order", map[string]any{"order_id": "order-task"})
	if !result.Result.IsError {
		t.Fatalf("task assignee gained entire order visibility: %+v", result)
	}
	result = call("reader-token", "get_release_approval_flow", map[string]any{"order_id": "order-task"})
	if result.Result.IsError {
		t.Fatalf("assigned flow task should be visible: %+v", result)
	}
	flow, _ := json.Marshal(result.Result.StructuredContent)
	if !strings.Contains(string(flow), "task-reader") || strings.Contains(string(flow), "nodes") || strings.Contains(string(flow), "flow_definition_id") {
		t.Fatalf("task assignee received unrelated approval flow details: %s", flow)
	}
	result = call("reader-token", "approve_release_task", map[string]any{"order_id": "order-task", "task_id": "not-assigned"})
	if !result.Result.IsError {
		t.Fatalf("unassigned task approved: %+v", result)
	}
	result = call("reader-token", "approve_release_task", map[string]any{"order_id": "order-task", "task_id": "task-reader"})
	if result.Result.IsError {
		t.Fatalf("assigned task approval failed: %+v", result)
	}
	result = call("reader-token", "build_release_order", map[string]any{"order_id": "order-dev"})
	if !result.Result.IsError || !strings.Contains(result.Result.Content[0].Text, "forbidden") {
		t.Fatalf("read-only user started build: %+v", result)
	}
	result = call("reader-token", "batch_execute_release_orders", map[string]any{"order_ids": []string{"order-dev", "order-prod"}, "batch_name": "test"})
	if !result.Result.IsError {
		t.Fatalf("batch bypassed authorization: %+v", result)
	}
	result = call("reader-token", "submit_release_approval", map[string]any{"order_id": "order-dev"})
	if !result.Result.IsError {
		t.Fatalf("non-creator submitted approval: %+v", result)
	}
}
