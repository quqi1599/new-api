# Responses 与 Chat 的终态和用量契约

更新时间：2026-09-30。本文件记录本地代码契约；测试通过不代表已发布或已切换生产镜像。

## 三个独立事实

1. HTTP/SSE 是否正常送达：由 HTTP 状态、`StreamStatus` 和下游断连决定。
2. 模型是否完成生成：由 `ResponsesOutcome` 记录 `completed` 或 `incomplete`。
3. 用量是否由上游报告：输入和输出分别记录 `reported`、`estimated`、`unreported`。

`response.incomplete` 是明确的协议终态，不等同于断流、502 或模型线路故障。
`max_output_tokens` 和 `content_filter` 保留明确原因；缺失、未识别或非标准原因记为 `unknown`，不从历史长度推断上下文超限，不将任意上游字符串写入原因日志。

## 接入路径

- 原生 Responses：HTTP 响应状态以及 SSE 的 `response.incomplete`；`response.completed` / `response.done` 内嵌的 incomplete 状态同样有效。
- Responses 转 Chat：相同终态合同，保留已有输出、终态快照中尚未发出的文本/工具参数后缀。预算耗尽映射 `finish_reason=length`，过滤映射 `content_filter`。
- 普通 OpenAI Chat：从已解析的上游响应读取 `finish_reason=length/content_filter/incomplete` 和顶层 `cpa_terminal`；不会读取客户请求、消息文本内的同名字段。`stop`、`tool_calls` 和 `function_call` 保持完成语义。
- 未知 incomplete 原因没有标准 Chat 对应项，使用显式 `finish_reason=incomplete` 扩展。不能虚构 `stop` 或 token 限制。严格枚举的客户端需兼容此扩展；原生 Responses 保留其原始协议结构。
- Chat 转 Responses 保留预算耗尽/过滤/unknown 的 `incomplete_details.reason`，避免下一跳再次丢失原因。
- 未完成工具输出不会因同时包含文本而丢失。转换为 Messages 时，非对象或截断的工具参数保留为文本，避免伪造可执行的工具输入。

## 共享 CPA 凭据下的客户边界

NewAPI 在最终出站 HTTP/WS 边界覆盖保留头 `X-CPA-Principal`：以当前渠道凭据为 HMAC-SHA256 密钥，输入固定版本域、服务端配置的 `ServerAddress`、已鉴权用户 ID 和 token ID。仅发送 64 位十六进制摘要，不发送 ID 或凭据原文；客户透传、渠道 Header Override、运行时覆盖都不能决定该头。

缺失已鉴权身份、渠道测试、空/无效/localhost/loopback 的实例地址不发送 claim，并清除伪造头。CPA 在没有有效 claim 时禁用此次新增的 reasoning 回填；有效 claim 仍须与 CPA 已认证的入口凭据共同限定作用域，不能替代凭据鉴权。多个实例必须配置各自稳定的 `ServerAddress`，不能使用客户提交的 Host 作为实例身份。此次修复不新增或轮换生产秘钥。

## 调度与日志

- 明确 incomplete 到达后结束本次调用，不发起第二次生成；不触发线路失败惩罚，也不作为恢复断路器的成功证据。重试切换渠道后按实际当前渠道归还半开探测占用；真实 Relay + miniredis 的回归覆盖此边界。
- SSE 仍需完成其协议封帧。已收到 incomplete 后下游断连可同时出现 `ResponsesOutcome=incomplete` 与传输 `client_gone`，实际收到的用量不会丢失。
- 消费日志保持 `type=2`，其含义是账务消费记录。日志 `other.responses_outcome` 保存生成终态、安全原因和逐字段用量来源；`other.stream_status.status=incomplete` 在界面显示“未完成”。传输失败时后者仍为 `error`。
- 当前项目没有以消费日志计算模型质量成功率的实现；现有日志统计计算金额、token 和 RPM/TPM。外部成功率报表必须读取终态，不能把所有 `type=2` 算作完整成功。请求速率限制属于流量配额，不等同于生成质量评分。

## 结算规则

- incomplete 仅按上游实际报告的字段结算；缺失输入/输出不估算扣费，不把缺失标记成实报零。
- 显式报告的零保持零，不因输出文本存在就覆盖它。逐字段合并部分 usage 与 details，后来的省略字段不能清掉先前缓存折扣或实报零。保留缓存 token 及 `output_tokens_details.reasoning_tokens`。CPA 桥接的 `cpa_usage.input_reported/output_reported=false` 优先于数值补零，不能把合成零标记为实报。
- 上游实际完成的内置工具项仍按原工具价格计入，`failed/incomplete/partial` 工具项不计入；整体生成未完成不抹掉已经完成的工具工作。
- 完成响应与传输中断沿用原有估算策略，估算来源在日志中可见。显式 `response.failed` 等错误沿用原退款路径。
- 非流客户遇到上游 SSE 时，缓冲后仍返回 JSON，不发送 SSE ping 或 `[DONE]`。EOF 没有明确终态时返回不可重试错误；已知上游用量仍返回给调用链，但因为尚未向客户交付响应，沿用退款合同，不新增收费。已向流式客户输出后的中断则按既有 `PartialStreamError` 合同部分结算。
- 不修改历史账单，不因上线新终态分类回补历史金额。

## 本次选择性上游迁入

| 上游来源 | 当前旧 fork 的处理 |
| --- | --- |
| #5772 `3a506f50` | 迁入独立 Responses→Chat 状态机、output_index/工具 ID 关联、custom tool、推理事件、混合文本与工具、非流客户缓冲 SSE；传输与账务仍由当前宿主负责。补回终态工具快照 alias 去重和缺失 delta 时的 done 快照。 |
| #7510 `6e9de44` | 将 Responses 数组工具结果的可识别媒体提升到整批连续工具结果之后的一条 user 消息；工具 ID 和文本关联不变，任意 JSON 结果仍序列化保留。 |
| #7512 `4eb3b916` | Claude 数组工具结果中的 base64/URL 图片保留为媒体；Chat→Responses 在 closed text/reasoning 之后重新开 item，生成成对的 reasoning summary part 事件并保留各段最终输出。 |
| #7561 `c2b7a9a9e` | 已由 `0b6e509` 迁入每条 Claude message 的 `output_config`，本次不重复迁入。 |
| #7598 `789c9701` | 增加 native Claude request 的原始 `safeguards` 字段及显式 false 的往返回归。 |

保留差异：unknown incomplete 不冒充 `length` 或 `stop`；EOF 不补造完成；incomplete 不估算缺失用量；工具快照与已发送参数只合并一次；缓冲模式使用当前统一超时和取消扫描器。未迁入 relaykit 拆分、前端升级、认证、数据库或全量 rc.40。

新增永久回归包括：正常 completed 的实报输入 0、completed/incomplete 部分 usage 保留 cached token、CPA 合成零标记与 ForceFormat、并行工具媒体批次、reasoning/text 分段、custom tool 终态快照不重放、JSON 缓冲的实报零/缺失/早帧用量、EOF 退款与真实 BillingSession 单次调用。

## 验证与维护

关键回归包含：HTTP/SSE 原生和转换四条路径、已知/未知原因、实际/零/部分/缺失用量、终态后额外帧、快照补送与去重、普通 Chat 分离 usage trailer、强制格式化、下游断连，以及临时 SQLite 中真实 BillingSession 的单次结算和退款幂等。

```sh
go test ./dto ./relay/channel ./relay/channel/openai ./relay/common ./service/openaicompat ./relay ./controller ./service
go test -race ./relay/channel ./relay/channel/openai ./relay ./controller ./service/openaicompat ./dto -run 'Responses|Converted|ProtocolIncomplete|ChatIncomplete|CPAPrincipal' -count=1
```

可选的实际跨仓库串联验收（常规 CI 不依赖相邻 CPA 仓库）：

```sh
python3 tools/cpa_terminal_contract.py --cpa-root /path/to/cpa-new --output /private/acceptance-output
```

脚本构建当前 CPA 工作树的真实进程，通过 NewAPI 实际 helper 与临时 SQLite BillingSession 调用 CPA，再到合成 HTTP 上游。40 个终态/用量组合加 3 个回填隔离场景均已通过：实报/零/单侧缺失/全缺失，HTTP/SSE，Chat/Responses，预算耗尽/unknown；每次调用上游一次、结算一次。共享 CPA bearer 和相同客户端 session 下，客户 A 的完整回答可以回填到 A 的匹配历史，不能注入客户 B。另有真实 HTTP/WS 测试验证伪造及 Header Override 无法覆盖 principal。

脚本保存源码摘要、CPA 二进制 SHA256、测试结果和脱敏案例，运行前后源码变化会让验收失败。进程、SQLite、配置、凭据均为临时合成数据，退出后清理；只保留指定目录中的证据。2026-09-30 的本地常规测试、针对终态/principal 的 race 检查及 JSX 语法检查均通过。

主要维护入口：`dto/responses_terminal.go`、`relay/common/responses_outcome.go`、`relay/channel/openai/responses_usage_estimate.go`、`relay/channel/openai/chat_terminal.go`。终态和用量逻辑集中于这些入口；适配器只接入协议观察结果，避免每条路径各自定义成功。

发布前仍需当前仓库 CI、固定镜像部署及受控业务验证，特别核对通道 143 的跨组件日志、真实用量和实际调用次数。本地合成用例不证明历史客户请求已恢复。
