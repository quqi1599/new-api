# CPA 渠道联合审计修复（2026-10-16）

本次为本地修复与合成测试，未提交、推送、部署，也未调用真实模型或修改线上配置。官方对照见[固定版本核查与复现](../reviews/2026-10-16-official-newapi/comparison_CN.md)。

## 请求拒绝与共同入口

- 仅在确有上游 HTTP 回包、错误代码精确为 `upstream_invalid_request`、状态为 `422` 时，停止本次请求的原样重试，不因参数拒绝处罚渠道。保留原始状态、代码、类型与说明。CPA 普通错误类型为 `cpa_error`，Messages 错误类型为 `api_error`，两种实际 HTTP 封装均已测试。
- 对 `auth_unavailable`、`auth_not_found`、`codex_channels_unavailable` 的精确 503 回包，只排除本次请求中**同服务、同协议**的重复入口，避免同协议别名被反复选中；不同协议仍可接管。
- 不对通用 `upstream_unavailable` 自动合并去重。CPA 的 `writeExecutionError` 在无法提取具体 Failure 时也会使用这个默认代码；单凭它不能证明所有候选耗尽。普通 429/500/502/503/504 均有保持现有恢复行为的反例测试。
- 同服务身份包括完整配置 BaseURL、凭据摘要、组织、模型映射、渠道协议类型，并以本次请求模型隔离。只规范化主机名和默认端口；路径、末尾斜杠、转义路径及查询均保留。不硬编码渠道编号。不同凭据/租户、模型、路径、组织、映射不合并；多 Key、配置或本次有效参数/请求头覆盖的渠道保守地不合并。摘要仅存请求内存，不写日志。
- CPA 的 `noEligibleRouteError` 会把 `json_object_messages_unsupported` 转成 `auth_unavailable/503`，明确允许兼容 OpenAI 入口接管。因此不能按同主机把 Messages 与 Chat 一起排除。新增测试保留这一跨协议能力切换。
- 有输出、工具调用或未知上游执行结果时继续禁止重放。同服务候选跳过原因记为 `gateway_service_exhausted`，不消耗实际上游调用次数，但仍受选择数量与时间预算限制。

## 结构化输出与转换

Chat 的 `response_format`、Responses 的 `text.format`、Messages 的 `output_config.format` 与旧 `output_format` 现在在相应转换中保留 JSON Schema。Schema 使用 RawMessage，不经过浮点数 map，因此 `9007199254740993`、零值、false、数组及未知 Schema 关键词不会被舍入或删除。Chat 与 Responses 之间保留 `strict:false`；Messages 原生 Schema 模式不能等价表达非严格 Schema，因此明确拒绝该候选，并允许其他兼容入口接管。

Messages 没有等价的通用 JSON object 模式。本地转换会在首次生成前返回精确的路由不兼容错误，候选永久排除，保留原请求交给兼容 Chat/Responses 入口。未知格式扩展不能静默丢掉：跨协议转换明确拒绝；原生 Chat 请求的扩展保留在 DTO 内。无兼容入口时返回 422；结构错误、错误类型的 strict 或冲突的新旧 Messages 格式返回 400，不生成。

Messages 的格式对象仅发送 `type` 和原始 `schema`。Chat 格式名称与描述按 CPA 现有合同作为 Schema 注释保留，不改变有效值集合；`strict:true` 对应 Messages 原生 Schema 约束，`strict:false` 不伪装成 strict。Messages 转 OpenAI Schema 使用原生严格语义。不会降级为字符串提示或伪造工具调用。

Claude effort 自动适配只修改 `output_config.effort`，不再覆盖已提供的格式对象；显式 stream=false、temperature/top_p/max_tokens=0 在相关转换中保留。该行为不表示上游模型必然接受零预算，模型仍可明确拒绝。

## 模型终态与请求最终结果

Claude 普通及流式输出，包括转 Chat/Responses，统一记录 `max_tokens → incomplete / max_output_tokens`、`refusal → incomplete / content_filter`；`end_turn`、`stop_sequence`、`tool_use` 为 completed。兼容 CPA 桥接的 `stop_reason=incomplete`、`cpa_terminal` 和 `cpa_usage`。CPA 起始事件里的占位零不会在最终明确未报告时被记为实际用量。客户端原停止原因及实际用量保留。

保留既有 `responses_outcome` 的模型终态与计费信息，新增 `generation_outcome` 表示包含流传输结果的最终状态。例如模型先截断、随后客户断开时，前者保留 `max_output_tokens`，后者为 `cancelled / client_gone`。后台优先展示有有效 state 的新字段；缺少新字段或只有空对象的旧日志仍回退原字段，缺少来源显示 unreported。

EOF、流错误或超时不能计作完整成功或恢复渠道健康；客户实际写入失败记 cancelled，并保留部分用量及现有一次结算标记，不重试、不补成功终止帧。达到输出长度上限本身不处罚渠道，也不自动重试。

## 本地验证与限制

已用完全合成 HTTP 服务检查：精确 422 只调用一次；同协议别名不循环；不同协议与独立备用保留；通用瞬时错误不过度去重；Chat↔Messages、Responses→Messages、Chat/Messages 经 Responses 出站的普通与 SSE 请求均检查实际 body。

格式验证包含完整 Schema、大整数、strict:false、零值、false、未知扩展、无候选时零生成，以及 effort 合并；终态验证包含正常、截断、拒绝、缺少 message_stop、上游错误、客户端断开、实报零与未报告。最终版本已通过 8 个相关 Go 包全量测试、新增用例的 5 包 race 检查、相关包 go vet、前端改动文件 Prettier 及 git diff --check。命令与输出保存在[核查目录](../reviews/2026-10-16-official-newapi/comparison_CN.md#本地修复验证)。

跨域服务别名、多 Key 池、存在覆盖规则而无法证明同一执行范围的渠道暂不自动合并。不读取或变更生产凭据以扩大去重。
