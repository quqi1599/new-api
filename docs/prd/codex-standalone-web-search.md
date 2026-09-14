# PRD：NewAPI 支持 Codex Standalone Web Search

## 1. 文档状态

- 状态：待评审、待实施
- 日期：2026-08-16
- 优先级：P0 客户兼容性修复
- 适用项目：`new-api`
- 关联上游：CLIProxyAPI（CPA）
- 目标接口：`POST /v1/alpha/search`
- 不包含：Responses 内嵌 `web_search` 工具、Codex 远程压缩、模型能力声明修改、DNS/GTM/Caddy 切流

## 2. 背景

客户使用以下 Codex 配置接入 `https://gpt-agent.cc/v1`：

```toml
web_search = "live"

[model_providers.proxy_http]
supports_standalone_web_search = true
```

普通 `POST /v1/responses` 请求可用，但 Codex 发起独立联网搜索时请求：

```http
POST /v1/alpha/search
```

当前生产返回：

```text
HTTP 404
Invalid URL (POST /v1/alpha/search)
X-New-Api-Version: sha-67b6a8966e35
```

当前证据表明：

1. 404 由公网 NewAPI 产生，请求尚未进入 CPA。
2. 生产 NewAPI SHA `67b6a8966e35` 未注册 `/v1/alpha/search`。
3. 直连当前生产 CPA 的同一路径返回认证错误而非 404，说明 CPA 已注册该接口。
4. NewAPI 官方仓库已经实现 Alpha Search 路由、渠道适配和工具计费，但当前生产分支未包含相关实现。

因此，本需求是补齐以下链路：

```text
Codex
  -> POST gpt-agent.cc/v1/alpha/search
  -> NewAPI 鉴权、限流、选路和计费
  -> CPA /v1/alpha/search
  -> ChatGPT Codex /backend-api/codex/alpha/search
  -> 原样响应 Codex
```

## 3. 问题定义

Standalone Web Search 不是普通 Responses 请求中的 hosted tool，而是 Codex 单独发起的一套 HTTP 协议。仅支持 `/v1/responses`，或者仅在请求体里允许 `web_search`，均不能代替 `/v1/alpha/search`。

当前 NewAPI 将该请求落入 `NoRoute`，所以无论 CPA 和上游是否支持，客户端都会得到 404。修改 Codex 模型名、Base URL 尾部斜杠或重复开启 `web_search = "live"` 都不能解决服务器缺少路由的问题。

## 4. 产品目标

### 4.1 核心目标

1. NewAPI 正式支持 `POST /v1/alpha/search`。
2. 请求复用现有 API Key 鉴权、令牌权限、模型权限、分组、RPM 限流和渠道分发能力。
3. 只向明确支持 Alpha Search 的渠道转发，禁止误发给普通 OpenAI-compatible 渠道。
4. 请求体除必要的模型映射外保持原样，响应体保持透明，不进行 Responses 协议转换。
5. 成功请求形成可审计的调用日志和一次 Web Search 工具计费。
6. 使用真实 Codex 完成“发起搜索—获得搜索结果—继续正常对话”的闭环。

### 4.2 非目标

- 不把 `/v1/alpha/search` 宣称为标准 OpenAI Responses API。
- 不为所有默认 OpenAI 渠道自动开启该能力。
- 不通过把 standalone search 改写成 `/v1/responses` hosted tool 来模拟兼容。
- 不修改 CPA 已有 Alpha Search 实现。
- 不在本需求中处理 Codex 远程压缩或 `/v1/responses/compact`。
- 不修改生产 DNS、GTM、Caddy、渠道密钥或流量权重。

## 5. 用户场景

### 5.1 主要场景

用户在 Codex 中启用：

```toml
web_search = "live"
supports_standalone_web_search = true
```

当模型调用 `web.run` 时，用户应看到正常的联网搜索过程和结果，不再出现 `/v1/alpha/search` 404。

### 5.2 兼容场景

- 普通 `/v1/responses` 请求行为不变。
- Responses 请求体内的 hosted `web_search` 行为不变。
- 未开启 standalone web search 的 Codex 用户不受影响。
- 不支持 Alpha Search 的渠道仍可承载其他请求，但不得被本接口选中。

## 6. 功能需求

### 6.1 路由与中间件

在现有 `/v1` Relay 路由组注册：

```http
POST /v1/alpha/search
```

该路由必须复用：

- `RouteTag("relay")`
- 系统性能门禁
- Token 鉴权
- 模型请求频率限制
- 渠道分发
- 请求 ID 和访问日志

未携带或携带无效 NewAPI Token 时，应返回现有统一的 401，不得越过 NewAPI 直接访问 CPA。

### 6.2 独立 Relay 类型

新增独立的 Relay Format/Mode，例如：

```text
RelayFormatOpenAIAlphaSearch
RelayModeAlphaSearch
```

不得复用 `RelayModeResponses` 或 `RelayModeResponsesCompact`，避免进入错误的请求转换、流式响应解析和 token usage 结算路径。

### 6.3 请求校验与透明转发

最低校验要求：

- 请求体必须是合法 JSON。
- `model` 必填且非空。
- 请求体遵循现有全局 Body Size 门禁。

转发要求：

1. 保存原始 JSON 请求体。
2. 没有模型映射时，按原始字节转发。
3. 存在模型映射时，只修改顶层 `model`，保留 `id`、`commands`、`settings`、`input` 以及未来新增的未知字段。
4. 不删除、重命名或自行解释 `commands.search_query` 等 Alpha Search 私有字段。
5. 入站 Authorization 不得原样泄漏给上游；使用选中渠道自身的认证信息。

### 6.4 渠道能力和选路

首期允许以下渠道类型承载 Alpha Search：

- Codex
- NewAPI
- Sub2API
- 已明确配置对应端点的高级自定义渠道

默认 OpenAI 渠道保持不支持，除非后续增加默认关闭的显式能力开关并完成单独评审。

请求进入分发前应排除不兼容渠道，避免先选中普通 OpenAI 渠道再返回 500。若同一模型同时存在兼容和不兼容渠道，只能在兼容渠道集合内执行既有优先级、分组和冷却策略。

生产上线前必须确认指向 CPA 的渠道类型属于上述允许范围；不得为了绕过能力校验而临时改变其他渠道语义。

### 6.5 上游路径映射

| 渠道类型 | 上游路径 |
|---|---|
| CPA / NewAPI / Sub2API | `/v1/alpha/search` |
| Codex OAuth | `/backend-api/codex/alpha/search` |
| 高级自定义 | 使用管理员显式配置的 Alpha Search 端点 |

本次生产目标链路为 NewAPI 转发至 CPA 的 `/v1/alpha/search`。

### 6.6 响应处理

- 上游 2xx 状态、`Content-Type` 和响应体应透明返回。
- 不把 Alpha Search 响应转换成 Chat Completions 或 Responses 对象。
- 不删除 `output`、`encrypted_output` 或未来新增的未知字段。
- 上游非 2xx 使用现有错误处理和状态码映射，但不得伪装成成功响应。
- 客户端断开、上游错误和 NewAPI 本地拒绝必须在日志中可区分。
- 不为该接口增加独立的短超时；沿用当前 Relay 生命周期和取消语义。

### 6.7 计费

Alpha Search 成功响应通常不提供标准 token usage，因此按工具调用计费：

1. 每个最终成功的客户端请求记录一次 `web_search_preview` 工具调用。
2. 工具单价使用系统现有可配置 Tool Price，不在代码中写死金额。
3. 只有成功返回上游 2xx 时完成结算；本地校验失败、渠道不兼容、认证失败和上游非 2xx 不向用户收取成功搜索费用。
4. 日志应显示模型、渠道、工具名、调用次数、实际扣费和请求 ID。
5. 同一客户端请求即使发生安全的选路重试，也只能形成一次成功结算。

上线前由运营确认生产 `web_search_preview` 单价；若单价未配置或异常，发布必须停止，不能默认免费放行。

### 6.8 重试安全

Alpha Search 是独立 POST 请求，不能假定无副作用或上游免费。

- 请求尚未发送上游时，可因渠道能力不匹配重新选路。
- 已连接且请求可能已写入上游后，默认不得跨渠道自动重放。
- 只有存在可验证的幂等 ID 和明确的上游去重语义时，才允许扩大重试范围。
- 日志必须区分 `not_sent`、`request_written` 和 `response_received`，避免隐藏重复调用风险。

## 7. 安全与隐私要求

- 不在日志中记录完整查询内容、原始请求体、上游响应体、API Key、OAuth Token 或 Cookie。
- Debug 日志也不得输出完整 Alpha Search JSON；只允许记录大小、模型、命令数量等非敏感摘要。
- 上游错误返回需经过现有敏感信息清理。
- 请求体大小、响应读取和并发数沿用现有生产安全门禁。

## 8. 可观测性

每次请求至少记录：

```text
request_id
relay_mode=alpha_search
model
mapped_model
channel_id
channel_type
upstream_path
status_code
duration_ms
request_written
response_received
billed_tool=web_search_preview
billed_call_count
```

建议增加以下统计：

- `/v1/alpha/search` 请求数和成功率
- 401、400、404、429、5xx 分布
- 各渠道耗时 P50/P95/P99
- `channel_not_supported` 数量
- 客户端断开数量
- 成功请求数与计费工具调用数差异

## 9. 验收标准

### 9.1 自动化测试

必须覆盖：

1. `/v1/alpha/search` 路由已注册，不再进入 `RelayNotFound`。
2. 无 Token 返回 401；无 `model` 或非法 JSON 返回 400。
3. 原始请求中的未知字段可无损到达上游。
4. 模型映射只修改 `model`。
5. NewAPI 渠道转发至上游 `/v1/alpha/search`。
6. Codex 渠道转发至 `/backend-api/codex/alpha/search`。
7. 默认 OpenAI 渠道不会被 Alpha Search 选中。
8. 上游 2xx 响应体及关键字段原样返回。
9. 上游非 2xx 不产生成功工具计费。
10. 成功请求只计一次 `web_search_preview`。
11. 普通 `/v1/responses`、`/v1/responses/compact` 和 hosted web search 回归测试通过。
12. 服务端构建、相关 Go 测试和完整 CI 通过。

### 9.2 集成测试

使用测试 Token 完成：

```text
Codex 测试客户端
  -> 测试 NewAPI
  -> 测试 CPA
  -> Codex OAuth 测试账号
```

验收必须同时证明：

- NewAPI 收到 `/v1/alpha/search`。
- NewAPI 选择了允许的 CPA 渠道。
- CPA 收到并转发该请求。
- 上游返回成功。
- Codex 将响应识别为一次成功 `web.run`。
- 搜索结束后 Codex 能继续使用搜索结果生成回答。
- NewAPI 只生成一条成功调用日志和一次工具费用。

单独 `curl` 返回 HTTP 200 不能替代真实 Codex 闭环。

### 9.3 生产验收

1. 生产 SHA、不可变镜像标签和 digest 一致。
2. 实际运行容器 revision 包含 Alpha Search 实现。
3. 未认证请求从原 404 变为预期 401。
4. 使用受控测试 Token 完成一次真实 Codex 搜索。
5. 普通 Responses 对话和远程压缩各完成一次回归请求。
6. 容器无新增重启、OOM、panic 或异常 5xx。
7. 发布后观察 Alpha Search 成功率、计费一致性和重复请求至少 24 小时。

## 10. 发布策略

### 10.1 实施原则

- 以 NewAPI 官方现有实现为行为基线，但按当前生产分支结构进行最小移植。
- 不直接机械 cherry-pick 大型上游提交；官方相关提交包含工具计费、Sub2API 和协议层重构，需避免覆盖当前分支的 `RelayDispatch`、后台中继、超时和计费定制。
- 新实现必须适配当前生产分支已有的工具计费和请求生命周期。

### 10.2 发布步骤

1. 确认当前生产 SHA、CPA 渠道类型、工具单价和回滚镜像。
2. 在任务分支完成最小实现和定向测试。
3. 合入最新生产基线，形成单调升级候选 SHA。
4. 对最终 SHA 执行完整 CI。
5. 构建并推送 `linux/amd64` 不可变镜像。
6. 串行更新 NewAPI 服务，不修改 CPA、Caddy、GTM 或数据库。
7. 完成分层生产验收和真实 Codex 闭环。
8. 记录生产 SHA、镜像 digest、验证结果和回滚点。

### 10.3 回滚

出现以下任一情况立即停止扩大影响并回滚 NewAPI 镜像：

- 普通 Responses 请求出现回归。
- Alpha Search 发生重复计费或明显重放。
- 请求体/响应体被错误转换，Codex 无法识别。
- 渠道选路泄漏到不兼容上游。
- 新版本产生持续 5xx、panic、OOM 或容器重启。

回滚只恢复部署前 NewAPI 不可变镜像，不修改 CPA、数据库、渠道凭据或网络配置。

## 11. 风险与待确认项

| 项目 | 风险 | 上线前动作 |
|---|---|---|
| CPA 渠道类型 | 若配置为默认 OpenAI，官方能力白名单会拒绝 | 只读确认实际渠道类型 |
| 工具单价 | 上游响应无 usage，可能漏费或误费 | 确认 `web_search_preview` 生产价格 |
| 自动重试 | POST 重放可能造成重复上游调用 | 锁定请求写入后的 skip-retry |
| 分支差异 | 当前生产分支与官方 main 长期分叉 | 采用定向移植，不覆盖现有定制 |
| 协议演进 | Alpha Search 是 Codex 私有接口，字段可能变化 | 原样透传未知字段，增加真实客户端回归 |

## 12. 上游参考

- [NewAPI Issue #6114：Codex CLI 内置联网搜索返回 404](https://github.com/QuantumNous/new-api/issues/6114)
- [NewAPI 官方 Alpha Search 实现提交](https://github.com/QuantumNous/new-api/commit/2d23cdf2915432632e37637198a72c752d642bcf)
- [NewAPI PR #6156：feat: codex alpha search](https://github.com/QuantumNous/new-api/pull/6156)
- [NewAPI PR #6263：support codex alpha search relay](https://github.com/QuantumNous/new-api/pull/6263)
- [NewAPI Issue #6490：默认 OpenAI 渠道转发请求未采纳](https://github.com/QuantumNous/new-api/issues/6490)
- [CLIProxyAPI Issue #4166：gpt-5.6 standalone search](https://github.com/router-for-me/CLIProxyAPI/issues/4166)
- [Sub2API PR #4063：转发 Alpha Search 独立端点](https://github.com/Wei-Shaw/sub2api/pull/4063)

