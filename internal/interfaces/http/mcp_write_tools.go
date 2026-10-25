package httpapi

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gos/internal/application/usecase"
	domain "gos/internal/domain/release"
	userdomain "gos/internal/domain/user"
)

type mcpCreateInput struct {
	ApplicationID string                           `json:"application_id" jsonschema:"required"`
	TemplateID    string                           `json:"template_id" jsonschema:"required"`
	ReleaseName   string                           `json:"release_name,omitempty"`
	EnvCode       string                           `json:"env_code" jsonschema:"required"`
	GitRef        string                           `json:"git_ref,omitempty"`
	ImageTag      string                           `json:"image_tag,omitempty"`
	TriggerType   string                           `json:"trigger_type,omitempty"`
	Remark        string                           `json:"remark,omitempty"`
	Params        []CreateReleaseOrderParamRequest `json:"params,omitempty"`
	Steps         []CreateReleaseOrderStepRequest  `json:"steps,omitempty"`
}
type mcpBatchCreateInput struct {
	Orders []mcpCreateInput `json:"orders" jsonschema:"required"`
}
type mcpUpdateInput struct {
	OrderID string         `json:"order_id" jsonschema:"required"`
	Order   mcpCreateInput `json:"order" jsonschema:"required"`
}
type mcpActionInput struct {
	OrderID string `json:"order_id" jsonschema:"required"`
	Comment string `json:"comment,omitempty"`
}
type mcpTaskActionInput struct {
	OrderID string `json:"order_id" jsonschema:"required"`
	TaskID  string `json:"task_id" jsonschema:"required"`
	Comment string `json:"comment,omitempty"`
}
type mcpBatchExecuteInput struct {
	OrderIDs           []string `json:"order_ids" jsonschema:"required"`
	BatchName          string   `json:"batch_name" jsonschema:"required"`
	StagedDispatchMode string   `json:"staged_dispatch_mode,omitempty"`
}
type mcpReplayInput struct {
	OrderID          string                           `json:"order_id" jsonschema:"required"`
	OverrideCDParams bool                             `json:"override_cd_params,omitempty"`
	CDParams         []CreateReleaseOrderParamRequest `json:"cd_params,omitempty"`
}

func (a *mcpAPI) addWriteTools(s *mcp.Server) {
	mcpRegister(s, "create_release_order", "Create a release order; caller identity is taken from the MCP token", false, a.createOrder)
	mcpRegister(s, "batch_create_release_orders", "Create up to 50 independent release orders after authorizing all inputs", false, a.batchCreateOrders)
	mcpRegister(s, "update_release_order", "Update an editable release order owned by the caller", false, a.updateOrder)
	mcpRegister(s, "submit_release_approval", "Submit a release order for approval as its creator", false, a.submitApproval)
	mcpRegister(s, "approve_release_order", "Approve a legacy release order as an assigned approver", false, a.approveOrder)
	mcpRegister(s, "reject_release_order", "Reject a legacy release order as an assigned approver", false, a.rejectOrder)
	mcpRegister(s, "approve_release_task", "Approve an assigned approval flow task", false, a.approveTask)
	mcpRegister(s, "reject_release_task", "Reject an assigned approval flow task", false, a.rejectTask)
	mcpRegister(s, "build_release_order", "Start CI build with release.execute permission", false, a.buildOrder)
	mcpRegister(s, "deploy_release_order", "Start deployment with release.execute permission", false, a.deployOrder)
	mcpRegister(s, "execute_release_order", "Execute the release order with release.execute permission", false, a.executeOrder)
	mcpRegister(s, "batch_execute_release_orders", "Execute a batch after checking each order's visibility and execute permission", false, a.batchExecuteOrders)
	mcpRegister(s, "cancel_release_order", "Cancel a release order as its creator or administrator", false, a.cancelOrder)
	mcpRegister(s, "create_release_rollback", "Create a rollback order from a visible source order", false, a.createRollback)
	mcpRegister(s, "create_release_replay", "Create a pipeline replay from a visible source order", false, a.createReplay)
	mcpRegister(s, "create_application_recovery", "Create a recovery order after environment precheck", false, a.createRecovery)
	mcpRegister(s, "confirm_release_live", "Confirm the deployed release as live", false, a.confirmLive)
}
func (a *mcpAPI) createInput(in mcpCreateInput, user userdomain.User) usecase.CreateReleaseOrderInput {
	return buildReleaseOrderInput(CreateReleaseOrderRequest{
		ApplicationID: in.ApplicationID, TemplateID: in.TemplateID, ReleaseName: in.ReleaseName,
		EnvCode: in.EnvCode, GitRef: in.GitRef, ImageTag: in.ImageTag,
		TriggerType: in.TriggerType, Remark: in.Remark, Params: in.Params, Steps: in.Steps,
	}, user)
}
func (a *mcpAPI) validateCreate(ctx context.Context, in mcpCreateInput) error {
	if strings.TrimSpace(in.ApplicationID) == "" || strings.TrimSpace(in.EnvCode) == "" || strings.TrimSpace(in.TemplateID) == "" {
		return errors.New("application_id, env_code and template_id are required")
	}
	if err := a.permission(ctx, "release.create", in.ApplicationID, in.EnvCode); err != nil {
		return err
	}
	// The template must belong to the target application even if the usecase
	// already checks the binding; this keeps authorization ahead of mutation.
	tpl, _, _, _, _, err := a.Templates.manager.GetByID(ctx, in.TemplateID)
	if err != nil {
		return mcpSafeError(err)
	}
	if tpl.ApplicationID != strings.TrimSpace(in.ApplicationID) {
		return errMCPForbidden
	}
	return nil
}
func (a *mcpAPI) createOrder(ctx context.Context, in mcpCreateInput) (any, error) {
	if err := a.validateCreate(ctx, in); err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	order, err := a.Release.manager.Create(ctx, a.createInput(in, user))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(order), nil
}
func (a *mcpAPI) batchCreateOrders(ctx context.Context, in mcpBatchCreateInput) (any, error) {
	if len(in.Orders) < 1 || len(in.Orders) > 50 {
		return nil, errors.New("orders must contain between 1 and 50 items")
	}
	for _, item := range in.Orders {
		if err := a.validateCreate(ctx, item); err != nil {
			return nil, err
		}
	}
	user, _ := mcpUser(ctx)
	orders := make([]ReleaseOrderResponse, 0, len(in.Orders))
	failures := make([]map[string]any, 0)
	for i, item := range in.Orders {
		created, err := a.Release.manager.Create(ctx, a.createInput(item, user))
		if err != nil {
			failures = append(failures, map[string]any{"index": i, "error": mcpSafeError(err).Error()})
			continue
		}
		orders = append(orders, mcpOrder(created))
	}
	return map[string]any{"orders": orders, "failures": failures}, nil
}
func (a *mcpAPI) existingForWrite(ctx context.Context, id, code string) (domain.ReleaseOrder, error) {
	view, err := a.visibleOrder(ctx, id)
	if err != nil {
		return domain.ReleaseOrder{}, err
	}
	if code != "" {
		if err := a.permission(ctx, code, view.ApplicationID, view.EnvCode); err != nil {
			return domain.ReleaseOrder{}, err
		}
	}
	return view.ReleaseOrder, nil
}
func mcpIsCreator(user userdomain.User, order domain.ReleaseOrder) bool {
	return strings.TrimSpace(user.ID) != "" && user.ID == order.CreatorUserID
}
func mcpIsApprover(user userdomain.User, order domain.ReleaseOrder) bool {
	return user.Role == userdomain.RoleAdmin || containsString(order.ApprovalApproverIDs, user.ID)
}
func (a *mcpAPI) updateOrder(ctx context.Context, in mcpUpdateInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	if user.Role != userdomain.RoleAdmin && !mcpIsCreator(user, order) {
		return nil, errMCPForbidden
	}
	// Update cannot silently change the application or environment without a
	// second scoped authorization check; existing REST update omits that check.
	if err := a.validateCreate(ctx, in.Order); err != nil {
		return nil, err
	}
	if order.ApplicationID != strings.TrimSpace(in.Order.ApplicationID) || order.EnvCode != strings.TrimSpace(in.Order.EnvCode) {
		return nil, errors.New("application_id and env_code cannot change during update")
	}
	updated, err := a.Release.manager.Update(ctx, order.ID, a.createInput(in.Order, user))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) submitApproval(ctx context.Context, in mcpActionInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	if !mcpIsCreator(user, order) {
		return nil, errMCPForbidden
	}
	updated, err := a.Release.manager.SubmitApproval(ctx, order.ID, user.ID, resolveTriggeredBy(user), in.Comment)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) legacyApproval(ctx context.Context, in mcpActionInput, approve bool) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	if !mcpIsApprover(user, order) {
		return nil, errMCPForbidden
	}
	var updated domain.ReleaseOrder
	if approve {
		updated, err = a.Release.manager.Approve(ctx, order.ID, user.ID, resolveTriggeredBy(user), in.Comment)
	} else {
		updated, err = a.Release.manager.Reject(ctx, order.ID, user.ID, resolveTriggeredBy(user), in.Comment)
	}
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) approveOrder(ctx context.Context, in mcpActionInput) (any, error) {
	return a.legacyApproval(ctx, in, true)
}
func (a *mcpAPI) rejectOrder(ctx context.Context, in mcpActionInput) (any, error) {
	return a.legacyApproval(ctx, in, false)
}
func (a *mcpAPI) approvalTask(ctx context.Context, in mcpTaskActionInput, approve bool) (any, error) {
	// The task assignee may differ from the order's legacy approver list.
	// Load only the order ID, then authorize against this flow's task mapping.
	if strings.TrimSpace(in.OrderID) == "" || strings.TrimSpace(in.TaskID) == "" { return nil, errors.New("order_id and task_id are required") }
	user, _ := mcpUser(ctx)
	order, err := a.Release.manager.GetByID(ctx, in.OrderID)
	if err != nil { return nil, mcpSafeError(err) }
	_, tasks, err := a.Release.manager.GetApprovalFlowInstance(ctx, order.ID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	assigned := false
	for _, task := range tasks {
		if task.ID == strings.TrimSpace(in.TaskID) && containsString(task.ApproverIDs, user.ID) {
			assigned = true
			break
		}
	}
	if !assigned {
		return nil, errMCPForbidden
	}
	var updated domain.ReleaseOrderApprovalFlowTask
	if approve {
		updated, err = a.Release.manager.ApproveApprovalFlowTask(ctx, order.ID, in.TaskID, user.ID, resolveTriggeredBy(user), in.Comment)
	} else {
		updated, err = a.Release.manager.RejectApprovalFlowTask(ctx, order.ID, in.TaskID, user.ID, resolveTriggeredBy(user), in.Comment)
	}
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := toReleaseOrderApprovalFlowTaskResponse(updated)
	resp.Message = ""
	for i := range resp.Records {
		resp.Records[i].Comment = ""
	}
	return resp, nil
}
func (a *mcpAPI) approveTask(ctx context.Context, in mcpTaskActionInput) (any, error) {
	return a.approvalTask(ctx, in, true)
}
func (a *mcpAPI) rejectTask(ctx context.Context, in mcpTaskActionInput) (any, error) {
	return a.approvalTask(ctx, in, false)
}
func (a *mcpAPI) dispatch(ctx context.Context, in mcpIDInput, action string) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "release.execute")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	var updated domain.ReleaseOrder
	switch action {
	case "build":
		updated, err = a.Release.manager.Build(ctx, order.ID, user.ID, resolveTriggeredBy(user))
	case "deploy":
		updated, err = a.Release.manager.Deploy(ctx, order.ID, user.ID, resolveTriggeredBy(user))
	case "execute":
		updated, err = a.Release.manager.Execute(ctx, order.ID, user.ID, resolveTriggeredBy(user))
	}
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) buildOrder(ctx context.Context, in mcpIDInput) (any, error) {
	return a.dispatch(ctx, in, "build")
}
func (a *mcpAPI) deployOrder(ctx context.Context, in mcpIDInput) (any, error) {
	return a.dispatch(ctx, in, "deploy")
}
func (a *mcpAPI) executeOrder(ctx context.Context, in mcpIDInput) (any, error) {
	return a.dispatch(ctx, in, "execute")
}
func (a *mcpAPI) batchExecuteOrders(ctx context.Context, in mcpBatchExecuteInput) (any, error) {
	if len(in.OrderIDs) < 2 || len(in.OrderIDs) > 50 {
		return nil, errors.New("order_ids must contain between 2 and 50 items")
	}
	if strings.TrimSpace(in.BatchName) == "" {
		return nil, errors.New("batch_name is required")
	}
	for _, id := range in.OrderIDs {
		if _, err := a.existingForWrite(ctx, id, "release.execute"); err != nil {
			return nil, err
		}
	}
	output, err := a.Release.manager.BatchExecute(ctx, usecase.BatchExecuteReleaseOrdersInput{OrderIDs: in.OrderIDs, BatchName: in.BatchName, StagedDispatchMode: usecase.BatchExecuteStagedDispatchMode(in.StagedDispatchMode)})
	if err != nil {
		return nil, mcpSafeError(err)
	}
	orders := make([]ReleaseOrderResponse, 0, len(output.Orders))
	for _, item := range output.Orders {
		orders = append(orders, mcpOrder(item))
	}
	return map[string]any{"batch_no": output.BatchNo, "batch_name": output.BatchName, "orders": orders, "dispatch_error_count": len(output.DispatchErrors)}, nil
}
func (a *mcpAPI) cancelOrder(ctx context.Context, in mcpIDInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	if user.Role != userdomain.RoleAdmin && !mcpIsCreator(user, order) {
		return nil, errMCPForbidden
	}
	updated, err := a.Release.manager.Cancel(ctx, order.ID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) createRollback(ctx context.Context, in mcpIDInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "release.create")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	updated, err := a.Release.manager.CreateStandardRollbackByOrder(ctx, order.ID, user.ID, resolveTriggeredBy(user))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) createReplay(ctx context.Context, in mcpReplayInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "release.create")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	params := make([]usecase.CreateReleaseOrderParamInput, 0, len(in.CDParams))
	for _, item := range in.CDParams {
		params = append(params, usecase.CreateReleaseOrderParamInput{PipelineScope: domain.PipelineScope(strings.ToLower(strings.TrimSpace(item.PipelineScope))), ParamKey: strings.ToLower(strings.TrimSpace(item.ParamKey)), ExecutorParamName: item.ExecutorParamName, ParamValue: item.ParamValue, ValueSource: domain.ValueSource(item.ValueSource)})
	}
	updated, err := a.Release.manager.CreatePipelineReplayByOrderWithOptions(ctx, order.ID, user.ID, resolveTriggeredBy(user), usecase.CreatePipelineReplayOptions{OverrideCDParams: in.OverrideCDParams || len(in.CDParams) > 0, CDParams: params})
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(updated), nil
}
func (a *mcpAPI) createRecovery(ctx context.Context, in mcpRecoveryInput) (any, error) {
	if strings.TrimSpace(in.EnvCode) == "" {
		return nil, errors.New("env_code is required")
	}
	if err := a.permission(ctx, "release.create", in.ApplicationID, in.EnvCode); err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	order, err := a.Release.manager.CreateApplicationRollbackOrder(ctx, in.ApplicationID, in.EnvCode, usecase.RollbackSupportedAction(strings.ToLower(strings.TrimSpace(in.Action))), user.ID, resolveTriggeredBy(user))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	return mcpOrder(order), nil
}
func (a *mcpAPI) confirmLive(ctx context.Context, in mcpIDInput) (any, error) {
	order, err := a.existingForWrite(ctx, in.OrderID, "release.execute")
	if err != nil {
		return nil, err
	}
	user, _ := mcpUser(ctx)
	state, err := a.Release.manager.ConfirmAppReleaseState(ctx, order.ID, resolveTriggeredBy(user))
	if err != nil {
		return nil, mcpSafeError(err)
	}
	resp := mcpOrder(order)
	resp.LiveStateStatus, resp.LiveStateIsCurrent, resp.LiveStateConfirmedAt = string(state.StateStatus), state.IsCurrentLive, state.ConfirmedAt
	return resp, nil
}
