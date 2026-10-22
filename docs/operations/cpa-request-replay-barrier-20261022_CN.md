# CPA 请求级禁止重放（2026-10-22）

本次为本地代码修复，未提交、推送或部署，不修改账务、渠道设置和生产配置。

旧 `X-CPA-Retry-Scope: gateway` 只排除同一 CPA 模型池，允许独立备用继续。CPA 已观察到输出或不能确定已接受生成的结果时，需要停止整个逻辑请求，新增合同为：

```text
X-CPA-Retry-Scope: request
X-CPA-Retry-Reason: upstream_output_observed
X-CPA-Retry-Attempts: 1
```

Reason 只接受 `upstream_output_observed`、`accepted_outcome_unknown`。后者表示结果未知，不表示已产生正文或已经收费。Attempts 必须是 1–1048576 的规范正整数；三项字段都必须单值。未知、重复、合并、缺失或越界字段不触发新规则。

## 信任与执行位置

- 只处理本次选中渠道 `cpa_audit_identity_enabled=true` 的真实响应，复用既有 CPA 身份渠道设置。不能从渠道名称、客户同名请求头或 JSON 推断信任。
- HTTP 传输层在适配器读取正文前采集 metadata，覆盖非 2xx 错误、2xx SSE 首帧前错误和错误体截断；状态映射不能擦除已采集的停止原因。
- 原始请求和实际响应来源必须完整，响应重定向的每一跳都与原 origin 相同。缺少 Request/Response.Request/URL 时不采信；不新增响应签名或更换既有传输信任。
- 最终出站剥除所有 `X-CPA-Retry-*` 请求头，阻止客户端或渠道头覆盖经回显伪造合同。每次 attempt 和 HTTP 调用前清空旧值。
- Relay 的最终安全判断在 GPT 强制 fallback 之后执行。合法信号使同池别名、独立备用、下一轮全部停止，日志 `retry_stop_reason` 使用同名固定原因。
- 成功响应仍按成功处理；已发流沿用已有输出后禁止重试规则。明确输出前拒绝仍按 `gateway` 合同允许独立备用。

2026-10-22 本轮只读配置核对确认：启用渠道 141/142/143 已具有 CPA 审计身份标识，本次不需要为它们修改生产配置。该结果是检查时快照，发布前仍按目标渠道读回确认。不得为不可信第三方渠道随意启用标识来接受其响应头。

## 本机回归

`go test ./service ./controller ./relay/channel -run 'Test(GatewayRetry|GatewayReplayBarrier|RelayErrorHandlerGateway|CPARetry|StripCPA|GatewayRetryScope|GatewayRetryExhaustion|RelayRetry|ShouldRetry|CPAState)' -count=1` 通过。

新增测试覆盖严格解析、无信任渠道、来源缺失、同源/跨源跳转、客户请求头反射、逐 attempt 清理、HTTP 429/502、首帧前 SSE 错误、截断响应，以及明确拒绝仍成功切换独立备用。相关 `-race` 定向检查也通过。

真实本机 CPA 进程的 `controller/cpa_no_replay_integration_test.go` 共 8 场景通过（普通/流式各 4）：观察到输出后失败与已接受坏流均只有 1 次 CPA、1 次实际上游、0 次备用；去掉合同的负控产生 2 次 CPA、2 次实际上游后才调用 1 次备用；明确 429 拒绝仍在 CPA 遍历 2 个 Key 后调用 1 次独立备用并成功。所有请求使用合成内容和测试凭据。

只读故障样本为 6 个逻辑请求、7 次 CPA 内部已停止而 NewAPI 仍续试的调用。此前日志没有保留首次输出屏障的具体触发事件，因此不据此声明重复扣费或工具重复执行。

## 发布与回滚

两端更新后才形成完整保护。可先更新 NewAPI，再更新 CPA；经过的代理必须保留这三个响应头。验证应关联逻辑请求、CPA request_id、内部 attempt 和最终停止原因，确认后续渠道没有被调用。

回滚对应代码版本即可，不涉及数据库迁移或渠道配置。单端回滚或代理丢头会恢复修复前的跨层信息缺口，需要重新验证停止行为；不能把健康检查或 HTTP 响应头视为业务成功证明。
