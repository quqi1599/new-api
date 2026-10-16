# NewAPI 官方实现对照（2026-10-16）

结论：官方已有部分相关功能，但当前官方主分支仍未解决本次审计中的关键反例，不能直接以一次升级替代本轮修复。本次没有移植整套官方大重构，也没有把参考相同转换方向说成已移植官方补丁。

## 固定版本与方法

本地 `official` remote 和 README 均指向 `QuantumNous/new-api`，`origin` 是用户的 fork。核查固定到[官方提交 d032204733db31208f10bac118b6e683aab3bf29](https://github.com/QuantumNous/new-api/commit/d032204733db31208f10bac118b6e683aab3bf29)，提交时间 2026-10-16 06:45:17 UTC。通过 GitHub API 读取代码、PR 状态、文件记录，并下载固定提交到仓库外临时目录；没有更新本地 Git 引用，没有 merge、提交或生产操作。

官方根模块使用 Go 1.26，转换逻辑已迁入独立的 `github.com/QuantumNous/new-api/relaykit` 模块，DTO、类型、转换 registry、诊断与计费用量结构均已移动。本地 fork 仍使用 Go 1.25.1 及现有 `dto` / `service/openaicompat` / provider adapter 边界。因此整体引入 relaykit 不属于一个可直接替换的小补丁。

## 官方已有的部分

| 主题 | 官方当前证据 | 本次如何处理 |
|---|---|---|
| Chat→Responses JSON Schema 映射 | [PR #2876](https://github.com/QuantumNous/new-api/pull/2876) 已合并，merge 为 `f18aec52819d1a671c928f8399e3a76ba0c75477`；当前[转换函数](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/relaykit/relayconvert/internal/oai_chat/to_oai_responses_req.go)仍以 `map[string]any` 处理 Schema | 本地原先已经有该转换；保留现有流程，只补 RawMessage 精度和不可表示扩展的明确拒绝 |
| Responses→Chat 格式映射 | 当前[转换函数](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/relaykit/relayconvert/internal/oai_responses/to_oai_chat_req.go)已读取 text.format；[PR #5825](https://github.com/QuantumNous/new-api/pull/5825)已合并转换 registry 重构，merge 为 `c36418c8632912377010a903bdcc9672dde1c22d` | 本地补齐这条已有转换方向，但不直接照搬其会舍入数字、丢未知字段的 map 解码；不是移植整个 #5825 |
| Claude effort 与 output_config 合并 | 当前[shared/claude/reasoning.go](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/relaykit/relayconvert/internal/shared/claude/reasoning.go)修改 effort 时会合并原配置；[PR #4318](https://github.com/QuantumNous/new-api/pull/4318)仍 open、未合并，主要是自定义 output_config/task_budget 透传 | 本地沿用“只改 effort、保留其余字段”的方式，以 RawMessage/sjson 保持 Schema 数字精度；未声称移植 #4318 |
| Claude 每条 message 的 output_config | [PR #7561](https://github.com/QuantumNous/new-api/pull/7561)已合并，merge 为 `c2b7a9a9e0b548c2051a949fceabb59029adcb49` | 本地已有对应 DTO 和回归；这不等于顶层结构化格式已保留，不重复实现 |
| 通用响应终态枚举 | 官方[StreamStatus](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/relay/common/stream_status.go)已有 completed/failed/incomplete/cancelled | 官方 Claude adapter 的 message_stop 仍直接 MarkCompleted，未把 max_tokens 映射为后台 incomplete；本地仅补本轮缺口，保留已有用量字段 |
| Responses 流缺少终态用量估算 | [PR #7427](https://github.com/QuantumNous/new-api/pull/7427)已合并，merge 为 `55a6cd2a44eaf189bbd565dd2cdfa591d19b06d6` | 本地原先已有基于它的适配；本轮不重写该计费策略，CPA 占位零/未报告需额外按 cpa_usage 区分 |
| 完成后 context 取消的健康统计 | [PR #7638](https://github.com/QuantumNous/new-api/pull/7638)已合并，merge 为 `3f926c3ef49745be273cd8f753fb691abdd7b836` | 处理已 completed 且正常结束之后的取消，不能修复 max_tokens 先被错误记录 completed 的问题 |

另检查了 [PR #5002](https://github.com/QuantumNous/new-api/pull/5002)：closed、`merged_at=null`，不能把其标题或自动摘要当成官方已发布修复。

## 在官方原代码上的合成反例

在固定源码的独立副本中只增加测试文件，未修改官方业务代码。所有请求和数字均为合成数据，无模型调用。

| 检查 | 官方当前实际结果 | 期望 |
|---|---|---|
| Chat→Responses Schema 中 const=9007199254740993 | 变成 9007199254740992 | 原值保留 |
| Responses→Chat 同一 Schema | 变成 9007199254740992 | 原值保留 |
| Chat→Claude 同一 Schema | output_config.format 中没有 Schema | 保留或在生成前明确拒绝 |
| Claude→Chat 同一 Schema | response_format 中没有 Schema | 保留或在生成前明确拒绝 |
| Claude adapter 接收 max_tokens，再接收 message_stop | 记录 completed | incomplete |

可复查：

- [Schema 复现测试源码](official-schema-repro_test.go.txt)与[实际失败输出](official-schema-repro.log)。将测试放入固定版本的 `relaykit/relayconvert`，在 `relaykit` 目录运行 `go test ./relayconvert -run TestAuditOfficialSchemaConversions -count=1`。
- [Claude 终态复现测试源码](official-terminal-repro_test.go.txt)与[实际失败输出](official-terminal-repro.log)。将测试放入固定版本的 `relay/channel/claude`，在根目录运行 `go test -mod=readonly ./relay/channel/claude -run TestAuditOfficialClaudeMaxTokensOutcome -count=1`。
- [固定来源记录](source.json)。官方默认 relaykit Codec 与宿主 common Codec 均使用标准 json.Unmarshal，不存在宿主已设置 UseNumber 从而消除上述差异的情况。

因此，当前没找到能通过这五个反例、可直接替代本轮改动的官方等价实现。继续复用本地已有的上游转换基础和 gjson/sjson/RawMessage；新增代码只承担缺失的格式保真、明确拒绝与终态记录。不因为“有相近 PR”采用会降低本轮验证标准的代码。

## 重试部分的补充核查

另一独立审查核对相同 SHA 的[状态码默认范围](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/setting/operation_setting/status_code_ranges.go#L19-L38)、[错误重试分类](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/service/relay_error.go#L20-L56)与[候选选择](https://github.com/QuantumNous/new-api/blob/d032204733db31208f10bac118b6e683aab3bf29/service/channel_select.go#L44-L76)：默认范围仍含 409—499，通用 skipRetry 没有本轮 CPA 精确 422 合同，也没有同服务/凭据/协议排除身份。

检索到的 [#6580](https://github.com/QuantumNous/new-api/pull/6580) closed、未合并，按 channel ID 排除并非同服务识别；[#3113](https://github.com/QuantumNous/new-api/pull/3113)、[#2754](https://github.com/QuantumNous/new-api/pull/2754)未合并；[#7294](https://github.com/QuantumNous/new-api/pull/7294)仍 open，仅调整优先级偏移。没有把它们视为可直接替代的官方修复。

查询覆盖固定主分支、相关文件近期提交，以及 response_format、output_config、json_schema、precision、incomplete、stop_reason 与重试主题的 PR；这是对本轮问题的定向核查，不声称穷尽所有历史或未来修复。

## 本地修复验证

- [8 包全量测试](local-package-tests.log)：`go test ./dto ./types ./service/openaicompat ./relay/channel/claude ./relay/common ./service ./controller ./relay -count=1` 全部通过。
- [新增用例 race 检查](local-race-tests.log)：controller、Claude、openaicompat、dto、relay/common 共 5 包通过；覆盖新增重试、格式、终态与取消测试。
- [静态检查](local-vet.log)：上述 8 包 `go vet` 通过。
- `bunx --no-install prettier src/hooks/usage-logs/useUsageLogsData.jsx --check`、`git diff --check`、两份文档的本地链接检查通过。

上述测试均在本机使用合成 HTTP 服务与临时数据库完成，不代表已部署或真实客户请求已恢复。
