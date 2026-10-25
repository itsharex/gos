# GOS MCP 接入与发布操作

GOS 在现有服务上提供 Streamable HTTP MCP 端点 `/mcp`。支持原生 MCP 客户端通过工具查询应用和发布状态，并按令牌持有人的权限执行常规发布流程。端点使用无状态 JSON 响应；客户端应支持 Streamable HTTP，并在每次请求发送 `Authorization: Bearer <个人 MCP 令牌>`。不支持通过 URL 传递令牌或从浏览器页面携带 `Origin` 直接调用。

## 创建令牌与连接

1. 登录 GOS，打开侧栏中的「个人 MCP 令牌」（`/me/mcp-tokens`）。
2. 输入名称和有效期（默认 90 天，最多 365 天），创建并立即复制明文。关闭弹窗后只能看到名称、时间和状态，无法再次查看明文。
3. 在支持自定义 HTTP 请求头的 MCP 客户端中配置实际服务地址。若使用随 GOS 部署的前端代理，端点通常为 `https://你的-GOS-域名/mcp`；直接连接后端时为 `http://主机:8081/mcp`（以部署配置为准）。

```json
{
  "mcpServers": {
    "gos": {
      "type": "http",
      "url": "https://your-gos.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <YOUR_MCP_TOKEN>"
      }
    }
  }
}
```

上述 JSON 是支持这种配置格式的客户端的示例；其他客户端可分别填写 URL 和 Bearer 请求头。服务端仅接受 POST 请求，请不要将令牌放进 URL、日志或仓库。接口默认拒绝带 `Origin` 的请求，因此浏览器内 MCP 客户端需要由受信任的服务端转发。

令牌只用于 MCP，不接受为普通 REST 登录会话；网页登录或重新登录不会替换 MCP 令牌。用户停用、令牌到期或在页面上吊销后，新请求立即被拒绝。轮换时先创建新令牌、更新客户端、再吊销旧令牌。每个工具仍按当前用户的应用、环境、操作权限和审批人/创建者身份检查，不会因为持有令牌而提升权限。

## 工具与执行顺序

| 目的 | 工具 |
| --- | --- |
| 找到应用、环境和模板 | `list_applications`、`get_application`、`list_release_environments`、`list_release_templates`、`get_release_template` |
| 查询单据和状态 | `list_release_orders`、`get_release_order`、`get_release_status`、`list_release_artifacts`、`list_approval_tasks`、`get_release_approval_flow`、`list_release_approval_records`、`list_release_stages`、`get_release_log` |
| 执行前检查 | `get_release_precheck`（`action` 为 `build`、`deploy` 或 `execute`）、`get_recovery_precheck` |
| 建单和编辑 | `create_release_order`、`batch_create_release_orders`、`update_release_order` |
| 审批 | `submit_release_approval`、`approve_release_order`、`reject_release_order`、`approve_release_task`、`reject_release_task` |
| 执行和收尾 | `build_release_order`、`deploy_release_order`、`execute_release_order`、`batch_execute_release_orders`、`cancel_release_order`、`confirm_release_live` |
| 恢复 | `create_release_rollback`、`create_release_replay`、`create_application_recovery` |

先查询应用、环境和模板，再使用 `create_release_order` 指定 `application_id`、`env_code` 和 `template_id`。创建发布单本身不会派发流水线。需要审批时，由创建人提交，随后由被指派的审批人处理相应的传统审批或审批流任务。审批流任务通过后，原本暂停的构建或部署可能自动继续。

派发前调用 `get_release_precheck`：`execute_release_order` 按平台既有规则执行全链路；需要分段操作时先调用 `build_release_order`，到达待部署状态后再调用 `deploy_release_order`。派发可能调用 Jenkins、ArgoCD 或 GitOps；`cancel_release_order` 可能尝试终止正在运行的任务。成功部署后按实际状态调用 `confirm_release_live`，写入平台记录的当前上线版本。写工具会触发真实业务副作用，客户端在执行前应展示目标应用、环境、发布单和具体动作。

回滚与重放工具先创建一张新的发布单，**不会在创建时自动派发**；确认目标与预检查结果后，还需单独执行新单。批量创建最多 50 条，批量执行至少 2 条且需要名称；服务端在开始批量操作前检查所有目标的权限。

发布数据按应用和环境授权；本人创建或本人参与审批的单据仍按原有规则可见。工具输出省略参数值、凭证 URL 和其他自由文本；`get_release_log` 返回最多 120 行、8000 字的日志尾部摘录，含凭据关键词、URL、赋值内容或长串的行会整体隐藏。日志格式千变万化，敏感内容请勿写入构建日志；需要完整日志时在有权限的 GOS 页面查看。第一版不提供定时发布、管理员删除或审批流定义管理工具。
