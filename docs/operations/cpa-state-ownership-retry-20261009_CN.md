# CPA 状态归属错误的请求级终止

## 技术契约

对以下精确机器码标记“不重试、不惩罚渠道”，保留上游 HTTP 状态、错误码、类型和恢复说明：

- `state_owner_unavailable`：原归属或兼容状态域没有可用候选。
- `state_owner_conflict`：已记录的状态归属冲突。
- `state_owner_expired`：已记录的状态归属过期。
- `local_state_unavailable`：本地准备状态缺失，需要原连接或完整上下文。

将同一份不透明历史发回相同入口或发往无相应状态的其他入口，不能修复归属。分类由 `types/upstream_request_error.go` 统一处理 OpenAI/Claude 错误封装，不依赖错误文本、不匹配任意 `state_` 前缀，也不禁止全部 HTTP 409。

普通 409、暂态 5xx、`continuation_storage_unavailable`、auth 与 compact 保留各自策略。后续状态码映射不移除已建立的不可重试和不惩罚标记，不修改模型映射、路由配置或计费。

## 合成验证

真实 NewAPI `Relay` 加本地 HTTP、临时 SQLite/Redis 的负对照复现相同入口调用三次；修复后只调用一次并返回原错误。

四类机器码分别覆盖普通和流式请求：原入口一次、其他归属入口零次，不写虚假 SSE 前缀、不伪造完成，原始状态引用保持。普通资源冲突、暂态失败、auth 的备用路径，以及确定性请求错误和 compact 的终止边界保持。

即使配置宽泛自动禁用范围，四类归属错误也不触发渠道禁用或熔断失败；状态映射后再次核验。集成回归同时覆盖采样错误一次终止，以及已知不可用入口由独立健康入口接管。

相关 types、service、controller 全量测试、目标 race、vet 与集成检查通过。该补丁抑制错误重放，不恢复已经缺失的上游状态。
