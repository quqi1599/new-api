# Embeddings 错选 Messages 渠道修复

## 线上证据

2026-10-28 11:17:50–11:47:50（UTC+8）的 NewAPI 日志中，48 条 `convert_request_failed` 全部为 `/v1/embeddings` 请求选中了 141 Anthropic 类型渠道：`deepseek-v4-pro` 24 条，`deepseek-v4.1-flash` 与 `deepseek-v4-flash` 各 12 条。固定错误为 `500, not implemented`；记录显示正文完整，但没有连接上游或写出请求。

原因是 Embeddings 请求没有进入现有的严格端点能力筛选，而 Anthropic 适配器的 `ConvertEmbeddingRequest` 未实现。模型出现在渠道模型列表中，不等于该渠道协议能处理 Embeddings。[Anthropic 官方文档](https://platform.claude.com/docs/en/build-with-claude/embeddings)也明确其不提供自有嵌入模型。

## 本地改动

- `/v1/embeddings`、旧 `/v1/engines/:model/embeddings` 路径的选路及 Embedding relay format 携带已有 `EndpointTypeEmbeddings`，复用缓存、数据库、亲和与重试选路中的端点门控。
- 当前已确认不支持 Embeddings 的 Anthropic 渠道在选路时排除；其他渠道继续由各自适配器处理，不按模型名猜测或扩大能力承诺。有已配置的兼容渠道时，可以选用该渠道；没有时保留现有无可用渠道错误。
- 显式指定 Anthropic 渠道，在选择 key 和计费前返回 `400 / request_feature_unsupported`，提示选择兼容的嵌入模型和路由。直接调用适配器的防御路径也返回此类型错误，经过转换错误包装仍保留状态码与停止重试语义。

旧 `/v1/engines/:model/embeddings` 原有 Gemini 请求解析未改；本次只补其选路筛选，不宣称它已完整兼容标准 OpenAI 嵌入正文。能力排除只针对已确认的 Anthropic 适配器，未完成全 provider 嵌入支持矩阵。

没有将聊天模型输出伪造成向量、没有替换请求模型、没有改动生产渠道或凭据。旧日志中 OpenAI 入口另有 Embeddings 404；本次修复错误候选选择与错误500，不宣称这些 DeepSeek 聊天模型已支持 Embeddings。

## 本机验证

新增四组回归覆盖入口及重试能力要求、缓存/数据库高优先级不兼容候选排除与兼容候选耗尽、显式指定渠道的选 key 前拒绝、适配器错误类型保留。旧生产逻辑的 overlay 反例在四组均失败；修复后四个相关包的定向回归通过。types、model、middleware、Claude 适配器、service、controller 六个包的全量测试通过；检查日志保存在本次任务记录。

本次仅本地修改与验证，尚未提交、推送或部署。
