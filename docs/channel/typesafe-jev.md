# TypeSafe Jev 原生决策接口

本实现移植自社区 [PR #7443](https://github.com/QuantumNous/new-api/pull/7443)（Jiiiin）及 [PR #7462](https://github.com/QuantumNous/new-api/pull/7462)（lanyinzly），并适配本仓库的渠道筛选、请求存储、计费和 Semi UI 管理端。协议依据为 [TypeSafe 官方 API 文档](https://docs.typesafe.ai/api)。不包含第三方 Jev 社区站的包装协议。

## 配置

- 渠道类型：`TypeSafe / Jev`（64，保留官方其他渠道的编号空间）。
- API 地址：`https://api.typesafe.ai`。
- 密钥：TypeSafe 官方控制台签发的 API Key。
- 模型：`jev-latest`、`jev-preview`、`jev-1.13.0`。支持现有模型映射。
- 决策接口路径：留空使用 `/v1/systemone`。OpenRouter 的 API 地址为 `https://openrouter.ai/api`，路径为 `/alpha/decisions`，模型名按其公开列表映射。
- 计费：在管理端显式设置模型价格。按官方输入单价配置 `tier("standard", p * 0.042)` 即每百万输入 token 0.042 美元、输出免费；接入前核对官方最新价格。已有模型和分组价格不自动改动。

## 客户端

三个入口使用同一套鉴权、模型权限、渠道选择和限流：

- `POST /v1/decisions`
- `POST /v1/systemone`（原生 SDK 兼容）
- `POST /typesafe/v1/systemone`（官方插件路径兼容）

```sh
curl "$NEWAPI_BASE_URL/v1/systemone" \
  -H "Authorization: Bearer $NEWAPI_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"jev-latest","state":{"sky":"blue"},"questions":{"blue":{"type":"noul","instructions":"Is the sky blue?"}}}'
```

请求使用 `state` 和 `questions`，直接返回上游 `model`、`answers` 和 `usage`。支持 `noul`、`choice`、`score` 以及官方定义的结构化说明；显式零值和嵌套 `false` 保持不变。不支持聊天接口或流式调用。

## 校验与结算

输入参数在请求前校验，参数覆盖后再次校验。响应必须包含匹配的问题答案、合法概率和整数 token 用量，才作为成功返回并结算；缺失或异常用量会失败并退回预扣，不使用估算量替代真实用量。由于上游没有幂等保证，决策调用失败后不会自动重放；客户端自行重试是独立调用。

回归测试覆盖三种问题、结构化 criteria、选项数量边界、模型映射、三个入口、权限拒绝、参数覆盖、错误响应、预扣退款、消费日志以及上游地址约束。真实调用测试使用 `JEV_TEST_API_KEY_FILE` 指向受限凭据文件，默认跳过，不把凭据写入仓库。
