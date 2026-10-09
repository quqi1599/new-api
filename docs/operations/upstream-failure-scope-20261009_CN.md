# 上游能力拒绝与输出校验失败的重试边界

## 能力拒绝：本次请求排除不兼容入口

NewAPI 的请求级排除集合新增三个精确机器码：`reasoning_budget_unsupported`、`reasoning_effort_unsupported`、`reasoning_extension_unsupported`。只有实际收到上游响应且 HTTP 状态为 `422` 时匹配。

该契约描述 CPA 在发送前拒绝无法保持思考控制语义的入口。请求可以尝试其他兼容入口，相同请求不能在后续轮次重新选择已拒绝的入口。不删除或降低思考参数，不将三类错误设为所有入口通用的终止错误，不修改全局渠道状态。

已输出、上游接收结果未知、取消、指定渠道、显式亲和性停止、尝试预算、状态归属及请求级不可重试约束继续生效。`reasoning_off_unsupported`、其他 422、错误正文中的同名字样及同码其他状态不纳入本次匹配。

## 输出校验失败：保留错误，不再次生成

以下五个精确机器码在原始错误状态为 `502` 时表示已经生成的结果未满足约束：

- `output_invalid_json`
- `output_schema_mismatch`
- `output_not_json_object`
- `output_stream_mismatch`
- `output_unknown_tool`

这些失败可能发生在向客户提交输出之前。NewAPI 设置 `skipRetry=true` 和 `allowChannelPenalty=false`，避免换入口再次生成，也避免把请求级格式失败计入整条渠道的自动禁用或熔断。保留原 HTTP 状态、错误码、类型及说明，后续配置中的状态映射不移除上述标志。

不放宽 JSON、schema 或工具校验，不改变 usage，不将错误包装成成功。普通 502/503、`native_response_conversion_failed` 和同机器码的原始 503 保留原策略。

## 合成验证

真实 Relay 加 SQLite、Redis 与本地 HTTP 上游夹具验证数据库和缓存路径：两个拒绝入口从重复六次收敛至各一次；单入口 extension 从三次收敛至一次。独立健康入口仍可完成，原思考参数保持不变。

Chat 与 Responses 非流式路径分别覆盖五个输出校验码：负对照会在其他入口第二次生成，修复后只调用一次并保留 502 及原说明。Responses SSE 在首事件和 `response.created` 之后均保留机器码、失败终态及 usage，禁止重试和渠道惩罚。

相关八包测试、四包目标 race、三包 vet 和差异检查通过；相邻状态归属、Grok、采样参数、选路及流式保护一并回归。

既有 `auth_not_found` 精确 503 契约继续按请求排除入口；该行为防止同入口重复调用，不补充上游认证。未知普通 503 不会因为这组规则被推断成认证池故障。
