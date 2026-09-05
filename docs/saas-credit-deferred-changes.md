# SaaS 授信以外的独立后续变更

日期：2026-09-06。本文定义后续评估范围，不授权合并、迁移或部署。

**本轮不引入通用缓存重构、官方原子消费整套方案或持久日志投影。** 当前变更只处理 SaaS 原子幂等授信、旧 grant 的授信缓存交错，以及必要的局部兼容保护。额度操作账本是到账凭证；原 `LogTypeTopup` 保持尽力记录，不能把日志成功与额度成功混为一谈。

## 本轮必要的明确改额兼容字段

`SaaSQuotaRevision` 属于授信与既有明确改额之间的必要兼容范围，不属于下文的通用缓存或消费重构。它只在已授信 Token 的 `Token.Update` 明确写余额时，与该次 DB 更新在同一条 SQL 中单调递增；普通消费、batch flush 和退款不递增此版本。更新后读取带版本的 DB 快照，缓存通过独立 edit floor 拒绝较旧编辑快照：同编辑版本仅叠加新增授信差额，较新编辑版本采用新余额并计入快照之后已送达的授信差额；UsedQuota 沿用本 fork 的 DB 回填口径。

引入该字段是为了区分“授信累计值未变，但余额被明确修改”与普通读取。仅 DEL 缓存、同步 fresh reload 或缩短 TTL 都不能保证两个并发明确编辑不会乱序覆盖；TTL 只能限制错误持续时间。该兼容字段不替代官方完整的元数据 mutation fence、用户 AuthVersion 或原子消费方案，也不证明禁用、删除和所有 metadata 写入已经具有统一版本协议；这些仍在后续变更 A 中独立评估。

## 固定的研究基线

官方参考固定为 `QuantumNous/new-api` 的 `eb99ab1b40343c3317bb47981cccdbb2b159a5fa`，不使用浮动 main 分支。本轮只读检查的是该 SHA 的本地官方源码导出，未运行其代码，也未把它合入本 fork。

| 已核对的官方源码 | 已确认的配套关系 | 对后续变更的要求 |
| --- | --- | --- |
| [model/quota_reserve.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/quota_reserve.go) | `TryReserveUserQuota` 在缓存命中时先原子扣 Redis，再持久化/进入 batch；失败时补偿，Redis 异常或无法水合时退回 DB 条件更新。Token 原子预扣同时维护 `RemainQuota` 与 `UsedQuota`。 | 这是消费权威和失败恢复边界的变化，不能只复制 Lua 或替换一个缓存函数。 |
| [model/token_cache.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/token_cache.go) | `cacheInitToken` 只初始化冷缓存；暖缓存仅续期。元数据修改通过独立 fence 阻止旧读回填，固定 fence 时间为 10 秒。 | 暖缓存不覆盖规则依赖所有消费字段在缓存中同步维护。必须实测 DB 读到回填的最长间隔与 fence 生命周期。 |
| [model/token.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/token.go) | Token 读取、修改、删除、批量失效和配额增减共同接入上述缓存/增量机制。 | 评估入口闭包，覆盖管理员、自助管理、批量操作、授信、退款和删除，不能仅审一个 helper。 |
| [model/user_auth_cache.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/user_auth_cache.go)、[model/user_cache.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/user_cache.go) | 用户鉴权采用 AuthVersion、pending fence、committed floor，并区分是否写配额字段。 | 本 fork 已有 generation、epoch 与 dirty fallback；先逐项对照，不能叠两套版本机制或丢掉现有禁用/撤销保护。 |
| [model/utils.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/model/utils.go) | batch delta 有溢出处理，用户 quota/used quota/request count 合并持久化。其代码仍使用进程内队列；不能据此推断崩溃零丢失。 | 与预扣、退款补偿、flush 失败、重启及多副本同时审查。 |
| [service/quota.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/service/quota.go)、[service/billing_session.go](https://github.com/QuantumNous/new-api/blob/eb99ab1b40343c3317bb47981cccdbb2b159a5fa/service/billing_session.go) | Token 预扣、funding 预扣、结算增量、部分成功与退款状态相互关联。 | 需要业务调用链验收，模型层单元测试通过不能代替一次完整账务闭环。 |

## 后续变更 A：通用缓存与原子消费整体配套评估

目标是减少缓存错误余额、并发超扣和错误拒绝，并明确 Redis/DB 故障时的结果。先比较两种完整设计：保留本 fork 的 DB 权威钱包预扣并完善派生缓存；或在单独设计中评估官方缓存优先路径的收益与恢复成本。官方代码是候选实现证据，不代表缓存优先方案已被选定。

### 必须保留的 fork 行为

- `model.ReserveUserQuota` 的 DB 条件扣减及现有调用链。任何后续评估默认保留它，不能为了对齐官方绕过或删除；更换权威边界必须有另行审查的设计和迁移证明。
- Token RPM 配置与优先级、模型限制、IP 限制、分组与跨组重试、受保护渠道限制、自定义管理接口。
- 管理员所有者 Token 的仅 Token 授信语义、SaaS 排除规则与原通用管理员 grant 的差异、累计授信 watermark、稳定操作 ID、30 亿以上授信及负余额恢复。
- 用户禁用/撤销的 generation/epoch/dirty fallback，以及流式、WebSocket、异步任务、订阅等业务预扣和补偿要求。

### 需要一起评估的范围与依赖

1. 画清用户/Token 的余额读取、预扣、最终结算、退款、授信、手工改额、禁用、删除、缓存回填和 batch flush 的全部写入路径。先统一每个余额字段的权威、版本和重放规则。
2. 区分 DB 读取快照与明确管理操作。授信累计量不是消费版本，也不是手工修改版本；禁止仅凭同一授信 watermark 判定整个 Token 快照仍然有效。
3. 明确 `RemainQuota`、`UsedQuota`、总授予额度的关系。当前 fork 的普通缓存扣减只改变 RemainQuota，因此不能单独移入官方“保留暖缓存 UsedQuota”策略；官方对应路径会同步更新这两个字段。
4. 设计缓存缺失、过期、Redis 重启、网络分区、事务确认丢失、DB 回填晚到的行为。需要明确操作已应用、已拒绝和结果未知的区别，不能用当前余额推断某笔操作是否完成。
5. 界定 batch 队列的持久性和恢复能力。进程内队列不是持久账本，flush 失败或主进程退出后的行为必须单独验证，不能通过扩大缓存锁的范围假装解决。
6. 给出多副本升级顺序、可读写版本兼容矩阵和回滚边界。混跑旧整对象回写与新版本协议必须有证据支持，不能默认为安全。

### 必要验收

| 场景 | 验收要求 |
| --- | --- |
| 两个并发消费请求，余额仅够一个 | 成功请求数、Token/账户扣款与上游工作一致；保留 DB 钱包不透支边界。 |
| pending debit/refund 与新旧授信交错 | 授信不重复，消费不丢；batch flush 前后 Remaining/Used/Total 各自符合确定的口径。 |
| 冷缓存夹在旧 grant 的 DB 提交与缓存通知之间 | 不能出现 DB 150、缓存 200 的重复授信。 |
| 旧读 → 明确改额/禁用 → 旧读回填 → 新读 | 旧快照不得永久锁住错误余额，也不得重新启用已禁用 Token；验证值而非仅验证 cache miss。 |
| Redis 故障/恢复、多副本、DB commit acknowledgement 丢失 | 补偿和重放不重复资金动作，过期本地标记可以恢复，没有无期限错误拒绝。 |
| 进程在预扣、持久化、退款和 batch flush 的各阶段退出 | 清楚证明可恢复结果，明确任何未承诺的损失窗口。 |
| 定制功能与性能 | RPM、分组、IP、模型和受保护渠道规则不回退；对比请求延迟、DB/Redis 往返、锁等待、连接占用及内存，不能只凭健康检查判定完成。 |

验证矩阵至少覆盖 SQLite/MySQL/PostgreSQL、Redis 开/关、batch 开/关、单副本/多副本；MySQL 没有真实实例就明确标记未验证。边界与故障测试通过之后才进行小流量生产候选评估，部署是另一个动作。

## 后续变更 B：持久日志增强

目标是额度成功后充值历史可最终补齐，同时不增加重复授信。这与资金原子提交可独立交付，本轮保留原尽力日志。

独立设计需要包括：主账本中的可恢复投影意图、LOG_DB 幂等去重、投影成功确认、失败可观测、有界独立恢复、保留期限、历史日志迁移与回滚。若使用 LOG_DB 去重表，其记录必须与日志在同一 LOG_DB 事务提交；主账本确认失败不能再次创建日志。不能把日志写入包进资金事务，也不能让 GET/回调承担无界补写。

必须提前确定日志库能力：PostgreSQL 日分区表不能只靠 operation ID 全局唯一索引；SQLite/MySQL/PostgreSQL 可评估独立小型去重表；ClickHouse MergeTree 的事务/唯一约束能力不同，应选择可证明的投影或读取方案，不能宣称自动具有相同的一次写入保证。

依赖和验收项：

- 明确首次日志、历史补投影和管理员手工记录的边界；日志保持原 Token/用户授信文案与可见性，凭证中不存明文密钥。
- 独立 LOG_DB 故障不能改变额度结果；日志提交后主库确认失败、重复消息及多副本并发最终只产生一个可见事件。
- 修改用户名/Token 名、日志保留期到期、分区缺失、更换 LOG_DB、迁移及恢复备份时，去重与历史显示仍按既定策略工作。
- 恢复每批条数、总执行时间、退避、告警、停止与关闭行为均有界；不能为了日志补齐拖延订阅额度重置等无关维护工作。
- 延迟评估覆盖健康日志库和长时间故障；用户请求不承担持续重试的开销。

## 旧候选不能直接应用

曾保存的扩大候选位于本工作区 `.audit/saas-credit-revision-20260906/rejected-source/`，其身份记录在相邻 `identity.json`：基线 `6bf46eaafdd2c97c66be507052da683deac5002b`，候选内容 SHA-256 `0a45fa6bfa3ccddb9d45fc39a10bfa21a65b94e65f236b5ceea4f4f2f0af1011`。它只是被拆出的研究材料，不能直接 apply/cherry-pick，也不能把它的旧测试结果当作当前缩减实现的验收。

已确认的设计问题包括：旧 grant 的异步缓存增加与冷读取可重复授信；全局暖缓存不回填与本 fork 当前消费实现不配套；同授信版本整对象回填会覆盖未落库消费；长期保留 UsedQuota 需要同步双字段消费作为前提；仅删除缓存而没有修改版本无法防住旧读跨过明确编辑重新填充。日志候选还把新迁移、后台恢复和请求补投影绑定到本次授信，引入无关维护延迟与日志库兼容范围。

后续变更应从各自问题和固定官方基线重新设计、提取必要部分、保留 fork 行为，并用新的差异和验收记录审查。**本文不把这些独立变更加入本轮交付。**

独立日志草稿已提取至 `.audit/saas-credit-revision-20260906/deferred-log-enhancement.patch`，配套状态见 `deferred-log-enhancement.json`。它只包含日志投影、迁移、恢复入口及相应测试初始化，未应用到当前源码；仅验证 patch 能适用于当前候选，不代表设计或运行验收通过。通用缓存候选仍保留为已拒绝材料，不能把已知错误重新包装为可合并补丁。
