# Image 2 与 Banana 官方兼容改动（2026-10-16）

本轮按用户要求核查 NewAPI 官方仓库，把当前仍有效的生图兼容改动移植到本地架构。保留之前的 CPA 配套修复和其他未提交工作；没有整体合入官方主分支，没有提交、推送、部署或调用收费模型。

## 来源与取舍

官方固定为 [QuantumNous/new-api `d032204733db31208f10bac118b6e683aab3bf29`](https://github.com/QuantumNous/new-api/commit/d032204733db31208f10bac118b6e683aab3bf29)。本地基于 `fec0c1c0dd0b1a5a3815a626da768f2afb41d4c2`，转换和计费结构与官方新 RelayKit 不同，因此按功能移植并验证。

| 官方改动 | 本轮处理 |
| --- | --- |
| [PR #4608](https://github.com/QuantumNous/new-api/pull/4608)，`d2576ddc`：Images 流式生成与编辑 | 接入请求中的 `stream`、可重复读取的编辑表单、图片事件转发及客户端返回格式适配；使用本地已有请求接受、流终态与一次结算机制 |
| [PR #7410](https://github.com/QuantumNous/new-api/pull/7410)，`e2bb05f7`：输出图像 token 明细 | 把图片输出明细传入既有用量结构；不调整费率或另加收费项 |
| [PR #6371](https://github.com/QuantumNous/new-api/pull/6371)，`8b41defb`：Gemini 图像模型正式名称 | 补 `gemini-3-pro-image` 和 `gemini-3.1-flash-image` 的模型声明及默认图像识别，聊天转换可按既有规则携带 `TEXT` / `IMAGE` 输出类型 |
| 官方当前 `relaykit/dto/billing_usage.go` 的 Gemini 输入图像统计 | 未单独移植，原因是它与缓存用量存在重叠，当前计费表达式不能安全区分，见下文 |

官方当前 OpenAI 模型列表没有单独列 `gpt-image-2`；本轮验证显式配置该模型时的通用 Images 路径，没有据此宣称官方模型发现列表已完整支持它。未合并的 [PR #4481](https://github.com/QuantumNous/new-api/pull/4481) 不作为 Codex 图像工具已支持的证据。

Banana 接入 `/images/generations`、`/images/edits` 的旧 [PR #2305](https://github.com/QuantumNous/new-api/pull/2305)、[PR #2321](https://github.com/QuantumNous/new-api/pull/2321) 后来被 [PR #2340](https://github.com/QuantumNous/new-api/pull/2340)、[PR #2338](https://github.com/QuantumNous/new-api/pull/2338) 撤回，本轮不移植撤回方案。Banana 继续使用已有的原生 Gemini 或聊天转换入口；Imagen 的 `:predict` 行为保留。大图片流缓冲及 HEIC/HEIF 等本地已有能力不重复实现。

## 请求、返回与重复生成

Images 请求的 `stream` 区分缺省、显式 `false` 和 `true`；编辑请求保留图片文件、mask、模型映射及原有参数。JSON 与 SSE 的适配按客户端请求决定，不能仅因上游返回 SSE 就改变客户端期望格式。

原生图片流接入本地流状态检查。完整终态、部分图片、缺失终态、读取错误和客户端断开分别处理；保留已确认用量，不能把 EOF 或没有图片的完成事件当成功，也不能在已经接受生成或输出图片后再请求另一渠道生成。请求数量上限 128、固定计价数量只乘一次等本地约束保留。部分预览不算完整图片；异常时按张计价只计算已经确认完成的图片，token 计价保留实报用量。

累计用量更新保留缺失字段，明确上报的零值可以覆盖旧值；不把各帧累计快照反复相加。图片汇总、完成图数、SSE 数据行和用量元数据分别设上限，防止上游不断追加导致内存无界增长。JSON 写给客户失败也按断开处理，不记成成功。

另补一项本地保护：Gemini 图像模型可能把只有文字的 Chat 请求转换为生图请求。响应解码恢复因此也检查原始及有效模型、选中渠道的模型映射和图像模型配置，不把这类请求误当成可以重复生成的普通文本。

## 用量与配置

本轮补 OpenAI 图片输出 token 明细，不修改默认价格、倍率、阶梯表达式或生产配置。现有规则若单独使用 `img_o`，会从输出总量中扣除对应子类；未使用时仍保留在普通输出总量中。实际金额仍由当前配置规则决定。

Gemini 的输入图像明细不能照搬：Google [UsageMetadata](https://ai.google.dev/api/generate-content#UsageMetadata) 说明输入总量包含缓存，并另给 `cacheTokensDetails`；本地及核查的官方 Gemini DTO 没有采集这组缓存模态。本地现有表达式同时使用 `img` 与 `cr` 时，输入 130、图像 115、缓存 25 会产生归一后的 0 + 115 + 25 = 140，超过原输入总量。官方新架构虽另有图片缓存变量，也不能自动修正缺失该明细的 Gemini 结果。因此本轮撤回仅在工作区试加的输入图像映射，保留原行为；没有为移植一个字段擅自改变客户的计费表达式。[实际反例与未移植原因](../reviews/2026-10-16-image-compat/gemini-input-image-deferred_CN.md)已保存。

新增 GA 名称只修改默认 `SupportedImagineModels`。用户已保存的自定义完整列表继续有效，不强制加回默认名称；该列表不含 GA 名称时，自动附加图像输出类型仍需相应配置。

## 验证范围

本轮以合成 HTTP 服务核验请求体、编辑文件、SSE 事件、失败重放和用量，不使用真实客户图片或收费上游。

- 分段 HTTP 验证上游先发送预览、等待下游收到后才发完成图，确认是真流式转发；覆盖 JSON/SSE 两个方向、生成/编辑、三种 `stream` 取值、可重读 multipart 和明确零值。
- 12 个 Images HTTP＋本地 SQLite 钱包场景检查一次上游调用、一次消费日志、部分失败的实际张数、客户端断开、重复完成事件、实报零值，以及重复结算/退款不改变结果。
- 7 个服务层账本场景验证真实 token、原单位价格、固定张费、token 计价和冻结的阶梯表达式不混用。
- 34 个隐式生图恢复场景验证图片不重复生成、普通文本仍可恢复、检查不修改原请求、渠道映射、价格或请求接受状态。
- Image 2、Gemini、controller、service/common 的相关普通测试、定向并发检查和静态检查通过；独立复核发现的问题均补有通过的反例。

整仓测试还发现上轮 Claude 断开修复关联的 AWS 旧断言，已仅更新该测试，并补充 `499/client_gone`、不惩罚上游、不重试、关闭流和不伪造完成/用量的检查；没有为匹配旧断言改生产分类。随后 `go test ./... -count=1 -timeout=180s` 全部通过，共 46 个有测试的包。所有本轮修改通过 `git diff --check`、Go 格式和文档本地链接检查。

全仓 `go vet ./...` 仍报告本轮未修改文件中的旧问题：`common/custom-event.go` 和 `model/log_migration_test.go` 复制锁、八个旧适配器的不可达代码；已用与 HEAD 的比较确认这些文件未改。本轮涉及的 9 个实现包静态检查通过，不能将其写成全仓静态检查通过。

本地验证不代表生产已经更新或所有供应商接受这些模型与参数；未提交、推送或部署。
