# CPA 图片错误客户提示与终止分类

日期：2026-10-06。本轮仅本地修改，没有提交、推送或部署。

CPA 按标准错误信封返回中文 `error.message`，说明图片大小/数量/编码问题及压缩、减少历史附件、必要时新建对话的处理建议。NewAPI 保留该 message 并按客户的 Chat、Responses、Claude 协议返回，不依赖会被忽略的自定义 actions 字段。

图片容量问题可能来自 HTTP 413，也可能来自 HTTP-200 SSE 内的结构化错误，后者在流式适配器中会暂时包装成 502。`types/upstream_request_error.go` 现明确归一：

| 结构化错误码 | 内部状态 / 行为 |
| --- | --- |
| `request_too_large`、`image_too_large` | 413、归一错误码 `request_too_large`、禁止重试、禁止渠道惩罚、保留 message |
| `invalid_image_input` | 400、禁止重试、禁止渠道惩罚、保留 message |
| 其他代码的 message 仅出现这些词 | 不改变分类，防止任意模型文本控制选路 |

客户端已经收到内容时保持 SSE 和 HTTP 200，以流内错误事件终止；未提交输出则由控制器返回正常 413/400 JSON。不能通过状态归一重写已经发送的响应头，不能重放已接受请求或清零可靠 usage。

新增类型回归及控制器的两渠道隔离 SQLite 集成测试。测试覆盖三种客户端格式、普通/SSE、已输出后的流内错误，故意将自动重试状态范围设为 400–599；仍只有一次合成上游请求，中文处理建议送达，渠道保持启用，没有成功流终态。

相关源码：`types/upstream_request_error.go`、`types/media_error_test.go`、`controller/cpa_media_customer_error_test.go`。CPA 对应本地说明：`cpa-new/docs/media-customer-errors-20261006_CN.md`。这不是现网客户路径验证，未修改生产重试范围或渠道设置。
