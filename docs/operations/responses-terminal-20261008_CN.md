# Responses 失败终态修复（2026-10-08）

## 问题与修改

已确认原生 Responses 的 `response.failed` 被定制分支改成普通 `error`，Codex 无法从该形状识别原失败终态，最后显示缺少 `response.completed`。

- 已开始输出时，原生 `response.failed` 原样转发一次，保留 response ID、sequence_number、usage 与未知扩展，继续向控制器返回失败以记录错误并禁止重试。
- 普通流内错误、取消和缺终态 EOF 等需要网关生成错误事件时，使用 `response.failed` / `response.status=failed` / `response.error`。
- 生成失败事件复用已发送 response ID；序号使用已知序号的下一值，避免浮点重编码。没有已知 ID 时不虚构；序号溢出时不伪造下一值。
- 已发送终态后不再追加失败终态。写出失败遵循已有下游写错误分类。
- 失败中的实际用量保留给调用链，失败退款与收费策略保持既有逻辑；没有根据失败内容估算新的收费。
- 首次写出前仍返回普通 HTTP 错误；已输出后不重试、不在 SSE 后追加普通 JSON。

## 验证与发布

回归覆盖原生失败的精确内容及大整数、已报告用量、顶层/嵌套错误、取消、EOF、首包前错误、response ID、事件序号、单次终态与下游写失败。发布使用现有自建 CI、绑定提交 SHA 的 GHCR 镜像、Compose 备份和线上版本/业务验证；具体发布结果记录在本次发布证据中。

参考：[NewAPI 官方原事件转发](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.41/relay/channel/openai/relay_responses.go)、[CLIProxyAPI 3522e481](https://github.com/router-for-me/CLIProxyAPI/commit/3522e481aa7baa68f47cb712d7c0f90919b69bf0)、[CLIProxyAPI 25913086](https://github.com/router-for-me/CLIProxyAPI/commit/259130863d4bc4a781b07b2e36e2ba70bf82cfb5)。
