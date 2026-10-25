package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gos/internal/application/usecase"
	domain "gos/internal/domain/release"
	userdomain "gos/internal/domain/user"
	"gos/internal/infrastructure/persistence/sqlrepo"

	_ "modernc.org/sqlite"
)

// 测试真实仓储与处理器的作用域交集，确保环境授权不会扩大为全应用授权。
func TestReleaseOrderHandlerEnvironmentVisibilityAcrossReadEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "release-visibility.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE sys_user (
		id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
		email TEXT NOT NULL DEFAULT '', phone TEXT NOT NULL DEFAULT '', role TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active', password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create sys_user: %v", err)
	}
	repo := sqlrepo.NewReleaseRepository(db, "sqlite")
	for _, initSchema := range []func(context.Context) error{
		repo.InitSchema,
		sqlrepo.NewProjectRepository(db, "sqlite").InitSchema,
		sqlrepo.NewApplicationRepository(db, "sqlite").InitSchema,
		sqlrepo.NewArtifactRepositoryConfigRepository(db, "sqlite").InitSchema,
	} {
		if err := initSchema(ctx); err != nil {
			t.Fatalf("init schema: %v", err)
		}
	}
	db.SetMaxOpenConns(1)

	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	orders := []struct {
		id       string
		env      string
		creator  string
		approver string
	}{
		{id: "order-dev", env: "dev", creator: "other"},
		{id: "order-prod", env: "prod", creator: "other"},
		{id: "order-own-prod", env: "prod", creator: "viewer"},
		{id: "order-approve-prod", env: "prod", creator: "other", approver: "viewer"},
	}
	for i, fixture := range orders {
		order := releaseOrderHandlerTestOrder(fixture.id, "RO-"+fixture.id, now.Add(time.Duration(i)*time.Minute))
		order.EnvCode = fixture.env
		order.CreatorUserID = fixture.creator
		if fixture.approver != "" {
			order.ApprovalApproverIDs = []string{fixture.approver}
		}
		if err := repo.Create(ctx, order, nil, nil, nil); err != nil {
			t.Fatalf("create order %s: %v", fixture.id, err)
		}
		if _, err := repo.UpsertArtifactMetadata(ctx, domain.ReleaseOrderArtifactMetadata{
			ID: "artifact-" + fixture.id, ReleaseOrderID: fixture.id,
			PipelineScope: domain.PipelineScopeCI, ArtifactName: fixture.id + ".jar",
			ArtifactType: "jar", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create artifact %s: %v", fixture.id, err)
		}
		if err := repo.CreateApprovalRecord(ctx, domain.ReleaseOrderApprovalRecord{
			ID: "record-" + fixture.id, ReleaseOrderID: fixture.id,
			Action: domain.ReleaseOrderApprovalActionApprove, OperatorUserID: "operator",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create approval record %s: %v", fixture.id, err)
		}
		if i < 2 {
			if err := repo.UpsertAppReleaseState(ctx, domain.AppReleaseState{
				ID: "state-" + fixture.id, ReleaseOrderID: fixture.id, ReleaseOrderNo: order.OrderNo,
				ApplicationID: order.ApplicationID, ApplicationName: order.ApplicationName,
				EnvCode: fixture.env, OperationType: order.OperationType,
				StateStatus: domain.AppReleaseStateStatusActive, IsCurrentLive: true,
				ImageTag: fixture.env + "-image", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create release state %s: %v", fixture.id, err)
			}
		}
	}

	if got := mergeReleaseVisibleApplicationIDs(nil, []domain.ApplicationEnvScope{{ApplicationID: "app-1", EnvCode: "dev"}}); len(got) != 0 {
		t.Fatalf("environment-only grant expanded into full application IDs: %v", got)
	}
	manager := usecase.NewReleaseOrderManager(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	tests := []struct {
		name        string
		user        userdomain.User
		permissions []userdomain.UserPermission
		wantIDs     []string
		wantStates  []string
	}{
		{
			name: "dev grant", user: userdomain.User{ID: "viewer", Role: userdomain.RoleNormal},
			permissions: []userdomain.UserPermission{{PermissionCode: "release.view", ScopeType: "application_env", ScopeValue: "app-1::dev", Enabled: true}},
			wantIDs:     []string{"order-dev", "order-own-prod", "order-approve-prod"},
			wantStates:  []string{"dev"},
		},
		{
			name:    "actor exceptions only",
			user:    userdomain.User{ID: "viewer", Role: userdomain.RoleNormal},
			wantIDs: []string{"order-own-prod", "order-approve-prod"},
		},
		{
			name: "application grant", user: userdomain.User{ID: "viewer", Role: userdomain.RoleNormal},
			permissions: []userdomain.UserPermission{{PermissionCode: "release.view", ScopeType: "application", ScopeValue: "app-1", Enabled: true}},
			wantIDs:     []string{"order-dev", "order-prod", "order-own-prod", "order-approve-prod"},
			wantStates:  []string{"dev", "prod"},
		},
		{
			name: "admin", user: userdomain.User{ID: "viewer", Role: userdomain.RoleAdmin},
			wantIDs:    []string{"order-dev", "order-prod", "order-own-prod", "order-approve-prod"},
			wantStates: []string{"dev", "prod"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { setCurrentUser(c, tt.user); c.Next() })
			NewReleaseOrderHandler(manager, nil, releaseVisibilityAuthorizer{permissions: tt.permissions}, nil).RegisterRoutes(router)
			request := func(path string) *httptest.ResponseRecorder {
				t.Helper()
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				return recorder
			}
			sort.Strings(tt.wantIDs)
			sort.Strings(tt.wantStates)
			for _, path := range []string{"/release-orders", "/release-orders?application_id=app-1", "/release-orders?env_code=prod"} {
				rec := request(path)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
				}
				var resp ReleaseOrderListResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("%s decode: %v", path, err)
				}
				ids := make([]string, 0, len(resp.Data))
				for _, item := range resp.Data {
					ids = append(ids, item.ID)
				}
				sort.Strings(ids)
				want := tt.wantIDs
				if strings.Contains(path, "env_code=prod") {
					want = nil
					for _, fixture := range orders {
						if fixture.env == "prod" && containsString(tt.wantIDs, fixture.id) {
							want = append(want, fixture.id)
						}
					}
					sort.Strings(want)
				}
				if !equalReleaseVisibilityValues(ids, want) || resp.Total != int64(len(want)) {
					t.Fatalf("%s got ids=%v total=%d, want=%v", path, ids, resp.Total, want)
				}
			}
			for _, path := range []string{"/artifacts/release-order-metadata", "/release-approval-records"} {
				rec := request(path)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
				}
				var resp struct {
					Data []struct {
						ReleaseOrderID string `json:"release_order_id"`
					} `json:"data"`
					Total int64 `json:"total"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("%s decode: %v", path, err)
				}
				ids := make([]string, 0, len(resp.Data))
				for _, item := range resp.Data {
					ids = append(ids, item.ReleaseOrderID)
				}
				sort.Strings(ids)
				if !equalReleaseVisibilityValues(ids, tt.wantIDs) || resp.Total != int64(len(tt.wantIDs)) {
					t.Fatalf("%s got ids=%v total=%d, want=%v", path, ids, resp.Total, tt.wantIDs)
				}
			}
			stats := request("/release-orders/stats")
			if stats.Code != http.StatusOK {
				t.Fatalf("stats status=%d body=%s", stats.Code, stats.Body.String())
			}
			var statsResponse ReleaseOrderStatsResponse
			if err := json.Unmarshal(stats.Body.Bytes(), &statsResponse); err != nil {
				t.Fatalf("decode stats: %v", err)
			}
			if statsResponse.Total != int64(len(tt.wantIDs)) {
				t.Fatalf("stats total=%d, want=%d", statsResponse.Total, len(tt.wantIDs))
			}
			for _, path := range []string{"/app-release-states/summaries", "/app-release-states/summaries?application_ids=app-1,app-unknown"} {
				rec := request(path)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
				}
				var resp AppReleaseStateSummaryListResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("%s decode: %v", path, err)
				}
				envs := make([]string, 0, len(resp.Data))
				for _, item := range resp.Data {
					envs = append(envs, item.EnvCode)
				}
				sort.Strings(envs)
				if !equalReleaseVisibilityValues(envs, tt.wantStates) {
					t.Fatalf("%s got envs=%v, want=%v", path, envs, tt.wantStates)
				}
			}
			if tt.user.Role != userdomain.RoleAdmin {
				rec := request("/app-release-states/summaries?application_ids=app-unknown")
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
					t.Fatalf("ungranted state request status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			for _, fixture := range orders {
				rec := request("/release-orders/" + fixture.id)
				wantStatus := http.StatusForbidden
				if containsString(tt.wantIDs, fixture.id) {
					wantStatus = http.StatusOK
				}
				if rec.Code != wantStatus {
					t.Fatalf("detail %s status=%d, want=%d body=%s", fixture.id, rec.Code, wantStatus, rec.Body.String())
				}
			}
		})
	}
}

type releaseVisibilityAuthorizer struct{ permissions []userdomain.UserPermission }

func (a releaseVisibilityAuthorizer) HasPermission(_ context.Context, user userdomain.User, code, scopeType, scopeValue string) (bool, error) {
	if user.Role == userdomain.RoleAdmin {
		return true, nil
	}
	if code == "application.manage" {
		return false, nil
	}
	for _, permission := range a.permissions {
		if !permission.Enabled || permission.PermissionCode != code {
			continue
		}
		if scopeType == "" && scopeValue == "" {
			return true, nil
		}
		if scopeType == "application" && permission.ScopeType == "application_env" {
			appID, _, ok := parseApplicationEnvScopeValue(permission.ScopeValue)
			if ok && appID == scopeValue {
				// 生产授权器在此情形也返回 true；详情仍须按实际环境拒绝 prod。
				return true, nil
			}
		}
		if scopeType == permission.ScopeType && scopeValue == permission.ScopeValue {
			return true, nil
		}
	}
	return false, nil
}

func (a releaseVisibilityAuthorizer) ListEffectivePermissions(context.Context, userdomain.User) ([]userdomain.UserPermission, error) {
	return a.permissions, nil
}

func equalReleaseVisibilityValues(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestNormalizeReleaseOrderErrorMessageStripsInvalidInputPrefix(t *testing.T) {
	err := fmt.Errorf("%w: 重放单不支持再次重放，继续重发请从原始单发起", usecase.ErrInvalidInput)

	got := normalizeReleaseOrderErrorMessage(err)
	want := "重放单不支持再次重放，继续重发请从原始单发起"

	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestDeriveReleaseBusinessStatusUsesActiveExecutionPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     domain.OrderStatus
		executions []domain.ReleaseOrderExecution
		want       domain.ReleaseBusinessStatus
	}{
		{
			name:   "jenkins ci dispatched before build url resolves",
			status: domain.OrderStatusDeploying,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCI,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				QueueURL:      "https://jenkins.example/queue/item/1/",
			}},
			want: domain.ReleaseBusinessStatusBuilding,
		},
		{
			name:   "jenkins ci build started",
			status: domain.OrderStatusDeploying,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCI,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				QueueURL:      "https://jenkins.example/queue/item/1/",
				BuildURL:      "https://jenkins.example/job/ci/12/",
			}},
			want: domain.ReleaseBusinessStatusBuilding,
		},
		{
			name:   "legacy queued order with active ci build",
			status: domain.OrderStatusQueued,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCI,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				BuildURL:      "https://jenkins.example/job/ci/13/",
			}},
			want: domain.ReleaseBusinessStatusBuilding,
		},
		{
			name:   "jenkins cd deployment started",
			status: domain.OrderStatusDeploying,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCD,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				BuildURL:      "https://jenkins.example/job/cd/7/",
			}},
			want: domain.ReleaseBusinessStatusDeploying,
		},
		{
			name:   "jenkins cd dispatched before build url resolves",
			status: domain.OrderStatusDeploying,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCD,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				QueueURL:      "https://jenkins.example/queue/item/7/",
			}},
			want: domain.ReleaseBusinessStatusDeploying,
		},
		{
			name:   "queued order without active execution stays queued",
			status: domain.OrderStatusQueued,
			want:   domain.ReleaseBusinessStatusQueued,
		},
		{
			name:   "settling failed release projects failure instead of the queue it was parked on",
			status: domain.OrderStatusRunning,
			executions: []domain.ReleaseOrderExecution{
				{
					PipelineScope: domain.PipelineScopeCI,
					Provider:      "jenkins",
					Status:        domain.ExecutionStatusFailed,
					BuildURL:      "https://jenkins.example/job/ci/42/",
				},
				{
					PipelineScope: domain.PipelineScopeCD,
					Provider:      "jenkins",
					Status:        domain.ExecutionStatusSkipped,
				},
			},
			want: domain.ReleaseBusinessStatusDeployFailed,
		},
		{
			name:   "settling cancelled release projects cancellation",
			status: domain.OrderStatusRunning,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCI,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusCancelled,
				BuildURL:      "https://jenkins.example/job/ci/43/",
			}},
			want: domain.ReleaseBusinessStatusCancelled,
		},
		{
			name:   "running release whose cd never started keeps the queue projection",
			status: domain.OrderStatusRunning,
			executions: []domain.ReleaseOrderExecution{
				{
					PipelineScope: domain.PipelineScopeCI,
					Provider:      "jenkins",
					Status:        domain.ExecutionStatusFailed,
					BuildURL:      "https://jenkins.example/job/ci/44/",
				},
				{
					PipelineScope: domain.PipelineScopeCD,
					Provider:      "jenkins",
					Status:        domain.ExecutionStatusPending,
				},
			},
			want: domain.ReleaseBusinessStatusQueued,
		},
		{
			name:   "terminal order wins over stale execution",
			status: domain.OrderStatusDeploySuccess,
			executions: []domain.ReleaseOrderExecution{{
				PipelineScope: domain.PipelineScopeCI,
				Provider:      "jenkins",
				Status:        domain.ExecutionStatusRunning,
				BuildURL:      "https://jenkins.example/job/ci/14/",
			}},
			want: domain.ReleaseBusinessStatusDeploySuccess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveReleaseBusinessStatus(tt.status, tt.executions); got != tt.want {
				t.Fatalf("deriveReleaseBusinessStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReleaseOrderApprovalFlowResponseIncludesFrozenGraphSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	instance := domain.ReleaseOrderApprovalFlowInstance{
		ID:               "flow-instance-1",
		ReleaseOrderID:   "order-1",
		FlowDefinitionID: "flow-1",
		FlowName:         "生产发布审批",
		Nodes: []domain.ApprovalFlowNode{{
			Code: "security-review", Name: "安全审批", Gate: domain.ApprovalFlowGateBeforeCD,
			NodeType: domain.ApprovalFlowNodeTypeApproval, ApprovalMode: domain.TemplateApprovalModeAll,
			ApproverIDs: []string{"user-1"}, ApproverNames: []string{"安全负责人"}, PositionX: 320, PositionY: 120,
		}},
		Links: []domain.ApprovalFlowLink{{
			FromCode: "start", ToCode: "security-review",
			ExecutionScopes: []string{string(domain.ApprovalFlowExecutionScopeFullRelease)}, Priority: 1,
		}},
		Status: domain.ApprovalFlowInstanceStatusPendingApproval, CreatedAt: now, UpdatedAt: now,
	}

	tasks := []domain.ReleaseOrderApprovalFlowTask{{
		ID: "approval-task-1", NodeCode: "security-review", NodeName: "安全审批",
		Records: []domain.ReleaseOrderApprovalFlowTaskRecord{{
			ID: "approval-record-1", TaskID: "approval-task-1", Action: domain.ReleaseOrderApprovalActionApprove,
			OperatorUserID: "user-1", OperatorName: "安全负责人", Comment: "审批备注", CreatedAt: now,
		}},
	}}
	response := toReleaseOrderApprovalFlowResponse(instance, tasks)
	if len(response.Nodes) != 1 || response.Nodes[0].Code != "security-review" || response.Nodes[0].ApproverNames[0] != "安全负责人" {
		t.Fatalf("approval flow response nodes = %#v", response.Nodes)
	}
	if len(response.Links) != 1 || response.Links[0].FromCode != "start" || response.Links[0].ToCode != "security-review" {
		t.Fatalf("approval flow response links = %#v", response.Links)
	}
	if len(response.Tasks) != 1 || len(response.Tasks[0].Records) != 1 || response.Tasks[0].Records[0].Comment != "审批备注" {
		t.Fatalf("approval flow response task records = %#v", response.Tasks)
	}
}

func TestReleaseOrderHTTPContractDoesNotExposeDeprecatedSonService(t *testing.T) {
	for _, file := range []string{
		"release_order_handler.go",
		"../../../docs/docs.go",
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile %s failed: %v", file, err)
		}

		text := string(source)
		for _, forbidden := range []string{"SonService", "son_service", `json:"son_service"`} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s should not expose deprecated %q", file, forbidden)
			}
		}
	}
}
