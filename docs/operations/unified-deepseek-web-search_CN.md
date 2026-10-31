# 统一 DeepSeek 网页搜索与抓取

2026-11-01 用户授权实现、测试并上线。搜索与抓取均按官方人民币价 5 元/千次的五倍，即 **25 元/千次有效请求**，不再叠加模型、分组或用户分组倍率。模型 token 仍按原模型每一轮实际用量和价格计费。

## 执行位置

NewAPI 接管客户声明的服务端联网工具，原模型生成搜索词，CPA 新增的 `/v1/tools/search`、`/v1/tools/fetch` 经过现有认证、签名身份与内容审核，再固定调用 DeepSeek 官方接口。GPT/Claude 的回答模型没有替换。普通客户端 function 恰好叫 `web_search` 不会被平台接管。

覆盖入口：Responses 的 web_search / web_search_preview、Claude 的 web_search / web_fetch、Chat 的 web_search_options / enable_search、Gemini Google Search / URL Context，以及 Codex `/v1/alpha/search` 的 search_query / open / find / click。Alpha Search 只调用搜索服务，不额外生成模型回答。

不能由 DeepSeek Search/Fetch 提供的专用命令（例如图片搜索、PDF 截图、金融和天气工具）、缓存模式或无法保留的供应商扩展会明确报错，不回退到另一家收费搜索引擎。搜索专用模型需改用普通模型加 web_search；模型供应商或渠道参数不得在转换后重新开启原厂 hosted 搜索。

## 计费和交付

- Search/Fetch 各自保留 max_uses 与域名限制；域名过滤后没有可用结果时不扣搜索费，越界抓取在调用前拒绝。
- Search 的 results 非空、Fetch 有正文才计一次。空结果、错误和超时不收成功工具费；同一请求中不同有效操作分别计数，读取已保存页面的 find/open 不重复访问上游，不新增 Fetch 费用。
- 引用和跨轮工具历史存于 Redis，按 user_id/token_id 绑定，24 小时到期；未知、过期或其他调用者的引用明确拒绝。查询结果是工具数据，不提升为系统指令。
- 每轮模型调用使用原有适配器、渠道、审核和计费规则计算费用，所有轮次最后统一结算一次。已成功检索或已产生可计量模型用量后，不因后续失败重新执行整条请求。
- 多轮输出在服务端暂存，完成工具循环后按原 JSON/SSE 协议交付；等待期间每 15 秒发送 SSE 注释。单请求最多 16 轮模型、32 次工具尝试；并发 8；请求/模型响应/合并历史分别有 32 MiB 上限。
- 客户中途断开或后续模型失败，仍保存实际已经发生的 token 与工具用量，最终状态为 failed/cancelled；成功响应在余额结算后交付，不靠 HTTP 200 判断是否真正搜索。
- 费率固定记录人民币 25 元/千次及本请求的 USDExchangeRate；用户确认充值 40 元获得 $200 平台额度，故每 $1 额度实收 ¥0.2；实际五倍售价应扣 $125 平台额度/千次（每次 62500 quota），不能用显示用外汇率 7.3 换算。通过 DEEPSEEK_SEARCH_CNY_PER_CREDIT=0.2 固定并逐请求记录充值折算。旧日志保留原值，不追补历史扣费。整数 quota 只在整笔费用末尾舍入。账单的 `managed_web` 保存搜索/抓取次数、模型轮数、模型费、人民币单价、汇率、总 quota 和最终状态。
- 管理界面按最终记录展示 Search/Fetch 和模型费用，不再拿搜索价格乘分组倍率。原工具价格表保留作为功能关闭时的旧行为，统一功能启用后这些工具不从旧表结算。

## 启用和回退

NewAPI 环境：

```text
UNIFIED_WEB_SEARCH_ENABLED=true
DEEPSEEK_SEARCH_PROXY_URL=http://<现有 CPA 内网地址>:18317
DEEPSEEK_SEARCH_PROXY_KEY_FILE=/run/secrets/deepseek-search-proxy-key
DEEPSEEK_SEARCH_CNY_PER_CREDIT=0.2
```

密钥文件内容是现有 CPA 入口 Key，以只读文件挂载；不是 DeepSeek 上游凭据。CPA 容器配置 `CPA_WEB_TOOLS_ENABLED=true`、`CPA_WEB_SEARCH_GATEWAY_ID`、`CPA_WEB_SEARCH_KEY_ID`，指定现有加密存储中的官方渠道与 Key。上游凭据保持在 CPA，NewAPI 使用现有 `CPA_AUDIT_IDENTITY_SECRET` 签名客户身份。缺少凭据、审核签名或 Redis 时失败关闭。

先发布并验证 CPA 工具端点，再发布 NewAPI 并打开统一开关。每次发布保存原 Compose、运行镜像与相关配置摘要。回退关闭 NewAPI 的统一开关或恢复原镜像；CPA 新端点可随之关闭，现有生成接口和其他 Key 状态不变。禁止把同名第三方 DeepSeek 设为搜索执行者。

## 验证

本机用真实适配器、HTTP fixture 和临时账户数据库验证 Responses/Messages/Chat 的普通及 SSE、倍率 1/3、两轮 token 累计、一次工具费及钱包/Token/日志一致；另覆盖空结果、协议错误、工具历史、Redis 调用者隔离与过期、重复工具 ID、模型失败、存储失败、断开和最终退款幂等。CPA 测试覆盖官方凭据身份、认证、参数边界和固定目标。

实际发布完成状态，以本轮 CI、固定镜像、运行配置及客户入口测试回执为准。

官方规则：[DeepSeek 网页搜索与抓取](https://api-docs.deepseek.com/zh-cn/guides/search/)。
