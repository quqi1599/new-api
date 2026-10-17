# Gemini 图片输入 token 映射：本轮未移植

本轮仅移植官方 PR #6371 的两个正式模型名与默认 `TEXT+IMAGE` 行为。曾在本机试验的 Gemini 输入 `IMAGE` token 映射已撤回；当前 Gemini 用量统计和计价行为保持本轮前状态，没有生产修改。

## 最小反例与实际结果

Gemini 输入总量为 130，其中图片 115、文本 15、缓存总量 25。缓存可能与图片重叠，不能把图片和缓存当作两个互斥集合。Google 的 `promptTokenCount` 包含缓存内容，`cacheTokensDetails` 用于描述缓存中的各模态。

将候选 mapper 产生的用量交给本地现有计价参数函数：

```go
usage := &dto.Usage{
    PromptTokens: 130,
    PromptTokensDetails: dto.InputTokenDetails{
        ImageTokens: 115,
        TextTokens: 15,
        CachedTokens: 25,
    },
}
params := service.BuildTieredTokenParams(usage, false, map[string]bool{
    "img": true,
    "cr": true,
})
// Actual: P=0, Img=115, CR=25; P+Img+CR=140, although PromptTokens=130.
```

2026-10-16 在仓库外 Go overlay 中对候选 mapper 运行 `TestReviewGeminiImageCacheOverlap`，实际失败输出：

```text
overlap counted twice: prompt=130 P=0 Img=115 CR=25 sum=140
```

留存：[实际测试函数](gemini-input-image-overlap_test.go.txt)、[实际失败输出](gemini-input-image-overlap-output.txt)。测试源码作为文本证据保存，不加入日常回归来锁定当前未采集图片输入的行为。

原因是现有函数分别减去 `cr` 和 `img`，剩余 `p` 为负时夹为 0；本例因此将 140 个 token 分配到计价项。之前只启用 `img` / `img_o` 的用例不能证明同时使用缓存变量时安全。

## 官方来源及缺失依赖

- [Google GenerateContent UsageMetadata](https://ai.google.dev/api/generate-content#UsageMetadata)：`promptTokenCount` 包含缓存；`cacheTokensDetails` 描述缓存模态。
- 固定官方源码提交：`d032204733db31208f10bac118b6e683aab3bf29`。
- [官方 Gemini canonical mapper](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/relaykit/dto/billing_usage.go#L330-L383) 会采集输入 `IMAGE`，但该提交的 Gemini DTO 和 mapper 没有接收、转换 `cacheTokensDetails`。
- [官方计价参数拆分](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/service/tiered_settle.go#L26-L109) 仅在表达式使用 `img_cr` 且存在有效 `CachedTokensDetails.ImageTokens` 时拆出图片缓存。Gemini mapper 不提供这一信息；已有 `img` + `cr` 表达式也不会自动进入该分支。
- [官方表达式兼容说明](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/pkg/billingexpr/expr.md) 明确保留未引用 `img_cr` 的原有归一化行为，因此不能把整个新架构当作该 Gemini 问题已经解决的证据。

因此，单独移植输入 `IMAGE` 字段不足以安全支持现有客户计费。本轮撤回该 mapper、顺带模态大小写归一化及只验证该 mapper 的测试，不修改全局计费策略或费率。后续若处理此项，应先定义缓存与图片重叠、缺失缓存明细和已有表达式的兼容策略，再独立验证。

## 本轮保留内容

[PR #6371](https://github.com/QuantumNous/new-api/pull/6371)（合并提交 `8b41defbe0d9752bd7bf60aac82480e58ded51bc`）新增 `gemini-3-pro-image`、`gemini-3.1-flash-image`，并让默认 Chat 转换附带 `responseModalities=["TEXT","IMAGE"]`。原生 `generateContent`、Imagen `predict`、普通文本和既有 preview 模型保持原有行为。

用户已持久化的完整 `SupportedImagineModels` 自定义名单不会被默认值覆盖；名单不含正式模型名时，需对应配置后才会自动附带 `TEXT+IMAGE`。
