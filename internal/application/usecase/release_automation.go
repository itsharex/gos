package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appdomain "gos/internal/domain/application"
	releasedomain "gos/internal/domain/release"
	automationdomain "gos/internal/domain/releaseautomation"
	"gos/internal/support/logx"
)

const (
	// releaseAutomationBatchLimit 是一次轮询最多处理的配置数：每条配置都要读一次
	// 远端 git（密码凭证走 git clone，可能秒级），不设上限会把一轮任务拖成小时级。
	releaseAutomationBatchLimit = 50
	// releaseAutomationLogComponent 是结构化日志的组件名，便于 ELK 按组件过滤。
	releaseAutomationLogComponent = "release_automation"
	// releaseAutomationDispatchRetryWindow 是「建单后派发失败」的补派发窗口：
	// 窗口内每轮轮询都会重试派发，超过窗口仍派不动就把单子置为失败并放弃这个提交，
	// 避免一张永远发不出去的单永久堵住后续建单。
	releaseAutomationDispatchRetryWindow = 10 * time.Minute
	// releaseAutomationManualBlockerTTL 是自动化给人为待执行单让路的上限：
	// 手工单建出来就挡在自动化前面是合理的（避免互相插队），但长期停在待执行的
	// 弃单不应该无限期挡住自动化，超过这个时限后不再把它算作阻塞单。
	releaseAutomationManualBlockerTTL = 24 * time.Hour
)

// ReleaseAutomationApplicationReader 只取应用名、仓库地址和归属校验，用最小接口隔离
// 应用大仓储，测试里可以只实现用到的方法。
type ReleaseAutomationApplicationReader interface {
	GetByID(ctx context.Context, id string) (appdomain.Application, error)
}

// ReleaseAutomationTemplateReader 只取模板元信息（模板名与归属应用）。
type ReleaseAutomationTemplateReader interface {
	GetTemplateByID(
		ctx context.Context,
		id string,
	) (releasedomain.ReleaseTemplate, []releasedomain.ReleaseTemplateBinding, []releasedomain.ReleaseTemplateParam, []releasedomain.ReleaseTemplateGitOpsRule, []releasedomain.ReleaseTemplateHook, error)
}

// ReleaseAutomationOrderDispatcher 是发布自动化和发布单执行链路之间唯一的接缝。
// *ReleaseOrderManager 天然实现它，测试里用假实现即可断言派发方式。
type ReleaseAutomationOrderDispatcher interface {
	Create(ctx context.Context, input CreateReleaseOrderInput) (releasedomain.ReleaseOrder, error)
	Build(ctx context.Context, id string, operatorUserID string, operatorName string) (releasedomain.ReleaseOrder, error)
	Deploy(ctx context.Context, id string, operatorUserID string, operatorName string) (releasedomain.ReleaseOrder, error)
	Execute(ctx context.Context, id string, operatorUserID string, operatorName string) (releasedomain.ReleaseOrder, error)
	// AutoDeployBuiltOrder 给「构建并发布」补上 CI 与 CD 之间的那一步：
	// 构建完成、单子停在待部署时把部署派发出去。
	AutoDeployBuiltOrder(ctx context.Context, orderID string, operatorUserID string, operatorName string) (bool, error)
	// FindActiveOrderByApplicationEnv 返回同应用同环境下的在途发布单；
	// releasedomain.ErrOrderNotFound 表示当前没有在途单，可以自动建单。
	FindActiveOrderByApplicationEnv(ctx context.Context, applicationID string, envCode string) (releasedomain.ReleaseOrder, error)
	// FindOpenOrderByApplicationEnv 返回同应用同环境下尚未结束（含待执行）的发布单；
	// 轮询触发用它判断「是否已有手工发布排在前面」。createdAfter 非零时只统计该
	// 时间之后创建的单，超过让路时限的人为弃单不再阻塞自动化。
	FindOpenOrderByApplicationEnv(ctx context.Context, applicationID string, envCode string, excludeReleaseOrderID string, createdAfter time.Time) (releasedomain.ReleaseOrder, error)
	// GetByID 读取发布单，用于识别上一轮建出但没派发成功的单。
	GetByID(ctx context.Context, id string) (releasedomain.ReleaseOrder, error)
	// FailUndispatchedOrder 把「建单后从未派发成功」的待执行单置为失败：
	// 自动化补派发超时后用它终止孤儿单，避免它永久占住同应用同环境的建单入口。
	FailUndispatchedOrder(ctx context.Context, orderID string, reason string) (bool, error)
}

// ReleaseAutomationManager 是「发布自动化」的应用层入口：
// 配置 CRUD 走这里，轮询建单也走这里。
type ReleaseAutomationManager struct {
	repo       automationdomain.Repository
	apps       ReleaseAutomationApplicationReader
	templates  ReleaseAutomationTemplateReader
	orders     ReleaseAutomationOrderDispatcher
	gitCommits ReleaseOrderHeadCommitResolver
	now        func() time.Time
}

func NewReleaseAutomationManager(
	repo automationdomain.Repository,
	apps ReleaseAutomationApplicationReader,
	templates ReleaseAutomationTemplateReader,
	orders ReleaseAutomationOrderDispatcher,
	gitCommits ReleaseOrderHeadCommitResolver,
) *ReleaseAutomationManager {
	return &ReleaseAutomationManager{
		repo:       repo,
		apps:       apps,
		templates:  templates,
		orders:     orders,
		gitCommits: gitCommits,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

// ReleaseAutomationInput 是 Create/Update 共用的入参。
type ReleaseAutomationInput struct {
	Name          string
	ApplicationID string
	TemplateID    string
	EnvCode       string
	GitRef        string
	DispatchMode  automationdomain.DispatchMode
	// Enabled 为 nil 表示按场景取默认值：创建时启用，更新时保持原值。
	Enabled *bool
	Params  []CreateReleaseOrderParamInput
	Remark  string
	// CheckGit 为 nil 表示默认校验（保存前必须能读到分支 HEAD）。
	CheckGit       *bool
	OperatorUserID string
	OperatorName   string
}

// ReleaseAutomationCheckResult 是「检查 git 权限」的结果。读取失败不是错误，
// 而是 reachable=false 加一条可读原因，页面直接展示。
type ReleaseAutomationCheckResult struct {
	Reachable bool   `json:"reachable"`
	HeadSHA   string `json:"head_sha"`
	Message   string `json:"message"`
}

// RunDueReleaseAutomationsOutput 是一轮轮询的统计输出。
type RunDueReleaseAutomationsOutput struct {
	Scanned   int
	Triggered int
	Unchanged int
	Blocked   int
	Failed    int
	// Deployed 是本轮替「构建并发布」的自动单补派发出去的部署数。
	Deployed int
	// DeployWaiting 是本轮该部署但暂时派不动（并发锁被占、等审批）的自动单数。
	DeployWaiting int
}

// releaseAutomationRunState 是单条配置在一轮轮询里的结果状态。
type releaseAutomationRunState int

const (
	releaseAutomationRunFailed releaseAutomationRunState = iota
	releaseAutomationRunUnchanged
	releaseAutomationRunTriggered
	releaseAutomationRunBlocked
)

// FindOpenOrderByApplicationEnv 返回同应用同环境下第一个尚未结束的发布单。
func (uc *ReleaseOrderManager) FindOpenOrderByApplicationEnv(
	ctx context.Context,
	applicationID string,
	envCode string,
	excludeReleaseOrderID string,
	createdAfter time.Time,
) (releasedomain.ReleaseOrder, error) {
	if uc == nil || uc.repo == nil {
		return releasedomain.ReleaseOrder{}, releasedomain.ErrOrderNotFound
	}
	return uc.repo.FindOpenOrderByApplicationEnv(ctx, applicationID, envCode, excludeReleaseOrderID, createdAfter)
}

// FindActiveOrderByApplicationEnv 返回同应用同环境下第一个在途发布单。
// releasedomain.ErrOrderNotFound 表示当前没有在途单。
func (uc *ReleaseOrderManager) FindActiveOrderByApplicationEnv(
	ctx context.Context,
	applicationID string,
	envCode string,
) (releasedomain.ReleaseOrder, error) {
	if uc == nil || uc.repo == nil {
		return releasedomain.ReleaseOrder{}, releasedomain.ErrOrderNotFound
	}
	return uc.repo.FindActiveOrderByApplicationEnv(ctx, applicationID, envCode, "")
}

func (uc *ReleaseAutomationManager) Create(ctx context.Context, input ReleaseAutomationInput) (automationdomain.Automation, error) {
	clean, err := uc.normalizeInput(ctx, input)
	if err != nil {
		return automationdomain.Automation{}, err
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	now := uc.now()
	item := automationdomain.Automation{
		ID:              generateID("rauto"),
		Name:            clean.name,
		ApplicationID:   clean.application.ID,
		ApplicationName: clean.application.Name,
		TemplateID:      clean.template.ID,
		TemplateName:    clean.template.Name,
		EnvCode:         clean.envCode,
		GitRef:          clean.gitRef,
		DispatchMode:    clean.dispatchMode,
		Enabled:         enabled,
		Params:          clean.params,
		Remark:          clean.remark,
		CreatorUserID:   strings.TrimSpace(input.OperatorUserID),
		CreatorName:     strings.TrimSpace(input.OperatorName),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// 保存前先确认分支可读，并把当前 HEAD 落成基线：这样「配置完就立刻发一版」
	// 不会发生，第一次自动发布必然由保存之后的新提交触发。
	if clean.checkGit {
		head, headErr := uc.resolveHeadCommit(ctx, clean.application, clean.gitRef)
		if headErr != nil {
			logx.Warn(releaseAutomationLogComponent, "create_git_check_failed",
				logx.F("application_id", clean.application.ID),
				logx.F("git_ref", clean.gitRef),
				logx.F("reason", headErr.Error()),
			)
			return automationdomain.Automation{}, headErr
		}
		item.LastSeenSHA = strings.TrimSpace(head.CommitSHA)
	}

	if err := uc.repo.Create(ctx, item); err != nil {
		logx.Error(releaseAutomationLogComponent, "create_failed", err,
			logx.F("application_id", item.ApplicationID),
			logx.F("env_code", item.EnvCode),
			logx.F("git_ref", item.GitRef),
		)
		return automationdomain.Automation{}, err
	}
	logx.Info(releaseAutomationLogComponent, "create_success",
		logx.F("automation_id", item.ID),
		logx.F("application_id", item.ApplicationID),
		logx.F("env_code", item.EnvCode),
		logx.F("git_ref", item.GitRef),
		logx.F("dispatch_mode", item.DispatchMode),
		logx.F("baseline_sha", item.LastSeenSHA),
	)
	return uc.repo.GetByID(ctx, item.ID)
}

func (uc *ReleaseAutomationManager) Update(ctx context.Context, id string, input ReleaseAutomationInput) (automationdomain.Automation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return automationdomain.Automation{}, ErrInvalidID
	}
	current, err := uc.repo.GetByID(ctx, id)
	if err != nil {
		return automationdomain.Automation{}, err
	}
	clean, err := uc.normalizeInput(ctx, input)
	if err != nil {
		return automationdomain.Automation{}, err
	}

	identityChanged := current.IdentityChanged(automationdomain.Automation{
		ApplicationID: clean.application.ID,
		EnvCode:       clean.envCode,
		GitRef:        clean.gitRef,
	})
	// 应用/环境/分支变了就必须重新取基线：旧 sha 属于旧分支，留着会被当成
	// 「新提交已处理」而漏发，或反过来立刻误发一单。拿不到 HEAD 时拒绝保存，
	// 不允许把一条基线不明的配置留在库里。
	resetBaseline := identityChanged

	enabled := current.Enabled
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	updated := current
	updated.Name = clean.name
	updated.ApplicationID = clean.application.ID
	updated.ApplicationName = clean.application.Name
	updated.TemplateID = clean.template.ID
	updated.TemplateName = clean.template.Name
	updated.EnvCode = clean.envCode
	updated.GitRef = clean.gitRef
	updated.DispatchMode = clean.dispatchMode
	updated.Enabled = enabled
	updated.Params = clean.params
	updated.Remark = clean.remark
	updated.UpdatedAt = uc.now()

	var resolvedHeadSHA string
	if clean.checkGit || resetBaseline {
		head, headErr := uc.resolveHeadCommit(ctx, clean.application, clean.gitRef)
		if headErr != nil {
			logx.Warn(releaseAutomationLogComponent, "update_git_check_failed",
				logx.F("automation_id", id),
				logx.F("application_id", clean.application.ID),
				logx.F("git_ref", clean.gitRef),
				logx.F("reason", headErr.Error()),
			)
			return automationdomain.Automation{}, headErr
		}
		resolvedHeadSHA = strings.TrimSpace(head.CommitSHA)
	}

	// 配置字段先落库（有意不写基线），再单独重设基线：基线只属于轮询，
	// ResetBaseline 自带条件校验，不会被这次编辑用来覆盖轮询刚推进的值。
	if err := uc.repo.Update(ctx, updated); err != nil {
		logx.Error(releaseAutomationLogComponent, "update_failed", err,
			logx.F("automation_id", id),
		)
		return automationdomain.Automation{}, err
	}
	if resetBaseline {
		committed, resetErr := uc.repo.ResetBaseline(
			ctx,
			id,
			updated.ApplicationID,
			updated.EnvCode,
			updated.GitRef,
			current.LastSeenSHA,
			resolvedHeadSHA,
			updated.UpdatedAt,
		)
		if resetErr != nil {
			logx.Error(releaseAutomationLogComponent, "update_reset_baseline_failed", resetErr,
				logx.F("automation_id", id),
			)
			return automationdomain.Automation{}, resetErr
		}
		if !committed {
			// 轮询或另一个编辑者已经改过这一行：它的基线更新，保留对方的写入。
			logx.Warn(releaseAutomationLogComponent, "update_reset_baseline_skipped",
				logx.F("automation_id", id),
				logx.F("expected_seen_sha", current.LastSeenSHA),
				logx.F("head_sha", resolvedHeadSHA),
			)
		}
	}
	logx.Info(releaseAutomationLogComponent, "update_success",
		logx.F("automation_id", id),
		logx.F("application_id", updated.ApplicationID),
		logx.F("env_code", updated.EnvCode),
		logx.F("git_ref", updated.GitRef),
		logx.F("dispatch_mode", updated.DispatchMode),
		logx.F("baseline_reset", resetBaseline),
	)
	return uc.repo.GetByID(ctx, id)
}

func (uc *ReleaseAutomationManager) Get(ctx context.Context, id string) (automationdomain.Automation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return automationdomain.Automation{}, ErrInvalidID
	}
	return uc.repo.GetByID(ctx, id)
}

func (uc *ReleaseAutomationManager) List(ctx context.Context, filter automationdomain.ListFilter) ([]automationdomain.Automation, int64, error) {
	const (
		defaultPage     = 1
		defaultPageSize = 20
		maxPageSize     = 100
	)
	filter.Keyword = strings.TrimSpace(filter.Keyword)
	filter.ApplicationID = strings.TrimSpace(filter.ApplicationID)
	if filter.Page <= 0 {
		filter.Page = defaultPage
	}
	if filter.PageSize <= 0 {
		filter.PageSize = defaultPageSize
	}
	if filter.PageSize > maxPageSize {
		filter.PageSize = maxPageSize
	}
	return uc.repo.List(ctx, filter)
}

func (uc *ReleaseAutomationManager) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrInvalidID
	}
	return uc.repo.Delete(ctx, id)
}

// CheckNow 是页面上的「检查 git 权限」按钮：只读探测，不落库。
//
// 读取失败不是错误而是结果（Reachable=false + 可读原因），页面不需要区分 HTTP 分支。
func (uc *ReleaseAutomationManager) CheckNow(ctx context.Context, id string) (ReleaseAutomationCheckResult, error) {
	item, err := uc.Get(ctx, id)
	if err != nil {
		return ReleaseAutomationCheckResult{}, err
	}
	application, err := uc.loadApplication(ctx, item.ApplicationID)
	if err != nil {
		return ReleaseAutomationCheckResult{}, err
	}
	repoURL := strings.TrimSpace(application.RepoURL)
	if repoURL == "" {
		return ReleaseAutomationCheckResult{Message: "应用未配置 Git 仓库地址"}, nil
	}
	if uc.gitCommits == nil {
		return ReleaseAutomationCheckResult{Message: "Git 读取通道不可用"}, nil
	}
	head, headErr := uc.gitCommits.ResolveHeadCommit(ctx, repoURL, item.GitRef)
	if headErr != nil {
		return ReleaseAutomationCheckResult{Message: readableGitCheckReason(headErr)}, nil
	}
	return ReleaseAutomationCheckResult{
		Reachable: true,
		HeadSHA:   strings.TrimSpace(head.CommitSHA),
	}, nil
}

// RunDueAutomations 轮询一轮：遍历启用中的配置，发现分支 HEAD 前进就按配置建单并派发。
//
// 单条配置失败只影响自己（记录 last_error 后继续），panic 也不会拖垮整轮；
// 只有列举配置本身失败才返回错误，交给调度器打日志。
func (uc *ReleaseAutomationManager) RunDueAutomations(ctx context.Context, limit int) (RunDueReleaseAutomationsOutput, error) {
	output := RunDueReleaseAutomationsOutput{}
	if limit <= 0 || limit > releaseAutomationBatchLimit {
		limit = releaseAutomationBatchLimit
	}
	items, err := uc.repo.ListEnabled(ctx, limit)
	if err != nil {
		return output, err
	}
	output.Scanned = len(items)
	for _, item := range items {
		switch uc.runDueAutomation(ctx, item) {
		case releaseAutomationRunTriggered:
			output.Triggered++
		case releaseAutomationRunUnchanged:
			output.Unchanged++
		case releaseAutomationRunBlocked:
			output.Blocked++
		default:
			output.Failed++
		}
	}
	// 建单这批处理完后再续跑上一批「构建并发布」的单子：
	// 构建要跑几分钟，等下一轮把它们从「已构建待部署」推去部署。
	for _, item := range items {
		dispatched, waiting := uc.continueBuiltAutomationOrders(ctx, item)
		if dispatched {
			output.Deployed++
		}
		if waiting {
			output.DeployWaiting++
		}
	}
	return output, nil
}

// continueBuiltAutomationOrders 把「构建并发布」的单子从构建完成推到部署。
//
// 普通发布单在 CI 和 CD 之间是故意停下等人工确认的，自动化没人点这一步，所以由轮询补。
// 只有派不动时才把原因写进 last_error（例如并发锁被别的单占着），
// 状态还没到（构建没完成、已经在部署）时保持沉默，避免刷出无意义的"异常"。
func (uc *ReleaseAutomationManager) continueBuiltAutomationOrders(
	ctx context.Context,
	item automationdomain.Automation,
) (dispatched bool, waiting bool) {
	if item.DispatchMode != automationdomain.DispatchModeBuildDeploy {
		return false, false
	}
	orderID := strings.TrimSpace(item.LastOrderID)
	if orderID == "" {
		return false, false
	}
	userID := strings.TrimSpace(item.CreatorUserID)
	userName := firstNonEmpty(strings.TrimSpace(item.CreatorName), userID, item.Name)
	done, err := uc.orders.AutoDeployBuiltOrder(ctx, orderID, userID, userName)
	switch {
	case err != nil:
		logx.Warn(releaseAutomationLogComponent, "auto_deploy_waiting",
			logx.F("automation_id", item.ID),
			logx.F("order_id", orderID),
			logx.F("reason", err.Error()),
		)
		if updateErr := uc.repo.UpdateCheckState(ctx, item.ID, uc.now(), err.Error()); updateErr != nil {
			logx.Warn(releaseAutomationLogComponent, "auto_deploy_state_failed",
				logx.F("automation_id", item.ID),
				logx.F("reason", updateErr.Error()),
			)
		}
		return false, true
	case done:
		logx.Info(releaseAutomationLogComponent, "auto_deploy_dispatched",
			logx.F("automation_id", item.ID),
			logx.F("order_id", orderID),
		)
		if updateErr := uc.repo.UpdateCheckState(ctx, item.ID, uc.now(), ""); updateErr != nil {
			logx.Warn(releaseAutomationLogComponent, "auto_deploy_state_failed",
				logx.F("automation_id", item.ID),
				logx.F("reason", updateErr.Error()),
			)
		}
		return true, false
	default:
		return false, false
	}
}

// runDueAutomation 处理单条配置。所有异常都在这里收口：任何 panic 都会被转成
// failed 写入 last_error，绝不影响同一轮的其他配置和调度协程。
func (uc *ReleaseAutomationManager) runDueAutomation(
	ctx context.Context,
	item automationdomain.Automation,
) (state releaseAutomationRunState) {
	state = releaseAutomationRunFailed
	defer func() {
		if recovered := recover(); recovered != nil {
			state = releaseAutomationRunFailed
			reason := fmt.Sprintf("轮询异常：%v", recovered)
			logx.Error(releaseAutomationLogComponent, "poll_panicked", errorForPanic(recovered),
				logx.F("automation_id", item.ID),
			)
			_ = uc.repo.UpdateCheckState(ctx, item.ID, uc.now(), reason)
		}
	}()

	now := uc.now()
	application, err := uc.loadApplication(ctx, item.ApplicationID)
	if err != nil {
		uc.markCheckFailed(ctx, item, now, readableGitCheckReason(err))
		return releaseAutomationRunFailed
	}
	repoURL := strings.TrimSpace(application.RepoURL)
	if repoURL == "" {
		uc.markCheckFailed(ctx, item, now, "应用未配置 Git 仓库地址")
		return releaseAutomationRunFailed
	}
	if uc.gitCommits == nil {
		uc.markCheckFailed(ctx, item, now, "Git 读取通道不可用")
		return releaseAutomationRunFailed
	}

	head, headErr := uc.gitCommits.ResolveHeadCommit(ctx, repoURL, item.GitRef)
	if headErr != nil {
		uc.markCheckFailed(ctx, item, now, readableGitCheckReason(headErr))
		return releaseAutomationRunFailed
	}
	headSHA := strings.TrimSpace(head.CommitSHA)

	// 先收尾上一轮「建单成功、派发失败」留下的待执行单：窗口内补派发，超时则终止。
	// 它必须排在 HEAD 检查和阻塞检查之前，否则这张自己建的单会把自己挡在门外。
	if handled, state := uc.reconcileUndispatchedOrder(ctx, item, now); handled {
		return state
	}

	// 基线未前进：只刷新检查时间，保持 last_error 为空（读得到分支就是健康）。
	if automationdomain.HeadUnchanged(item.LastSeenSHA, headSHA) {
		if err := uc.repo.UpdateCheckState(ctx, item.ID, now, ""); err != nil {
			logx.Warn(releaseAutomationLogComponent, "poll_check_state_failed",
				logx.F("automation_id", item.ID),
				logx.F("reason", err.Error()),
			)
			return releaseAutomationRunFailed
		}
		return releaseAutomationRunUnchanged
	}

	// 这个提交曾经由本自动化建过单（last_triggered_sha），现在已经没有待补发的单：
	// 说明那张单被人工取消/终止了。尊重人的决定，把基线推到该提交，避免用户刚取消
	// 完、下一轮又立刻建出同一张单。
	if headSHA != "" && headSHA == strings.TrimSpace(item.LastTriggeredSHA) {
		committed, commitErr := uc.repo.CommitTrigger(
			ctx,
			item.ID,
			item.LastSeenSHA,
			headSHA,
			strings.TrimSpace(item.LastTriggeredSHA),
			strings.TrimSpace(item.LastOrderID),
			now,
			"",
		)
		if commitErr != nil {
			logx.Error(releaseAutomationLogComponent, "poll_commit_trigger_failed", commitErr,
				logx.F("automation_id", item.ID),
				logx.F("head_sha", headSHA),
			)
			return releaseAutomationRunFailed
		}
		logx.Info(releaseAutomationLogComponent, "poll_commit_skipped_terminal_order",
			logx.F("automation_id", item.ID),
			logx.F("order_id", strings.TrimSpace(item.LastOrderID)),
			logx.F("head_sha", headSHA),
			logx.F("committed", committed),
		)
		return releaseAutomationRunUnchanged
	}

	// 有新提交，但同应用同环境还有在途发布单：本次不建单，也**不推进基线**，
	// 等在途单结束后下一轮会补建，保证每个提交最终都被发一次。
	// 用「尚未结束」而不是调度视角的「在途」：手工单只要建出来（哪怕还停在待执行）就先让路，
	// 不推进基线，等它结束后下一轮再补建，避免同一分支堆出两张单。
	// 让路有时间上限：停在待执行的弃单超过 TTL 后不再算阻塞单。没有上限时，一张
	// 建完就没人管的待执行单会永久挡住这条自动化，而手工发布根本不看 pending，
	// 只有自动化会被卡死。
	blocker, blockerErr := uc.orders.FindOpenOrderByApplicationEnv(
		ctx,
		item.ApplicationID,
		item.EnvCode,
		"",
		now.Add(-releaseAutomationManualBlockerTTL),
	)
	switch {
	case blockerErr == nil:
		reason := fmt.Sprintf("已有在途发布单 %s，本次不创建自动发布单", strings.TrimSpace(blocker.OrderNo))
		uc.markCheckFailed(ctx, item, now, reason)
		logx.Info(releaseAutomationLogComponent, "poll_blocked_by_active_order",
			logx.F("automation_id", item.ID),
			logx.F("application_id", item.ApplicationID),
			logx.F("env_code", item.EnvCode),
			logx.F("active_order_no", strings.TrimSpace(blocker.OrderNo)),
			logx.F("head_sha", headSHA),
		)
		return releaseAutomationRunBlocked
	case !errors.Is(blockerErr, releasedomain.ErrOrderNotFound):
		uc.markCheckFailed(ctx, item, now, blockerErr.Error())
		return releaseAutomationRunFailed
	}

	order, createErr := uc.orders.Create(ctx, CreateReleaseOrderInput{
		ApplicationID: item.ApplicationID,
		TemplateID:    item.TemplateID,
		EnvCode:       item.EnvCode,
		GitRef:        item.GitRef,
		TriggerType:   releasedomain.TriggerTypeAutomation,
		Remark:        fmt.Sprintf("发布自动化 %s 检测到分支新提交后自动创建", item.Name),
		CreatorUserID: strings.TrimSpace(item.CreatorUserID),
		TriggeredBy:   firstNonEmpty(strings.TrimSpace(item.CreatorName), strings.TrimSpace(item.CreatorUserID), item.Name),
		Params:        releaseAutomationCreateParams(item.Params),
	})
	if createErr != nil {
		// 并发放行：预检查与建单之间别的发布单可能已经插进来。按「本轮不建单」
		// 处理，同样不推进基线，避免把没发出去的分支记成已处理。
		if errors.Is(createErr, ErrConcurrentReleaseBlocked) {
			reason := fmt.Sprintf("已有在途发布单，本次不创建自动发布单：%s", createErr.Error())
			uc.markCheckFailed(ctx, item, now, reason)
			return releaseAutomationRunBlocked
		}
		uc.markCheckFailed(ctx, item, now, createErr.Error())
		logx.Error(releaseAutomationLogComponent, "poll_create_order_failed", createErr,
			logx.F("automation_id", item.ID),
			logx.F("application_id", item.ApplicationID),
			logx.F("env_code", item.EnvCode),
			logx.F("head_sha", headSHA),
		)
		return releaseAutomationRunFailed
	}

	// 先派发再推进基线：派发失败时基线必须停在原值，否则这个提交会被记成「已处理」，
	// 而那张没派发出去的单既不会重试，又以「在途单」的身份永久挡住后续建单。
	// 失败时只把单子挂到配置上（RecordOrder 不推进基线），下一轮由
	// reconcileUndispatchedOrder 继续补派发。
	dispatchErr := uc.dispatchAutomationOrder(ctx, item, order)
	if dispatchErr != nil && !uc.dispatchLanded(ctx, order.ID) {
		reason := dispatchErr.Error()
		if _, recordErr := uc.repo.RecordOrder(
			ctx,
			item.ID,
			item.LastSeenSHA,
			headSHA,
			order.ID,
			now,
			reason,
		); recordErr != nil {
			logx.Warn(releaseAutomationLogComponent, "poll_record_order_failed",
				logx.F("automation_id", item.ID),
				logx.F("order_id", order.ID),
				logx.F("reason", recordErr.Error()),
			)
		}
		logx.Error(releaseAutomationLogComponent, "poll_dispatch_failed", dispatchErr,
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("order_no", order.OrderNo),
			logx.F("dispatch_mode", item.DispatchMode),
		)
		return releaseAutomationRunFailed
	}
	if dispatchErr != nil {
		// 状态已经推进（例如执行人写库失败这类后半段报错）：单子确实派出去了，
		// 按成功处理，否则等它跑完会为同一个提交再建一单。
		logx.Warn(releaseAutomationLogComponent, "poll_dispatch_landed_with_error",
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("order_no", order.OrderNo),
			logx.F("reason", dispatchErr.Error()),
		)
	}

	committed, commitErr := uc.repo.CommitTrigger(
		ctx,
		item.ID,
		item.LastSeenSHA,
		headSHA,
		headSHA,
		order.ID,
		now,
		"",
	)
	if commitErr != nil {
		logx.Error(releaseAutomationLogComponent, "poll_commit_trigger_failed", commitErr,
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("head_sha", headSHA),
		)
		return releaseAutomationRunFailed
	}
	if !committed {
		// 另一个副本已经认领了这一轮：发布单已经建出来（本次多建的一单需要人工
		// 判断是否取消），但基线以对方的写回为准，这里绝不覆盖。
		logx.Warn(releaseAutomationLogComponent, "poll_cas_conflict_discarded",
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("expected_seen_sha", item.LastSeenSHA),
			logx.F("head_sha", headSHA),
		)
		return releaseAutomationRunFailed
	}
	logx.Info(releaseAutomationLogComponent, "poll_triggered",
		logx.F("automation_id", item.ID),
		logx.F("application_id", item.ApplicationID),
		logx.F("env_code", item.EnvCode),
		logx.F("git_ref", item.GitRef),
		logx.F("order_id", order.ID),
		logx.F("order_no", order.OrderNo),
		logx.F("dispatch_mode", item.DispatchMode),
		logx.F("head_sha", headSHA),
	)
	return releaseAutomationRunTriggered
}

// reconcileUndispatchedOrder 收尾「上一轮建单成功、但派发失败」的待执行单。
//
// 建单和派发不是原子的：派发可能因为 Jenkins 读参数瞬时失败、预检校验失败等原因没走通。
// 这张单如果既不重试又留在待执行状态，会同时踩两个坑：提交没有被真正发布；它还会被
// 当成「同应用同环境尚未结束的单」，把这条自动化永久挡在门外。
//
// 语义：
//   - 重试窗口内每轮都补派发；派发成功才推进基线；
//   - 如果只是排在别的在途单后面（同应用同环境有 queued/building/running 的单），
//     继续等待，不进入放弃计时；
//   - 超过窗口仍派不动，说明是配置/参数这类硬错误：终止这张单并把基线推到它的 HEAD，
//     放弃这个提交，避免同一个提交反复建单。
//
// 返回 handled=false 表示没有需要收尾的遗留单，调用方继续常规的 HEAD 检查。
func (uc *ReleaseAutomationManager) reconcileUndispatchedOrder(
	ctx context.Context,
	item automationdomain.Automation,
	now time.Time,
) (handled bool, state releaseAutomationRunState) {
	orderID := strings.TrimSpace(item.LastOrderID)
	if orderID == "" {
		return false, releaseAutomationRunUnchanged
	}
	order, err := uc.orders.GetByID(ctx, orderID)
	if err != nil {
		if errors.Is(err, releasedomain.ErrOrderNotFound) {
			return false, releaseAutomationRunUnchanged
		}
		uc.markCheckFailed(ctx, item, now, err.Error())
		return true, releaseAutomationRunFailed
	}
	// 配置改过应用/环境/分支后，LastOrderID 可能指向旧身份的单，不要碰它。
	if !isAutomationOrderOf(item, order) {
		return false, releaseAutomationRunUnchanged
	}
	if order.Status != releasedomain.OrderStatusPending {
		return false, releaseAutomationRunUnchanged
	}

	// 建单时解析 HEAD 是异步写库的，下一轮读到的列可能还是空；RecordOrder 在派发
	// 失败时已经把建单 HEAD 写进 last_triggered_sha，用它兜底，避免补派发成功后
	// 推不动基线、下一轮又为同一个提交建一单。
	orderHeadSHA := strings.TrimSpace(order.HeadCommitSHA)
	if orderHeadSHA == "" {
		orderHeadSHA = strings.TrimSpace(item.LastTriggeredSHA)
	}
	dispatchErr := uc.dispatchAutomationOrder(ctx, item, order)
	if dispatchErr == nil || uc.dispatchLanded(ctx, order.ID) {
		if dispatchErr != nil {
			// 同 runDueAutomation：派发动作已经把状态推进了，按成功处理。
			logx.Warn(releaseAutomationLogComponent, "poll_dispatch_landed_with_error",
				logx.F("automation_id", item.ID),
				logx.F("order_id", order.ID),
				logx.F("order_no", order.OrderNo),
				logx.F("reason", dispatchErr.Error()),
			)
		}
		logx.Info(releaseAutomationLogComponent, "poll_dispatch_resumed",
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("order_no", order.OrderNo),
			logx.F("head_sha", orderHeadSHA),
		)
		uc.commitResumedOrder(ctx, item, order, orderHeadSHA, now)
		return true, releaseAutomationRunTriggered
	}

	// 只是排在别的在途单后面，属于正常排队：保持这张单等待，不进入放弃计时。
	if active, activeErr := uc.orders.FindActiveOrderByApplicationEnv(
		ctx,
		item.ApplicationID,
		item.EnvCode,
	); activeErr == nil {
		reason := fmt.Sprintf(
			"自动发布单 %s 等待在途发布单 %s 结束后派发",
			firstNonEmpty(strings.TrimSpace(order.OrderNo), order.ID),
			firstNonEmpty(strings.TrimSpace(active.OrderNo), active.ID),
		)
		uc.markCheckFailed(ctx, item, now, reason)
		logx.Info(releaseAutomationLogComponent, "poll_dispatch_waiting_active_order",
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("active_order_no", firstNonEmpty(strings.TrimSpace(active.OrderNo), active.ID)),
		)
		return true, releaseAutomationRunBlocked
	}

	retryMinutes := int(releaseAutomationDispatchRetryWindow.Minutes())
	reason := fmt.Sprintf(
		"自动发布单 %s 派发失败，将在 %d 分钟内重试：%s",
		firstNonEmpty(strings.TrimSpace(order.OrderNo), order.ID),
		retryMinutes,
		dispatchErr.Error(),
	)
	logx.Warn(releaseAutomationLogComponent, "poll_dispatch_retry_waiting",
		logx.F("automation_id", item.ID),
		logx.F("order_id", order.ID),
		logx.F("order_no", order.OrderNo),
		logx.F("reason", dispatchErr.Error()),
	)
	uc.markCheckFailed(ctx, item, now, reason)
	if now.Sub(order.CreatedAt) < releaseAutomationDispatchRetryWindow {
		return true, releaseAutomationRunFailed
	}

	uc.abandonUndispatchedOrder(ctx, item, order, orderHeadSHA, now, reason)
	return true, releaseAutomationRunFailed
}

// commitResumedOrder 在补派发成功后推进基线。
// 基线推到这张单的 HEAD 而不是当前分支 HEAD：补发的可能是旧提交，更新的提交
// 留给下一轮按「HEAD 已前进」重新走建单流程。
func (uc *ReleaseAutomationManager) commitResumedOrder(
	ctx context.Context,
	item automationdomain.Automation,
	order releasedomain.ReleaseOrder,
	orderHeadSHA string,
	now time.Time,
) {
	if orderHeadSHA == "" {
		return
	}
	committed, err := uc.repo.CommitTrigger(
		ctx,
		item.ID,
		item.LastSeenSHA,
		orderHeadSHA,
		orderHeadSHA,
		order.ID,
		now,
		"",
	)
	if err != nil {
		logx.Error(releaseAutomationLogComponent, "poll_commit_trigger_failed", err,
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("head_sha", orderHeadSHA),
		)
		return
	}
	if !committed {
		logx.Warn(releaseAutomationLogComponent, "poll_cas_conflict_discarded",
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
			logx.F("expected_seen_sha", item.LastSeenSHA),
			logx.F("head_sha", orderHeadSHA),
		)
	}
}

// abandonUndispatchedOrder 放弃一张始终派发不出去的自动单。
// 先推进基线再终止单子：顺序反过来时如果进程正好退出，下一轮会把同一个提交再建一次，
// 形成「失败—重建」的循环。
func (uc *ReleaseAutomationManager) abandonUndispatchedOrder(
	ctx context.Context,
	item automationdomain.Automation,
	order releasedomain.ReleaseOrder,
	orderHeadSHA string,
	now time.Time,
	reason string,
) {
	giveUpReason := fmt.Sprintf(
		"自动派发超过 %d 分钟仍失败，已终止本次发布：%s",
		int(releaseAutomationDispatchRetryWindow.Minutes()),
		strings.TrimSpace(reason),
	)
	if orderHeadSHA != "" {
		committed, err := uc.repo.CommitTrigger(
			ctx,
			item.ID,
			item.LastSeenSHA,
			orderHeadSHA,
			orderHeadSHA,
			order.ID,
			now,
			giveUpReason,
		)
		if err != nil {
			logx.Error(releaseAutomationLogComponent, "poll_commit_trigger_failed", err,
				logx.F("automation_id", item.ID),
				logx.F("order_id", order.ID),
				logx.F("head_sha", orderHeadSHA),
			)
		} else if !committed {
			logx.Warn(releaseAutomationLogComponent, "poll_cas_conflict_discarded",
				logx.F("automation_id", item.ID),
				logx.F("order_id", order.ID),
				logx.F("head_sha", orderHeadSHA),
			)
		}
	}
	if _, err := uc.orders.FailUndispatchedOrder(ctx, order.ID, giveUpReason); err != nil {
		logx.Error(releaseAutomationLogComponent, "poll_dispatch_abandon_failed", err,
			logx.F("automation_id", item.ID),
			logx.F("order_id", order.ID),
		)
	}
	logx.Error(releaseAutomationLogComponent, "poll_dispatch_abandoned",
		errors.New(giveUpReason),
		logx.F("automation_id", item.ID),
		logx.F("order_id", order.ID),
		logx.F("order_no", order.OrderNo),
		logx.F("reason", giveUpReason),
	)
}

// dispatchLanded 判断派发动作是否已经落到单子上。Build/Execute 可能先推进状态、
// 再在收尾步骤报错（例如写执行人失败），这种「带错的成功」不能再当成没派发：
// 否则基线不推进，等这张单跑完后会为同一个提交再建一单。
// 等待审批的单（pending_approval）从建单起就不是 pending，同样按「已交给后续流程」处理。
func (uc *ReleaseAutomationManager) dispatchLanded(ctx context.Context, orderID string) bool {
	current, err := uc.orders.GetByID(ctx, orderID)
	if err != nil {
		return false
	}
	return current.Status != releasedomain.OrderStatusPending
}

// isAutomationOrderOf 判断发布单是否由这条自动化配置建出，避免改过配置身份后误操作
// 上一张单。
func isAutomationOrderOf(item automationdomain.Automation, order releasedomain.ReleaseOrder) bool {
	if order.TriggerType != releasedomain.TriggerTypeAutomation {
		return false
	}
	return strings.TrimSpace(order.ApplicationID) == strings.TrimSpace(item.ApplicationID) &&
		strings.TrimSpace(order.EnvCode) == strings.TrimSpace(item.EnvCode) &&
		strings.TrimSpace(order.GitRef) == strings.TrimSpace(item.GitRef)
}

// dispatchAutomationOrder 按配置的派发方式触发发布单。
//
// 模板要求审批时，Build/Execute 会把发布单推进到审批中并返回成功，
// 后续由既有审批流接管，这里不再继续下一步（也继续不了）。
func (uc *ReleaseAutomationManager) dispatchAutomationOrder(
	ctx context.Context,
	item automationdomain.Automation,
	order releasedomain.ReleaseOrder,
) error {
	userID := strings.TrimSpace(item.CreatorUserID)
	userName := firstNonEmpty(strings.TrimSpace(item.CreatorName), strings.TrimSpace(item.CreatorUserID), item.Name)
	switch item.DispatchMode {
	case automationdomain.DispatchModeBuild:
		_, err := uc.orders.Build(ctx, order.ID, userID, userName)
		return err
	case automationdomain.DispatchModeBuildDeploy:
		// 只派发构建：构建刚下发时部署必然被「当前仍在构建中」挡住，
		// 构建完成后的部署由 continueBuiltAutomationOrders 在后续轮询里补上。
		_, err := uc.orders.Build(ctx, order.ID, userID, userName)
		return err
	case automationdomain.DispatchModeExecute:
		_, err := uc.orders.Execute(ctx, order.ID, userID, userName)
		return err
	default:
		return fmt.Errorf("%w: dispatch_mode is invalid", ErrInvalidInput)
	}
}

// markCheckFailed 记录一次「没建单」的检查结果。基线保持不变，这是「在途单结束
// 后补建」和「配置坏了持续报错」两种行为的前提。
func (uc *ReleaseAutomationManager) markCheckFailed(
	ctx context.Context,
	item automationdomain.Automation,
	checkedAt time.Time,
	reason string,
) {
	if err := uc.repo.UpdateCheckState(ctx, item.ID, checkedAt, reason); err != nil {
		logx.Warn(releaseAutomationLogComponent, "poll_check_state_failed",
			logx.F("automation_id", item.ID),
			logx.F("reason", err.Error()),
		)
	}
}

// resolveHeadCommit 读取分支 HEAD，失败时返回带可读原因的 git 校验错误。
func (uc *ReleaseAutomationManager) resolveHeadCommit(
	ctx context.Context,
	application appdomain.Application,
	gitRef string,
) (HeadCommit, error) {
	repoURL := strings.TrimSpace(application.RepoURL)
	if repoURL == "" {
		return HeadCommit{}, newReleaseAutomationGitError("应用未配置 Git 仓库地址")
	}
	if uc.gitCommits == nil {
		return HeadCommit{}, newReleaseAutomationGitError("Git 读取通道不可用")
	}
	head, err := uc.gitCommits.ResolveHeadCommit(ctx, repoURL, gitRef)
	if err != nil {
		return HeadCommit{}, newReleaseAutomationGitError(readableGitCheckReason(err))
	}
	if strings.TrimSpace(head.CommitSHA) == "" {
		return HeadCommit{}, newReleaseAutomationGitError("分支上没有读取到任何提交")
	}
	return head, nil
}

type releaseAutomationCleanInput struct {
	name         string
	application  appdomain.Application
	template     releasedomain.ReleaseTemplate
	envCode      string
	gitRef       string
	dispatchMode automationdomain.DispatchMode
	params       []automationdomain.Param
	remark       string
	checkGit     bool
}

// normalizeInput 校验并归一化创建/更新共用的配置字段。
// 创建和更新要求一致：配置必须完整，否则轮询到它只会持续报错。
func (uc *ReleaseAutomationManager) normalizeInput(
	ctx context.Context,
	input ReleaseAutomationInput,
) (releaseAutomationCleanInput, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	applicationID := strings.TrimSpace(input.ApplicationID)
	if applicationID == "" {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: application_id is required", ErrInvalidInput)
	}
	templateID := strings.TrimSpace(input.TemplateID)
	if templateID == "" {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: template_id is required", ErrInvalidInput)
	}
	envCode := strings.TrimSpace(input.EnvCode)
	if envCode == "" {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: env_code is required", ErrInvalidInput)
	}
	gitRef := strings.TrimSpace(input.GitRef)
	if gitRef == "" {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: git_ref is required", ErrInvalidInput)
	}

	dispatchMode := input.DispatchMode
	if dispatchMode == "" {
		dispatchMode = automationdomain.DefaultDispatchMode
	}
	if !dispatchMode.Valid() {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: dispatch_mode is invalid", ErrInvalidInput)
	}

	params, err := normalizeReleaseAutomationParams(input.Params)
	if err != nil {
		return releaseAutomationCleanInput{}, err
	}

	application, err := uc.loadApplication(ctx, applicationID)
	if err != nil {
		return releaseAutomationCleanInput{}, err
	}
	template, err := uc.loadTemplate(ctx, templateID)
	if err != nil {
		return releaseAutomationCleanInput{}, err
	}
	if strings.TrimSpace(template.ApplicationID) != application.ID {
		return releaseAutomationCleanInput{}, fmt.Errorf("%w: 发布模板不属于该应用", ErrInvalidInput)
	}

	return releaseAutomationCleanInput{
		name:         name,
		application:  application,
		template:     template,
		envCode:      envCode,
		gitRef:       gitRef,
		dispatchMode: dispatchMode,
		params:       params,
		remark:       strings.TrimSpace(input.Remark),
		checkGit:     input.CheckGit == nil || *input.CheckGit,
	}, nil
}

func (uc *ReleaseAutomationManager) loadApplication(ctx context.Context, applicationID string) (appdomain.Application, error) {
	if uc.apps == nil {
		return appdomain.Application{}, fmt.Errorf("%w: application repository is not configured", ErrInvalidInput)
	}
	application, err := uc.apps.GetByID(ctx, strings.TrimSpace(applicationID))
	if err != nil {
		if errors.Is(err, appdomain.ErrNotFound) {
			return appdomain.Application{}, fmt.Errorf("%w: 应用不存在或已删除", ErrInvalidInput)
		}
		return appdomain.Application{}, err
	}
	return application, nil
}

func (uc *ReleaseAutomationManager) loadTemplate(ctx context.Context, templateID string) (releasedomain.ReleaseTemplate, error) {
	if uc.templates == nil {
		return releasedomain.ReleaseTemplate{}, fmt.Errorf("%w: release template repository is not configured", ErrInvalidInput)
	}
	template, _, _, _, _, err := uc.templates.GetTemplateByID(ctx, strings.TrimSpace(templateID))
	if err != nil {
		if errors.Is(err, releasedomain.ErrTemplateNotFound) {
			return releasedomain.ReleaseTemplate{}, fmt.Errorf("%w: 发布模板不存在或已删除", ErrInvalidInput)
		}
		return releasedomain.ReleaseTemplate{}, err
	}
	return template, nil
}

// normalizeReleaseAutomationParams 校验参数形状：模板级别的参数规则（哪些必填、
// 值从哪来）由建单链路负责，这里只保证落库的参数能原样交给建单接口。
func normalizeReleaseAutomationParams(params []CreateReleaseOrderParamInput) ([]automationdomain.Param, error) {
	if len(params) == 0 {
		return []automationdomain.Param{}, nil
	}
	result := make([]automationdomain.Param, 0, len(params))
	for _, item := range params {
		scope := releasedomain.PipelineScope(strings.ToLower(strings.TrimSpace(string(item.PipelineScope))))
		if !scope.Valid() {
			return nil, fmt.Errorf("%w: pipeline_scope is invalid", ErrInvalidInput)
		}
		paramKey := strings.TrimSpace(item.ParamKey)
		if paramKey == "" {
			return nil, fmt.Errorf("%w: param_key is required", ErrInvalidInput)
		}
		valueSource := releasedomain.ValueSource(strings.TrimSpace(string(item.ValueSource)))
		if valueSource != "" && !valueSource.Valid() {
			return nil, fmt.Errorf("%w: value_source is invalid", ErrInvalidInput)
		}
		result = append(result, automationdomain.Param{
			PipelineScope:     string(scope),
			ParamKey:          paramKey,
			ExecutorParamName: strings.TrimSpace(item.ExecutorParamName),
			ParamValue:        item.ParamValue,
			ValueSource:       string(valueSource),
		})
	}
	return result, nil
}

// releaseAutomationCreateParams 把落库的参数还原成建单入参。
func releaseAutomationCreateParams(params []automationdomain.Param) []CreateReleaseOrderParamInput {
	if len(params) == 0 {
		return nil
	}
	result := make([]CreateReleaseOrderParamInput, 0, len(params))
	for _, item := range params {
		result = append(result, CreateReleaseOrderParamInput{
			PipelineScope:     releasedomain.PipelineScope(item.PipelineScope),
			ParamKey:          item.ParamKey,
			ExecutorParamName: item.ExecutorParamName,
			ParamValue:        item.ParamValue,
			ValueSource:       releasedomain.ValueSource(item.ValueSource),
		})
	}
	return result
}

// releaseOrderWaitsApproval 判断发布单是否停在审批环节：是则派发链路已经由审批流
// 接管，自动化不需要（也不能）继续推进。
func releaseOrderWaitsApproval(order releasedomain.ReleaseOrder) bool {
	return order.Status == releasedomain.OrderStatusPendingApproval || order.Status == releasedomain.OrderStatusApproving
}
