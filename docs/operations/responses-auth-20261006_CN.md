# Responses 入口鉴权兼容与诊断

## 技术契约

- `TokenAuth` 只从明确、非空的 `openai-insecure-api-key.` WebSocket 子协议项提取凭据。普通协议名称、空的密钥子协议和错误前缀不覆盖已有 Authorization；明确子协议凭据保留原有优先级。
- Bearer scheme 按大小写无关规则解析，兼容已有 raw token、`mj-api-secret` 回退及管理员指定渠道后缀，不改变令牌权限。
- 拒绝时增加带 Request ID 的 `token_auth_rejected` 固定原因：`missing_api_key`、`invalid_api_key`、`token_disabled`、`token_expired`、`token_exhausted`。额外字段只表示 Authorization 或显式 WebSocket 凭据是否存在，不记录密钥、密钥哈希、令牌名称或请求正文。
- 对外继续返回原鉴权错误，不披露令牌状态。停用、过期、额度耗尽及无效令牌仍拒绝。

## 合成验证

使用真实 Gin 中间件、临时 SQLite 和合成凭据，负对照复现有效 Authorization 被无关 WebSocket 子协议覆盖。修复后覆盖 GET/POST、大小写 scheme、明确子协议优先级、空项和错误前缀、不可用令牌状态及旧凭据来源回退。

断言 Request ID 存在、最终处理器只在鉴权成功时执行，公开错误与内部日志均不含合成密钥、用户名或正文。中间件测试、目标 race 与 vet 通过。

历史通用鉴权错误不能仅凭这些解析缺陷推断具体原因；固定原因诊断用于精确区分后续拒绝。

参考：[RFC 9110 §11.1](https://www.rfc-editor.org/rfc/rfc9110.html#section-11.1)。
