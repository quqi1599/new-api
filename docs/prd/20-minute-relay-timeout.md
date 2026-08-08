# PRD：NewAPI 全链路 20 分钟业务等待预算

## 1. 文档状态

- 状态：待评审、待实施
- 日期：2026-08-09
- 适用项目：`new-api`
- 目标环境：生产 NewAPI、所有实际承载 NewAPI 流量的 Caddy/OpenResty 入口和区域中继
- 不包含：DNS/GTM 权重调整、渠道账号迁移、数据库迁移、支付或配额规则调整

## 2. 背景

生产曾使用 480 秒上游响应头超时，后调整为 520 秒。上线后仍出现精确 520 秒的 504，日志证明请求已经连接并完整写入上游，但未收到任何响应头。

本需求不是消除上游卡死，而是把“客户可等待的完整业务预算”提高到 20 分钟，同时保证：

1. NewAPI 必须早于外层反向代理结束请求。
2. 超时必须由 NewAPI 返回可分类的 504，不能由 Caddy 抢先返回通用 504。
3. 不能恢复首事件前心跳，避免提前提交 HTTP 200。
4. TCP 建连、TLS 握手、单次轮询等短阶段超时不能机械放大到 20 分钟。
5. 客户端断开仍应记录为 499；上游阶段超时必须记录为 504。

## 3. 产品目标

### 3.1 核心目标

- 流式请求在收到首个有效事件前，共享最多 1200 秒请求级预算。
- 非流式请求从入站、上传、等待响应头到读完响应体，共享最多 1200 秒预算。
- 首个有效流式事件到达后，允许最长 1200 秒无有效数据的 idle 间隔。
- 外层反向代理提供 1260 秒预算，为 NewAPI 返回错误和连接收尾保留 60 秒。
- 所有生产入口的超时层级一致，不允许某个区域入口仍保留 600 秒并提前截断。

### 3.2 非目标

- 本需求不承诺上游一定在 20 分钟内成功。
- 本需求不允许对已经写入上游、结果未知的 POST 请求进行无条件重放。
- 本需求不把 TCP、TLS、心跳或单次轮询改成 20 分钟。
- 本需求不修改 DNS、GTM、证书、路由权重或渠道密钥。

## 4. 超时层级

必须满足：

```text
TCP/TLS 阶段超时
< NewAPI 响应头超时 1140s
< NewAPI 请求级总预算 1200s
< Caddy/OpenResty 外层预算 1260s
< 建议客户端总超时 1320s
```

任何实现不得把 NewAPI 与 Caddy 设置为相同截止时间。

## 5. NewAPI 参数要求

### 5.1 生产必须显式设置

生产不得只依赖代码默认值，Compose 或受控环境文件中必须显式配置：

```dotenv
RELAY_TIMEOUT=0
RELAY_DIAL_TIMEOUT_SECONDS=10
RELAY_TLS_HANDSHAKE_TIMEOUT_SECONDS=10
RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS=1140
RELAY_EXPECT_CONTINUE_TIMEOUT_SECONDS=1
RELAY_FIRST_EVENT_TIMEOUT_SECONDS=1200
RELAY_FIRST_EVENT_TOTAL_TIMEOUT_SECONDS=1200
RELAY_NON_STREAM_TIMEOUT_SECONDS=1200
RELAY_PRE_FIRST_EVENT_HEARTBEAT_SECONDS=0
RELAY_STREAM_HEARTBEAT_SECONDS=15
STREAMING_TIMEOUT=1200
ALI_TASK_POLL_REQUEST_TIMEOUT_SECONDS=60
BACKGROUND_RELAY_TEXT_JOB_TIMEOUT_SECONDS=1260
BACKGROUND_RELAY_JOB_TIMEOUT_SECONDS=1260
```

### 5.2 参数语义

| 参数 | 目标值 | 语义 |
|---|---:|---|
| `RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS` | 1140 | 单次尝试等待上游响应头；必须比请求级总预算早 60 秒 |
| `RELAY_FIRST_EVENT_TIMEOUT_SECONDS` | 1200 | 响应头后等待首个有效事件的单次上限，实际受请求级绝对截止时间缩短 |
| `RELAY_FIRST_EVENT_TOTAL_TIMEOUT_SECONDS` | 1200 | 从入站开始、跨尝试共享的首事件总预算 |
| `RELAY_NON_STREAM_TIMEOUT_SECONDS` | 1200 | 非流式上传至读完响应体的绝对总预算 |
| `STREAMING_TIMEOUT` | 1200 | 首个有效事件后，相邻有效事件的 idle timeout，不是流的总时长 |
| `BACKGROUND_RELAY_TEXT_JOB_TIMEOUT_SECONDS` | 1260 | 后台文本任务外层收尾预算 |
| `BACKGROUND_RELAY_JOB_TIMEOUT_SECONDS` | 1260 | 其他后台 Relay Job 外层预算 |
| `RELAY_STREAM_HEARTBEAT_SECONDS` | 15 | 仅首个有效事件后发送心跳 |
| `ALI_TASK_POLL_REQUEST_TIMEOUT_SECONDS` | 60 | Ali 单次轮询请求上限，完整任务仍受 1200 秒总预算约束 |

### 5.3 保持不变的安全参数

以下参数不得改成 1200 秒：

| 参数 | 保持值 | 原因 |
|---|---:|---|
| `RELAY_DIAL_TIMEOUT_SECONDS` | 10 | 黑洞地址或代理建连失败必须快速结束 |
| `RELAY_TLS_HANDSHAKE_TIMEOUT_SECONDS` | 10 | TLS 卡死不应占用 20 分钟连接 |
| `RELAY_EXPECT_CONTINUE_TIMEOUT_SECONDS` | 1 | 仅用于 `100-continue` 协商 |
| `RELAY_IDLE_CONN_TIMEOUT` | 90 | 连接池空闲连接生命周期，不是业务请求预算 |
| `RELAY_STREAM_HEARTBEAT_SECONDS` | 15 | 心跳间隔，不是超时 |
| `ALI_TASK_POLL_REQUEST_TIMEOUT_SECONDS` | 60 | 单次 poll 必须短；总体可运行 1200 秒 |
| `RELAY_RETRY_MAX_ELAPSED_SECONDS` | 180 | 不扩大文本请求重试风暴 |
| `RELAY_RETRY_IMAGE_MAX_ELAPSED_SECONDS` | 600 | 不因总等待预算增加而扩大重复图片任务风险 |

### 5.4 必需代码修改

1. 将上述默认值、`.env.example`、Compose 示例、README 和配置测试同步更新。
2. 新增 `TASK_POLLING_REQUEST_TIMEOUT_SECONDS=1200`，替换当前任务轮询内部 600 秒硬上限；轮询 cycle 与单任务请求必须共享同一个绝对截止时间，不能每次重新获得 1200 秒。
3. 动态识别为 SSE 的响应必须继承入站时刻建立的 1200 秒首事件绝对截止时间。
4. AWS、Coze、Xunfei、Volcengine、任务轮询、供应商鉴权、媒体预处理和 WebSocket 首事件路径不得重新起算 1200 秒。
5. 首个有效事件到达后解除首事件绝对截止时间，仅保留 1200 秒 idle timeout。
6. 首事件前禁止写入 PING、注释或任何响应体字节。
7. 请求已写入且结果未知时保持 `skipRetry`，除非对应供应商链路具有可验证的幂等键。
8. 启动时校验并警告以下错误配置：
   - 响应头超时大于或等于请求级总预算；
   - 请求级总预算大于或等于已知外层代理预算；
   - 后台任务外层预算小于 Relay 内层预算。

## 6. Caddy 和区域入口要求

### 6.1 生产源站 Caddy

只修改反向代理到 `127.0.0.1:3000` 的 NewAPI 路径。不得顺带修改图片服务 `:8080`、静态站点或其他服务。

所有 NewAPI `reverse_proxy` 应使用：

```caddyfile
reverse_proxy 127.0.0.1:3000 {
    flush_interval -1
    transport http {
        dial_timeout 19s
        response_header_timeout 1260s
        read_timeout 1260s
        write_timeout 1260s
    }
}
```

覆盖范围至少包括：

- `gpt-agent.cc`
- `api.llm-token.cn`
- `token.gpt-agent.cc` 的 `/api` 与 `/v1`
- `quota.llm-token.cn` 的 `/api` 与 `/v1`
- 其他实际复用同一 NewAPI upstream 的域名

### 6.2 美国、香港及其他 Caddy 中继

所有当前启用且可能被 GTM 或区域入口选中的 Caddy 节点必须同步：

```caddyfile
lb_try_duration 1260s

transport http {
    # 保留节点现有短连接值，不统一放大
    dial_timeout <保持当前值>
    response_header_timeout 1260s
    read_timeout 1260s
    write_timeout 1260s
}
```

上线前必须从当前 GTM/路由状态重新生成节点清单；不得只按历史 IP 列表修改。

### 6.3 OpenResty/Nginx 入口

若实际流量经过 OpenResty/Nginx，NewAPI 相关 location 必须同步：

```nginx
proxy_connect_timeout 19s;
proxy_send_timeout 1260s;
proxy_read_timeout 1260s;
send_timeout 1260s;
```

保持 SSE buffering 关闭。不得修改无关网站或静态资源 location。

### 6.4 客户端要求

- 官方示例和内部调用方建议总超时至少 1320 秒。
- 客户端若在 1200 秒前主动断开，NewAPI 应记录 499，而不是伪装成上游 504。
- 不承诺无法配置长超时的第三方客户端支持完整 20 分钟等待。

## 7. 错误语义和日志要求

### 7.1 上游响应头超时

在约 1140 秒结束，并记录：

```text
HTTP 504
error_code=upstream_response_header_timeout
timeout_phase=response_headers
timeout_seconds=1140
cancel_origin=upstream_timeout
connected_upstream=true
request_written=true
response_headers_received=false
output_started=false
```

### 7.2 首事件总预算耗尽

在入站后最多 1200 秒结束，并记录：

```text
HTTP 504
error_code=upstream_first_event_timeout
timeout_phase=first_valid_event_total
timeout_seconds=1200
output_started=false
```

### 7.3 非流式总预算耗尽

```text
HTTP 504
error_code=upstream_non_stream_total_timeout
timeout_phase=non_stream_total
timeout_seconds=1200
```

### 7.4 下游断开

- 客户端主动断开继续返回/记录 499。
- 不计入上游渠道健康故障。
- 不得改写成 504。

## 8. 验收标准

### 8.1 自动化测试

必须使用缩短后的测试时钟覆盖：

1. 上游永不返回响应头：NewAPI 先于外层代理返回 typed 504。
2. 响应头接近总预算才到达：剩余首事件预算正确缩短，不能重新获得 1200 秒。
3. 首事件在总预算内到达：解除首事件 deadline，健康长流不被 1200 秒总时长截断。
4. 首事件后超过 idle timeout 无有效数据：明确流式 idle timeout。
5. 非流式响应体卡死：在请求级绝对 1200 秒预算结束。
6. 动态 SSE、WebSocket、AWS、Coze、Xunfei、Volcengine 和任务轮询不重置预算。
7. 首事件前 writer 未提交，超时能真实返回 HTTP 504。
8. 调用方 deadline 更早时，保持 caller deadline/499 或既定 504 语义。
9. Caddy 配置通过 `caddy validate`；OpenResty/Nginx 配置通过 `-t`。
10. 全量 Go 测试、超时核心 race 测试和前端构建通过。

### 8.2 生产验收

1. 容器 revision、镜像 digest 与 CI SHA 一致。
2. `restart=0`、OOM=false，无 panic/fatal。
3. 所有实际入口 `/api/status` 返回 200；未认证 `/v1/models` 返回预期 401。
4. 使用受控假上游验证 1140 秒 NewAPI 504，禁止拿真实付费渠道故意挂满测试。
5. 504 日志包含正确 phase/seconds/stage，且 `output_started=false`。
6. 至少完成一条真实流式成功请求，验证首帧、持续输出和正常结束。
7. 观察 24 小时和 72 小时的连接数、FD、内存、goroutine、499/503/504、各渠道首帧分位和超时并发。

## 9. 发布顺序

必须按以下顺序，避免外层仍为 600 秒时先部署 1200 秒内层：

1. 只读确认当前生产 SHA、镜像、Compose、环境变量、全部实际入口和回滚点。
2. 备份各入口 Caddy/OpenResty 配置。
3. 先把所有外层代理预算从 600 秒提高到 1260 秒；逐台 validate、reload、健康检查。
4. 完成代码修改、测试、commit、push、最终 SHA CI 和不可变镜像签名。
5. 在生产部署锁内显式写入 NewAPI 环境变量，并只重建 `new-api`。
6. 执行生产验收和 24/72 小时观察。

## 10. 回滚顺序

若出现资源压力、错误率上升或代理异常：

1. 先回滚 NewAPI 镜像和环境变量到上一生产版本。
2. 验证旧 NewAPI 在更宽的 1260 秒外层下正常工作。
3. 再逐台把 Caddy/OpenResty 从 1260 秒恢复为原 600 秒。
4. 验证所有入口和真实业务请求。

不得先把外层降到 600 秒再保留 1200 秒 NewAPI，否则会重新产生代理先超时。

## 11. 风险与停止条件

把等待时间从约 9 分钟提高到 20 分钟会增加并发连接、文件描述符、goroutine、内存和上游悬挂请求占用。它不会修复上游卡死。

满足任一条件应停止扩大发布并回滚：

- 容器发生 OOM 或 restart。
- FD 使用率超过安全上限，或相对基线持续增长。
- 内存、goroutine 或并发连接相对基线增长超过 30% 且不回落。
- 499、502、503、504 或无可用渠道比例明显高于发布前基线。
- 任一区域入口仍在 600 秒提前结束。
- 出现首事件前 HTTP 200、超时被包装为 500、或超时阶段元数据缺失。

## 12. 业务结论

20 分钟是等待策略，不是上游可用性修复。对于历史上成功首帧通常在几十秒内、但偶发长时间无响应头的渠道，仍应继续建设上游账户/代理熔断、路由隔离和幂等安全换路，不能依靠无限延长超时解决。
