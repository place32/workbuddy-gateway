## 合并上游 v1.13.0

本版本合并 `CangShui/workbuddy-gateway` 的 v1.13.0，并保留 fork 全部私有能力（OpenAI 兼容思考等级归一化、客户端指纹与字节级对齐、保序 JSON、会话作用域链路 ID、SSE 心跳保真、反审查净化）。

### 工具调用序列自愈（解决 11148）

出站前按 `tool_call_id` 修复并行调用中夹入普通消息的历史结构：合并 Responses API 拆散的单调用消息，对称删除无结果调用、孤儿结果和重复项。结果保持原始出现顺序，不按调用顺序重排。修复对保序 JSON 对象就地生效，不把请求体打回字典序 `map`。

### 配置化单行 JSON 调试日志

工作目录 `config.json` 的 `debug.enabled` 控制，默认关闭。开启后写入 `logs/debug-YYYY-MM-DD.jsonl`。请求体只记长度、耗时、SHA-256 前 12 位和 JSON 是否合法，不记正文；凭据只记是否提供 Authorization 和不可逆短哈希。

客户端传入的 `X-Trace-ID` 优先作为日志里的 `trace_id`。出站链路头仍只由会话作用域对齐写入，调试开关不会用中间件 UUID 覆盖，也不会追加非客户端的 `X-Parent-Request-ID`。

### 模型黑白名单

`config.json` 的 `models.blocklist` / `models.allowlist`（大小写与首尾空白不敏感）。白名单非空时只放行名单内模型。被禁模型从 `/v1/models`、`/health`、`monitor` 附表隐藏；请求返回 403 中文提示，不消耗上游额度。

### 上游超时与空闲看门狗

不再对整条流设总超时。`ResponseHeaderTimeout` 默认 300 秒，流式空闲读默认 120 秒（持续有数据则不超时），均可在 `config.json` 的 `upstream` 段覆盖。流中断时下发 `upstream_stream_interrupted` / `response.failed`，不伪造 `[DONE]` 或 `response.completed`。

### 账号套餐列与统计表

账号表「付费用户」列改为套餐：`Pro试用`（`ProTrialStatus=1`）、`pro`（`IsPaidUser=true`）、`免费`（其余，含识别不出）。模型统计附表新增 `总Token(M)`，按百万累计进程启动后的上游 usage；优先 `total_tokens`，否则 `prompt_tokens + completion_tokens`，上游未返回 usage 的请求不估算。

### 修正 14018 误判

账号额度耗尽（14018）只阻断当前账号调度，不再把免费模型改写成收费模型。

### 合并冲突处理

- 调试日志接入 `upstreamChat` 时去掉上游对出站头的覆盖（`Header.Set("X-Trace-ID")` / `X-Parent-Request-ID`）。链路头继续由 `setTraceHeaders` 按会话作用域写入，键名大小写保持客户端口径。
- 工具序列修复的入参从 `map[string]any` 改为同时接受保序 `*jsonObject`。字段读写走 `Get` / `Set` / `Delete`，不直接改内部键值表，避免删键后键序表残留。
- 上游新增测试里的 `profileCN.Origin` 改为 fork 的 `PortalOrigin`。`streamChatResponse` 补上审计中间件所需的入站 `*http.Request`。
- `config.example.json` 同时给出 `debug`、`upstream` 超时和 `models` 黑白名单三段。
- README 保留 fork 的「思考等级传参」「客户端指纹对齐」，并改写调试日志一节：客户端 trace 只进入日志，不覆盖出站头。
- 版本取上游 `1.13.0` 后 bump 至 `1.13.1`（fork 已领先上游）。

## 验证

- `go vet ./...`、`go test ./...` 全绿。
- 五个平台交叉编译全部成功（windows/linux/darwin × amd64/arm64）。

> 说明：本机无可用上游登录凭据，无法对真实上游做端到端对话验证；上述验证均以本地假上游 + 单元测试完成。

下载对应平台文件替换网关程序后重启即可；附 `SHA256SUMS.txt` 用于校验。
