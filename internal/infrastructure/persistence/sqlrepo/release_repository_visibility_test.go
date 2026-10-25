package sqlrepo

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	releasedomain "gos/internal/domain/release"
)

func TestReleaseRepositoryVisibilityKeepsEnvironmentAndActorExceptions(t *testing.T) {
	repo := newTestReleaseRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	orders := []struct {
		id, env, creator, approver string
	}{
		{id: "dev", env: "dev", creator: "other"},
		{id: "prod", env: "prod", creator: "other"},
		{id: "own-prod", env: "prod", creator: "viewer"},
		{id: "approve-prod", env: "prod", creator: "other", approver: "viewer"},
	}
	for i, fixture := range orders {
		order := newTestReleaseOrder(fixture.id, "RO-"+fixture.id, "app-1", fixture.env, releasedomain.OrderStatusSuccess, now.Add(time.Duration(i)*time.Minute))
		order.CreatorUserID = fixture.creator
		if fixture.approver != "" {
			order.ApprovalApproverIDs = []string{fixture.approver}
		}
		if err := repo.Create(ctx, order, nil, nil, nil); err != nil {
			t.Fatalf("create %s: %v", fixture.id, err)
		}
		if _, err := repo.UpsertArtifactMetadata(ctx, releasedomain.ReleaseOrderArtifactMetadata{
			ID: "artifact-" + fixture.id, ReleaseOrderID: fixture.id, PipelineScope: releasedomain.PipelineScopeCI,
			ArtifactName: fixture.id + ".jar", ArtifactType: "jar", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("artifact %s: %v", fixture.id, err)
		}
		if err := repo.CreateApprovalRecord(ctx, releasedomain.ReleaseOrderApprovalRecord{
			ID: "approval-" + fixture.id, ReleaseOrderID: fixture.id,
			Action: releasedomain.ReleaseOrderApprovalActionApprove, OperatorUserID: "operator", CreatedAt: now,
		}); err != nil {
			t.Fatalf("approval %s: %v", fixture.id, err)
		}
	}

	scopes := []releasedomain.ApplicationEnvScope{{ApplicationID: "app-1", EnvCode: "dev"}}
	checks := []struct {
		name   string
		appIDs []string
		viewer string
		want   []string
	}{
		{name: "dev only", viewer: "viewer", want: []string{"dev", "own-prod", "approve-prod"}},
		{name: "full application", appIDs: []string{"app-1"}, viewer: "viewer", want: []string{"dev", "prod", "own-prod", "approve-prod"}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			filter := releasedomain.ListFilter{ApplicationIDs: tc.appIDs, VisibleApplicationEnvScopes: scopes, VisibleToUserID: tc.viewer, Page: 1, PageSize: 20}
			items, total, err := repo.List(ctx, filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			ids := make([]string, 0, len(items))
			for _, item := range items {
				ids = append(ids, item.ID)
			}
			sort.Strings(ids)
			sort.Strings(tc.want)
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") || total != int64(len(tc.want)) {
				t.Fatalf("list ids=%v total=%d, want=%v", ids, total, tc.want)
			}
			stats, err := repo.ListStats(ctx, filter)
			if err != nil || stats.Total != int64(len(tc.want)) {
				t.Fatalf("stats=%+v err=%v, want=%d", stats, err, len(tc.want))
			}
			artifacts, artifactTotal, err := repo.ListArtifactMetadataSummaries(ctx, releasedomain.ArtifactMetadataListFilter{
				ApplicationIDs: tc.appIDs, VisibleApplicationEnvScopes: scopes, VisibleToUserID: tc.viewer, Page: 1, PageSize: 20,
			})
			if err != nil || artifactTotal != int64(len(tc.want)) {
				t.Fatalf("artifacts=%d total=%d err=%v", len(artifacts), artifactTotal, err)
			}
			artifactIDs := make([]string, 0, len(artifacts))
			for _, artifact := range artifacts {
				artifactIDs = append(artifactIDs, artifact.Artifact.ReleaseOrderID)
			}
			sort.Strings(artifactIDs)
			if strings.Join(artifactIDs, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("artifact order IDs=%v, want=%v", artifactIDs, tc.want)
			}
			approvals, approvalTotal, err := repo.ListApprovalRecordSummaries(ctx, releasedomain.ApprovalRecordListFilter{
				ApplicationIDs: tc.appIDs, VisibleApplicationEnvScopes: scopes, VisibleToUserID: tc.viewer, Page: 1, PageSize: 20,
			})
			if err != nil || approvalTotal != int64(len(tc.want)) {
				t.Fatalf("approvals=%d total=%d err=%v", len(approvals), approvalTotal, err)
			}
			approvalIDs := make([]string, 0, len(approvals))
			for _, approval := range approvals {
				approvalIDs = append(approvalIDs, approval.ReleaseOrderID)
			}
			sort.Strings(approvalIDs)
			if strings.Join(approvalIDs, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("approval order IDs=%v, want=%v", approvalIDs, tc.want)
			}
		})
	}
}

// TestBuildReleaseOrderVisibilityClauseWithEnvScopes 组装业务执行所需的输入数据。
func TestBuildReleaseOrderVisibilityClauseWithEnvScopes(t *testing.T) {
	clause, args := buildReleaseOrderVisibilityClauseWithAlias(
		"ro",
		[]string{"app-global"},
		[]releasedomain.ApplicationEnvScope{{ApplicationID: "app-env", EnvCode: "prod"}},
		"user-1",
	)

	if !strings.Contains(clause, "ro.application_id IN") {
		t.Fatalf("expected application visibility clause, got %s", clause)
	}
	if !strings.Contains(clause, "ro.application_id = ? AND ro.env_code = ?") {
		t.Fatalf("expected env visibility clause, got %s", clause)
	}
	if !strings.Contains(clause, "ro.creator_user_id = ?") {
		t.Fatalf("expected creator visibility clause, got %s", clause)
	}
	if len(args) != 5 {
		t.Fatalf("expected 5 query args, got %d", len(args))
	}
}
