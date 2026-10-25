package httpapi

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gos/internal/application/usecase"
	appdomain "gos/internal/domain/application"
	domain "gos/internal/domain/release"
	userdomain "gos/internal/domain/user"
)

type releaseOrderView struct{ domain.ReleaseOrder }
type mcpIDInput struct {
	OrderID string `json:"order_id" jsonschema:"required"`
}
type mcpAppInput struct {
	ApplicationID string `json:"application_id" jsonschema:"required"`
}
type mcpPageInput struct {
	ApplicationID string `json:"application_id,omitempty"`
	Keyword       string `json:"keyword,omitempty"`
	Status        string `json:"status,omitempty"`
	EnvCode       string `json:"env_code,omitempty"`
	Page          int    `json:"page,omitempty"`
	PageSize      int    `json:"page_size,omitempty"`
}
type mcpPrecheckInput struct {
	OrderID string `json:"order_id" jsonschema:"required"`
	Action  string `json:"action" jsonschema:"required"`
}
type mcpStageLogInput struct {
	OrderID string `json:"order_id" jsonschema:"required"`
	StageID string `json:"stage_id" jsonschema:"required"`
}
type mcpRecoveryInput struct {
	ApplicationID string `json:"application_id" jsonschema:"required"`
	EnvCode       string `json:"env_code" jsonschema:"required"`
	Action        string `json:"action,omitempty"`
}

func mcpPage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}
func mcpOrder(item domain.ReleaseOrder) ReleaseOrderResponse {
	resp := toReleaseOrderResponse(item)
	// Remarks and upstream error text may contain tokens, URLs and arbitrary user input.
	resp.Remark, resp.QueuedReason, resp.RejectedReason = "", "", ""
	resp.HeadChangeTitle, resp.HeadChangeAuthor, resp.HeadChangeURL = "", "", ""
	resp.DeploySnapshots = nil
	return resp
}
func mcpTemplate(item domain.ReleaseTemplate) ReleaseTemplateResponse {
	resp := toReleaseTemplateResponse(item)
	resp.Remark, resp.ComplianceSummary = "", ""
	resp.ComplianceFindings = nil // upstream findings may include arbitrary script text
	return resp
}
func mcpApplication(item appdomain.Application) ApplicationResponse {
	resp := toResponse(item)
	resp.RepoURL, resp.Description, resp.ArtifactDirectory = "", "", ""
	resp.GitOpsBranchMappings = nil
	return resp
}

func (a *mcpAPI) addReadTools(s *mcp.Server) {
	mcpRegister(s, "list_applications", "List applications visible to the caller", true, a.listApplications)
	mcpRegister(s, "get_application", "Get a visible application", true, a.getApplication)
	mcpRegister(s, "list_release_environments", "List configured release environments permitted for an application", true, a.listEnvironments)
	mcpRegister(s, "list_release_templates", "List visible release templates", true, a.listTemplates)
	mcpRegister(s, "get_release_template", "Get a template summary and parameter names without values", true, a.getTemplate)
	mcpRegister(s, "list_release_orders", "List release orders visible to the caller", true, a.listOrders)
	mcpRegister(s, "get_release_order", "Get a visible release order summary", true, a.getOrder)
	mcpRegister(s, "get_release_status", "Get the order status and current live state", true, a.getStatus)
	mcpRegister(s, "list_release_artifacts", "List artifact metadata without URLs or arbitrary metadata", true, a.listArtifacts)
	mcpRegister(s, "list_approval_tasks", "List the caller's pending or handled approval tasks", true, a.listApprovalTasks)
	mcpRegister(s, "get_release_approval_flow", "Get the approval flow of a visible order", true, a.getApprovalFlow)
	mcpRegister(s, "list_release_approval_records", "List approval history of a visible order", true, a.listApprovalRecords)
	mcpRegister(s, "get_release_precheck", "Check build, deploy or execute readiness", true, a.getPrecheck)
	mcpRegister(s, "list_release_stages", "List a visible order's pipeline stages to find stage IDs", true, a.listStages)
	mcpRegister(s, "get_release_log", "Get a bounded, redacted pipeline stage log excerpt", true, a.getLog)
	mcpRegister(s, "get_recovery_precheck", "Check environment recovery readiness", true, a.getRecoveryPrecheck)
}

func (a *mcpAPI) listApplications(ctx context.Context, in mcpPageInput) (any, error) {
	user, _ := mcpUser(ctx)
	allowAll := user.Role == userdomain.RoleAdmin
	if !allowAll {
		var err error
		allowAll, err = a.Release.authz.HasPermission(ctx, user, "application.manage", "", "")
		if err != nil {
			return nil, errors.New("permission check failed")
		}
	}
	ids := []string(nil)
	if !allowAll {
		permissions, err := a.Release.authz.ListEffectivePermissions(ctx, user)
		if err != nil {
			return nil, errors.New("permission check failed")
		}
		ids, scopes := collectApplicationScopesFromPermissions(permissions, map[string]struct{}{"application.view": {}, "release.view": {}, "release.create": {}, "release.execute": {}, "release.cancel": {}})
		ids = releaseCandidateApplicationIDs(ids, scopes)
		if len(ids) == 0 {
			return map[string]any{"data": []ApplicationResponse{}, "total": 0}, nil
		}
	}
	page, size := mcpPage(in.Page, in.PageSize)
	items, total, err := a.Applications.query.List(ctx, appdomain.ListFilter{Keyword: in.Keyword, ApplicationIDs: ids, Page: page, PageSize: size})
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := make([]ApplicationResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, mcpApplication(item))
	}
	return map[string]any{"data": resp, "total": total, "page": page, "page_size": size}, nil
}
func (a *mcpAPI) getApplication(ctx context.Context, in mcpAppInput) (any, error) {
	if strings.TrimSpace(in.ApplicationID) == "" {
		return nil, errors.New("application_id is required")
	}
	item, err := a.Applications.query.GetByID(ctx, in.ApplicationID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	user, _ := mcpUser(ctx)
	if user.Role != userdomain.RoleAdmin {
		manage, err := a.Release.authz.HasPermission(ctx, user, "application.manage", "", "")
		if err != nil {
			return nil, errors.New("permission check failed")
		}
		if !manage {
			visible := false
			for _, code := range []string{"application.view", "release.view", "release.create", "release.execute", "release.cancel"} {
				allowed, err := a.Release.authz.HasPermission(ctx, user, code, "application", item.ID)
				if err != nil {
					return nil, errors.New("permission check failed")
				}
				visible = visible || allowed
			}
			if !visible {
				return nil, errMCPForbidden
			}
		}
	}
	return mcpApplication(item), nil
}
func (a *mcpAPI) listEnvironments(ctx context.Context, in mcpAppInput) (any, error) {
	if a.Settings == nil {
		return nil, errMCPNotConfigured
	}
	if _, err := a.getApplication(ctx, in); err != nil {
		return nil, err
	}
	settings, err := a.Settings.Execute(ctx)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	user, _ := mcpUser(ctx)
	codes := make([]string, 0, len(settings.EnvOptions))
	for _, env := range settings.EnvOptions {
		if user.Role == userdomain.RoleAdmin {
			codes = append(codes, env)
			continue
		}
		allowed := false
		for _, code := range []string{"release.view", "release.create", "release.execute", "release.cancel"} {
			ok, err := a.Release.authz.HasPermission(ctx, user, code, "application_env", buildApplicationEnvScopeValue(in.ApplicationID, env))
			if err != nil {
				return nil, errors.New("permission check failed")
			}
			allowed = allowed || ok
		}
		if allowed {
			codes = append(codes, env)
		}
	}
	return map[string]any{"application_id": in.ApplicationID, "env_codes": codes}, nil
}
func (a *mcpAPI) templateVisible(ctx context.Context, applicationID string) error {
	user, _ := mcpUser(ctx)
	if user.Role == userdomain.RoleAdmin {
		return nil
	}
	if err := a.permission(ctx, "release.template.manage", "", ""); err == nil {
		return nil
	} else if err != errMCPForbidden {
		return err
	}
	return a.permission(ctx, "release.create", applicationID, "")
}
func (a *mcpAPI) listTemplates(ctx context.Context, in mcpPageInput) (any, error) {
	user, _ := mcpUser(ctx)
	allowAll := user.Role == userdomain.RoleAdmin
	if !allowAll {
		var err error
		allowAll, err = a.Release.authz.HasPermission(ctx, user, "release.template.manage", "", "")
		if err != nil {
			return nil, errors.New("permission check failed")
		}
	}
	ids := []string(nil)
	if !allowAll {
		perms, err := a.Release.authz.ListEffectivePermissions(ctx, user)
		if err != nil {
			return nil, errors.New("permission check failed")
		}
		appIDs, scopes := collectApplicationScopesFromPermissions(perms, map[string]struct{}{"release.create": {}})
		ids = mergeReleaseVisibleApplicationIDs(appIDs, scopes)
		if in.ApplicationID != "" {
			if err := a.templateVisible(ctx, in.ApplicationID); err != nil {
				return nil, err
			}
		} else if len(ids) == 0 {
			return map[string]any{"data": []ReleaseTemplateResponse{}, "total": 0}, nil
		}
	}
	page, size := mcpPage(in.Page, in.PageSize)
	items, total, err := a.Templates.manager.List(ctx, usecase.ListReleaseTemplateInput{ApplicationID: in.ApplicationID, ApplicationIDs: ids, Status: domain.TemplateStatus(in.Status), Page: page, PageSize: size})
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := make([]ReleaseTemplateResponse, 0, len(items))
	for _, item := range items {
		if err := a.templateVisible(ctx, item.ApplicationID); err == nil {
			resp = append(resp, mcpTemplate(item))
		} else if err != errMCPForbidden {
			return nil, err
		}
	}
	return map[string]any{"data": resp, "total": total, "page": page, "page_size": size}, nil
}
func (a *mcpAPI) getTemplate(ctx context.Context, in struct {
	TemplateID string `json:"template_id" jsonschema:"required"`
}) (any, error) {
	if strings.TrimSpace(in.TemplateID) == "" {
		return nil, errors.New("template_id is required")
	}
	tpl, bindings, params, _, _, err := a.Templates.manager.GetByID(ctx, in.TemplateID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	if err := a.templateVisible(ctx, tpl.ApplicationID); err != nil {
		return nil, err
	}
	paramNames := make([]map[string]any, 0, len(params))
	for _, p := range params {
		paramNames = append(paramNames, map[string]any{"param_key": p.ParamKey, "pipeline_scope": p.PipelineScope, "required": p.Required})
	}
	bindingViews := make([]map[string]any, 0, len(bindings))
	for _, b := range bindings {
		bindingViews = append(bindingViews, map[string]any{"id": b.ID, "pipeline_scope": b.PipelineScope, "enabled": b.Enabled})
	}
	return map[string]any{"template": mcpTemplate(tpl), "bindings": bindingViews, "params": paramNames}, nil
}
func (a *mcpAPI) listOrders(ctx context.Context, in mcpPageInput) (any, error) {
	allowAll, ids, scopes, userID, err := a.releaseListScope(ctx)
	if err != nil {
		return nil, err
	}
	if !allowAll && len(ids) == 0 && len(scopes) == 0 {
		return map[string]any{"data": []ReleaseOrderResponse{}, "total": 0}, nil
	}
	page, size := mcpPage(in.Page, in.PageSize)
	items, total, err := a.Release.manager.List(ctx, usecase.ListReleaseOrderInput{ApplicationID: in.ApplicationID, ApplicationIDs: ids, VisibleApplicationEnvScopes: scopes, VisibleToUserID: userID, Keyword: in.Keyword, EnvCode: in.EnvCode, Status: domain.OrderStatus(in.Status), Page: page, PageSize: size})
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := make([]ReleaseOrderResponse, 0, len(items))
	for _, item := range items {
		user, _ := mcpUser(ctx)
		ok, err := a.canSeeOrder(ctx, user, item)
		if err != nil {
			return nil, err
		}
		if ok {
			resp = append(resp, mcpOrder(item))
		}
	}
	return map[string]any{"data": resp, "total": total, "page": page, "page_size": size}, nil
}
func (a *mcpAPI) getOrder(ctx context.Context, in mcpIDInput) (any, error) {
	view, err := a.visibleOrder(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	return mcpOrder(view.ReleaseOrder), nil
}
func (a *mcpAPI) getStatus(ctx context.Context, in mcpIDInput) (any, error) {
	view, err := a.visibleOrder(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	resp := mcpOrder(view.ReleaseOrder)
	state, stateErr := a.Release.manager.GetAppReleaseStateByOrderID(ctx, in.OrderID)
	if stateErr == nil {
		resp.LiveStateStatus = string(state.StateStatus)
		resp.LiveStateIsCurrent = state.IsCurrentLive
		resp.LiveStateConfirmedAt = state.ConfirmedAt
		can, err := a.Release.manager.CanConfirmAppReleaseState(ctx, in.OrderID)
		if err == nil {
			resp.LiveStateCanConfirm = can
		}
	}
	return resp, nil
}
func (a *mcpAPI) listArtifacts(ctx context.Context, in mcpIDInput) (any, error) {
	if _, err := a.visibleOrder(ctx, in.OrderID); err != nil {
		return nil, err
	}
	items, err := a.Release.manager.ListArtifactMetadata(ctx, in.OrderID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{"id": item.ID, "artifact_name": item.ArtifactName, "artifact_type": item.ArtifactType, "artifact_version": item.ArtifactVersion, "pipeline_scope": item.PipelineScope, "checksum": item.Checksum, "size_bytes": item.SizeBytes, "build_number": item.BuildNumber})
	}
	return out, nil
}
func (a *mcpAPI) listApprovalTasks(ctx context.Context, in struct {
	View     string `json:"view,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}) (any, error) {
	user, _ := mcpUser(ctx)
	page, size := mcpPage(in.Page, in.PageSize)
	filter := usecase.ListApprovalWorkbenchInput{UserID: user.ID, Page: page, PageSize: size}
	if in.View == "" || in.View == "pending" {
		items, total, err := a.Release.manager.ListApprovalWorkbenchTasks(ctx, filter)
		if err != nil {
			return nil, mcpSafeError(err)
		}
		resp := make([]ReleaseApprovalWorkbenchTaskResponse, 0, len(items))
		for _, item := range items {
			resp = append(resp, toReleaseApprovalWorkbenchTaskResponse(item))
		}
		return map[string]any{"data": resp, "total": total, "page": page, "page_size": size}, nil
	}
	if in.View != "handled" {
		return nil, errors.New("view must be pending or handled")
	}
	items, total, err := a.Release.manager.ListApprovalWorkbenchRecords(ctx, filter)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := make([]ReleaseApprovalWorkbenchRecordResponse, 0, len(items))
	for _, item := range items {
		record := toReleaseApprovalWorkbenchRecordResponse(item)
		record.Comment = ""
		resp = append(resp, record)
	}
	return map[string]any{"data": resp, "total": total, "page": page, "page_size": size}, nil
}
func (a *mcpAPI) getApprovalFlow(ctx context.Context, in mcpIDInput) (any, error) {
	order, err := a.Release.manager.GetByID(ctx, in.OrderID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	instance, tasks, err := a.Release.manager.GetApprovalFlowInstance(ctx, order.ID)
	if errors.Is(err, domain.ErrApprovalFlowInstanceNotFound) {
		user, _ := mcpUser(ctx)
		visible, authErr := a.canSeeOrder(ctx, user, order)
		if authErr != nil {
			return nil, authErr
		}
		if !visible {
			return nil, errMCPForbidden
		}
		return map[string]any{"data": nil}, nil
	}
	if err != nil {
		return nil, mcpSafeError(err)
	}
	user, _ := mcpUser(ctx)
	visible, err := a.canSeeOrder(ctx, user, order)
	if err != nil {
		return nil, err
	}
	visibleOrder := visible
	if !visible {
		for _, task := range tasks {
			if containsString(task.ApproverIDs, user.ID) {
				visible = true
				break
			}
		}
	}
	if !visible {
		return nil, errMCPForbidden
	}
	if !visibleOrder {
		assigned := make([]map[string]any, 0, 1)
		for _, task := range tasks {
			if containsString(task.ApproverIDs, user.ID) {
				assigned = append(assigned, map[string]any{
					"task_id": task.ID, "node_name": task.NodeName, "gate": task.Gate,
					"status": task.Status, "created_at": task.CreatedAt,
				})
			}
		}
		return map[string]any{"order_id": order.ID, "status": instance.Status, "assigned_tasks": assigned}, nil
	}
	resp := toReleaseOrderApprovalFlowResponse(instance, tasks)
	for i := range resp.Tasks {
		resp.Tasks[i].Message = ""
		for j := range resp.Tasks[i].Records {
			resp.Tasks[i].Records[j].Comment = ""
		}
	}
	return resp, nil
}
func (a *mcpAPI) listApprovalRecords(ctx context.Context, in mcpIDInput) (any, error) {
	if _, err := a.visibleOrder(ctx, in.OrderID); err != nil {
		return nil, err
	}
	items, err := a.Release.manager.ListApprovalRecords(ctx, in.OrderID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := make([]ReleaseOrderApprovalRecordResponse, 0, len(items))
	for _, item := range items {
		record := toReleaseOrderApprovalRecordResponse(item)
		record.Comment = ""
		resp = append(resp, record)
	}
	return resp, nil
}
func (a *mcpAPI) getPrecheck(ctx context.Context, in mcpPrecheckInput) (any, error) {
	if _, err := a.visibleOrder(ctx, in.OrderID); err != nil {
		return nil, err
	}
	var result usecase.ReleaseOrderPrecheckOutput
	var err error
	switch strings.ToLower(in.Action) {
	case "build":
		result, err = a.Release.manager.PrecheckBuild(ctx, in.OrderID)
	case "deploy":
		result, err = a.Release.manager.PrecheckDeploy(ctx, in.OrderID)
	case "execute":
		result, err = a.Release.manager.PrecheckExecute(ctx, in.OrderID)
	default:
		return nil, errors.New("action must be build, deploy or execute")
	}
	if err != nil {
		return nil, mcpSafeError(err)
	}
	for i := range result.Items {
		result.Items[i].Message = ""
	}
	result.ConflictMessage, result.ConflictOrderID, result.ConflictOrderNo = "", "", ""
	result.LockKey = ""
	return result, nil
}
func (a *mcpAPI) listStages(ctx context.Context, in mcpIDInput) (any, error) {
	if _, err := a.visibleOrder(ctx, in.OrderID); err != nil {
		return nil, err
	}
	view, err := a.Release.manager.ListStoredPipelineStagesView(ctx, in.OrderID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	stages := make([]map[string]any, 0, len(view.Stages))
	for _, stage := range view.Stages {
		stages = append(stages, map[string]any{
			"id": stage.ID, "pipeline_scope": stage.PipelineScope,
			"status": stage.Status, "sort_no": stage.SortNo,
			"started_at": stage.StartedAt, "finished_at": stage.FinishedAt,
		})
	}
	return stages, nil
}

func (a *mcpAPI) getLog(ctx context.Context, in mcpStageLogInput) (any, error) {
	if _, err := a.visibleOrder(ctx, in.OrderID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.StageID) == "" {
		return nil, errors.New("stage_id is required")
	}
	stage, stageLog, err := a.Release.manager.GetPipelineStageLog(ctx, in.OrderID, in.StageID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	content, truncated := mcpLogExcerpt(stageLog.Content)
	resp := toReleaseOrderPipelineStageResponse(stage)
	resp.RawStatus, resp.StageName = "", ""
	return map[string]any{"stage": resp, "content": content, "truncated": truncated, "has_more": stageLog.HasMore}, nil
}
func (a *mcpAPI) getRecoveryPrecheck(ctx context.Context, in mcpRecoveryInput) (any, error) {
	if err := a.permission(ctx, "release.create", in.ApplicationID, in.EnvCode); err != nil {
		return nil, err
	}
	output, err := a.Release.manager.GetApplicationRollbackPrecheck(ctx, in.ApplicationID, in.EnvCode, usecase.RollbackSupportedAction(strings.ToLower(strings.TrimSpace(in.Action))))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := toApplicationRollbackPrecheckResponse(output)
	resp.Params = nil
	resp.Reason, resp.ConflictMessage, resp.LockKey, resp.ConflictOrderNo = "", "", "", ""
	for i := range resp.Items {
		resp.Items[i].Message = ""
	}
	return resp, nil
}
