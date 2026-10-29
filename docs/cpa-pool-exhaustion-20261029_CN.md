# CPA 零调用同池耗尽协同

2026-10-29。用户授权合并并部署本次聚合轮询及其 NewAPI 配套改动。

可信 CPA 的 HTTP503 响应提供单值 `X-CPA-Pool-Exhausted: capacity-v1`，且错误类别为 model_cooldown/key_suspended/auth_unavailable 时，当前请求排除同 CPA 服务的其他协议入口；独立备用保留。声明表示 CPA 对全部配置目标确认入口无关的容量阻断，实际上游调用为0，不伪造尝试次数。已有已输出/接受结果未知的禁止重放规则优先。

运输层验证管理员 CPAAuditIdentityEnabled 和完整同源响应来源，信任信息绑定本次HTTP响应并在重新核验时清除；客户/覆盖配置的保留头在出站前剥离。重复头、未知版本、不可信来源及能力错误不采纳。状态码映射保留已验证语义。不修改渠道启停、额度和长期熔断状态。

本机 service/types/controller/relay/channel 回归与 vet 通过；真实本机CPA进程+NewAPI控制器HTTP集成覆盖普通/流式和有无独立备用，共4组。发布复用main的Trusted CI和固定提交签名生产镜像；先NewAPI后CPA。回滚仅镜像，保留实时数据库；旧NewAPI忽略新头后恢复原来的入口重试行为。
