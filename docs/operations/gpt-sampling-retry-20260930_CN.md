# GPT 采样参数拒绝的重试边界

## 技术契约

上游以结构化 `error.code=gpt_sampling_unsupported` 明确拒绝采样参数时，原样重复请求不能改变参数兼容性。仅依赖宽泛 HTTP 重试范围会使普通重试和 GPT 备用路径反复发送相同请求。

`ErrorCodeGPTSamplingUnsupported` 接入已有 `alwaysSkipRetryCodes`。普通重试和 GPT 强制备用均停止，保留原 HTTP 状态、机器码及参数说明。该规则匹配精确机器码，不按错误正文猜测，也不禁止所有 HTTP 422。

不自动删除采样参数，不降低推理要求，不声称被拒绝的参数已经生效。调用方应按所选模型的参数契约修正请求。

## 合成验证

- 真实 `Relay`、本地 HTTP 上游、临时数据库及 Redis 的负对照复现 A→B→A 重复调用；修复后只调用一次并以 `not_retryable` 结束。
- Chat/Responses、普通/流式、数据库/内存选路组合保留原错误说明，渠道保持启用。
- 状态映射和自定义宽重试范围不能重启该确定性错误；仅在正文提及同名字符串不会触发规则。
- 暂时不可用入口仍可由独立健康入口接管，已输出请求继续禁止重放。

相关 controller、types、operation_setting 测试与目标 race、vet 通过。此补丁防止重复调用，不会让不支持的采样参数变得兼容。
