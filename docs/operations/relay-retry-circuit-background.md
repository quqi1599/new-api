# Relay 多轮重试、渠道熔断与后台重连

## 行为边界

- 同步 Relay 在尚未向客户端输出时，可以在同模型兼容渠道之间切换。
- 每轮不重复已失败渠道；渠道耗尽后按指数退避进入下一轮。
- 总轮次、总尝试次数和总时间均有硬上限，避免上游整体故障时无限放大请求和费用。
- 上游明确返回可重试 HTTP 失败时允许换渠道，包括渠道端 `404/408/425/429/5xx`。请求结果未知、已经输出内容、Realtime、异步 Task 和 Midjourney 不盲目重放。
- 熔断状态按“渠道 ID + 模型”隔离。Redis 可用时跨实例共享；Redis 故障时退化为进程内状态。

默认重试边界：

```text
轮次上限：3
总尝试上限：min((RetryTimes + 1) * 3, 30)
文本总时间：180 秒
图片总时间：600 秒
轮间退避：500ms 起，最高 5 秒
```

每次请求的管理日志包含：

- `attempt_no`
- `attempt_round_no`
- `round_attempt_no`
- `distinct_channel_count`
- `retry_stop_reason`
- `use_channel`

## 渠道熔断

默认情况下，同一个渠道与模型在 60 秒内累计 3 次可归因于上游的失败后开路 30 秒。开路结束只放行一个半开探针：

- 探针成功：关闭熔断并清零失败状态。
- 探针失败：重新开路。
- 已经开路后才返回的旧在途失败不会续期当前开路窗口。
- 499、请求参数错误、普通本地错误不计入熔断。

自身已有多账号池、重试和熔断的聚合渠道可通过
`CHANNEL_CIRCUIT_BYPASS_CHANNEL_IDS` 跳过 NewAPI 外层熔断，多个渠道 ID
使用逗号分隔。此设置只应应用于已经具备内部保护的聚合渠道，普通直连渠道仍使用
上述熔断策略。

## 可选后台 Relay Job

后台模式不会改变现有接口的默认行为。调用原接口时增加：

```http
X-Oneapi-Background: true
Idempotency-Key: customer-operation-unique-id
```

提交示例：

```bash
curl https://example.com/v1/chat/completions \
  -H 'Authorization: Bearer ...' \
  -H 'Content-Type: application/json' \
  -H 'X-Oneapi-Background: true' \
  -H 'Idempotency-Key: order-123-chat-1' \
  -d '{"model":"gpt-example","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

服务返回 `202`：

```json
{
  "id": "task_bg_...",
  "object": "background_relay.job",
  "status": "queued",
  "status_url": "/v1/background/task_bg_...",
  "result_url": "/v1/background/task_bg_...?raw=1"
}
```

按偏移量增量重连：

```http
GET /v1/background/{job_id}?offset=0&limit=1048576&wait=25
Authorization: Bearer ...
```

JSON 返回中的 `data_base64` 是从 `offset` 开始的新数据，下一次使用 `next_offset`。`result_available` 表示当前实例或持久化记录中仍有可读结果。`wait` 最大 25 秒，用于长轮询。

原始结果读取：

```http
GET /v1/background/{job_id}?raw=1&offset=0
Authorization: Bearer ...
```

响应头包括当前 Job 状态、偏移量和是否完成。后台执行使用独立超时上下文，因此提交连接断开不会取消上游任务。

## 存储与限制

- 默认每个 Token 同时最多 3 个后台 Job，全局最多 32 个。
- 活跃结果默认最多在内存保留 128 MiB，完成后保留 60 分钟。
- 默认只把不超过 8 MiB 的最终结果持久化到 Task 表；大结果在进程内 TTL 到期后不再提供原文，避免把大图片 Base64 长期写入数据库。
- `Idempotency-Key` 按用户和 Token 隔离；相同键但请求路径、协议格式或请求体不同时返回 `409`。
- 后台重连是显式扩展协议。普通 OpenAI/Claude 客户端不会自动识别 Job ID，需要调用方接入提交与轮询流程。
- 当前执行器与活跃输出缓冲在进程内：客户端断线不会取消任务，但服务器进程重启会中断正在执行的 Job。已完成且不超过持久化上限的结果可在重启后继续读取。
- 多实例部署时，活跃 Job 的提交和增量读取需要保持在同一实例；在将 Job 队列和结果缓冲迁移到外部持久化存储前，应使用粘性路由。
- 如果上游在已输出部分内容后主动断流，系统不会把另一次生成拼接到旧流后面；这种情况会标记任务失败，避免重复工具调用、重复扣费或内容串流。
