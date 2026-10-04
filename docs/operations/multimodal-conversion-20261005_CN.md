# 多模态转换修复与验收

## 官方补丁来源

核验官方 `QuantumNous/new-api` 的 `main` 至 `1a4166d8e8ba9802d2ca56fe8ecf0ed5404e80d5`。
官方提交 `0ed497f066a68613375124303ef54f220267b334`（PR #7137，2026-09-01）
已在 `relaykit/dto/openai_request.go` 的 `Message.ParseContent` 增加
`[]MediaContent` 分支。本仓库仍使用旧目录布局，因此移植该独立逻辑到
`dto/openai_request.go`，保持现有代理、结算及失败终态实现。

## 问题与处理

- Responses→Chat 的图片/文件内容是 `[]MediaContent`，Claude重建Message后失去解析缓存。
  原解析器只接受 `[]any`，最终向上游发送空内容。采用官方补丁后复制消息也能保留媒体。
- 文件字段统一读取 `filename`，兼容旧 `file_name`；JSON解析保留完整文件字段，
  避免空file_id或不完整附件被解析器静默丢弃。
- Responses图片缺少可用image_url时明确400；Claude转换不能读取file_id、
  不支持的文件/媒体、非法内联编码和非UTF-8文本均明确拒绝，保留原文且跳过重试。
  file_id支持受原文件存储/权限约束，本次不宣称跨提供商文件ID可以直接读取。
- 上游明确413（JSON或HTML响应）保留错误并禁止同请求重试；状态映射不能重新打开重试。
  超大输入仍需客户端压缩或更换传输方式，不能保证超过上游限制的请求成功。
- 管理端单渠道测试增加可选 `test_mode=vision`，使用固定64×64红色PNG，
  只有正确回答颜色才通过。自动渠道测试保持既有默认请求。

## 图片专项测试

沿用现有管理端鉴权与权限边界：

`GET /api/channel/test/{id}?model={model}&endpoint_type=openai-response&stream=true&test_mode=vision`

支持 `openai`、`openai-response`、`anthropic` 三种端点，流式与非流式。
测试使用合成图片，不需要客户key或客户附件。HTTP200不等于图像验收通过，
必须同时检查JSON中的 `success=true`。测试会像原有内置测试一样记录合成用量/响应时间。

## 验证范围

- DTO复制后媒体保留、标准/旧文件名。
- 真实Responses→Claude适配器：图片、图片+文字、URL图片、工具结果图片、PDF、UTF-8附件。
- 普通Chat图片、原生Messages对照；无法解析的媒体400及SkipRetry。
- 真实Relay入口的数据库/缓存两种选路：413首次失败即结束、不触达下一渠道。
- 单渠道图片探针必须正确识别颜色，空回答或错误颜色不能通过。

生产扫描的报错摘要不得把BODY METADATA中的content_type误判成媒体错误。
日志中的相似错误是调查线索，未知正文不能被当成图片丢失根因证据。
提交、云端CI、固定digest发布与生产探针分别记录，文档本身不证明部署完成。
