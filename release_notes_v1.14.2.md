## 合并上游 v1.14.0 / v1.14.1（配置热生效、网页管理台、输出预算修复、构建矩阵）

本版本合并 `CangShui/workbuddy-gateway` 自 `6565407` 起的 6 个上游提交（v1.14.0 配置热生效与网页管理台、v1.14.1 定时任务页、构建矩阵、价格探测默认关闭、网页完整 JSON 编辑器，以及上游 v1.13.17 的 `max_tokens` 输出预算修复），并保留 fork 全部私有能力（OpenAI 兼容思考等级归一化、客户端指纹与字节级对齐、保序 JSON、会话作用域链路 ID、SSE 心跳保真、反审查净化、单账号串行化）。

### 升级须知（行为变化）

- **后台价格探测默认关闭**：只有显式 `-allow-price-probes` 才发起后台实测生成请求（会消耗积分）。旧参数 `-disable-price-probes` 仍兼容；两者同时出现时禁用优先。fork 1.13.17 及更早版本默认开启探测，如需保持原行为请在启动参数中显式加上 `-allow-price-probes`。客户端请求、目录刷新与显式 `probe` 命令不受影响。
- **`config.json` 改为约 1 秒热生效**：不再需要重启即可应用模型名单、账号名单、`systemPrompt`、`debug.enabled`、`gateway.adminKey` / `apiKeyEnabled` / `apiKey`、`upstream` 超时与重试次数。`gateway.apiPort` / `webPort` 仍写入配置但**重启后**才变更监听。
- **网页控制台从只读升级为可管理**（见下），且不再展示凭据页面。
- **模型黑白名单不再接受相互冲突的非空组合**：若旧配置同时填写了非空白名单与非空黑名单，请在升级前择一保留。启动时遇到此类配置会明确报错；运行中热加载遇到非法配置则保留上一份有效配置并在日志中记录原因。

### 新增：`config.json` 热生效（上游 v1.14.0）

- `serve` 独立每 1 秒按文件内容检测配置变化，不受 `-reload-interval 0`（只关闭凭据扫描）影响；网页开关先写文件、再立即应用，外部编辑器修改在下一次扫描应用。
- 配置先整体解析、校验、再应用：半写 JSON、空文件、文件短暂缺失、错误类型或非法账号路径都保留上一份有效配置，错误原因写入 `logs/gateway-YYYY-MM-DD.log`，修正后自动重试；不会把读取失败误当成「全部开启」。
- 新增 `logs.retentionDays`（0 保留全部）：保留含今天在内的 N 个日历日。

### 新增：网页控制台账号 / 模型 / 凭据 / 日志 / 配置管理（上游 v1.14.0、v1.14.1）

- 账号池（全局调度开关）、模型列表（启停开关）、日志（保留天数与过期清理）、访问设置（模型 API Key 校验与端口编辑）、定时任务（只读）。
- 「配置」页：确认风险警告后可编辑完整 JSON 原文（含 Key）；读取时返回 SHA-256 `revision`，保存需带 `acknowledged: true`，若配置已被其他操作修改则返回 `409` 而不覆盖；原文不写入浏览器存储，离开配置页即清除，未保存修改需确认丢弃；配置上限 1 MiB。
- 「定时任务」页由现有 `logs/gateway-*.log` 中的 `[定时任务]` 记录聚合 11 项后台任务（含启动续期补跑），只展示、不触发任务、不修改调度；没有新增任务调度模块或状态文件。
- 网页凭据页与凭据编辑器移除，不再展示完整凭据；后端 `/admin/api/credentials`、`/admin/api/credential-edit` 暂保留兼容（仍受独立管理鉴权、同源校验与文件范围限制）。
- 所有管理接口继续要求独立管理 Key；模型 API Key 不能编辑配置、凭据或清理日志。

### 修复：客户端未声明输出预算时按模型目录补 `max_tokens`（上游 v1.13.17）

上游按「输入 token + 输出预算」判断是否超出模型窗口，请求不带 `max_tokens` 时会为输出默认预留 384000：标称 1048576 窗口的 `deepseek-v4.1-flash` 实际只剩约 664576 可用输入，长会话会在远未到窗口上限时被 `400 code=11133` 拒绝。本版本在 Chat / Responses / Anthropic 三个入口出站前补一个目录声明的输出上限；客户端显式传入的值（含 `max_completion_tokens`、`null` 语义）一律不覆盖，目录未收录的模型保持原行为。

### 构建：9 目标矩阵与校验清单（上游 3e40bb65）

- `build.ps1` 现构建 Windows 386/amd64/arm64、Linux 386/amd64/armv7/arm64、macOS amd64/arm64，默认先跑 `go vet` + `go test`，输出 `SHA256SUMS`、`build-manifest.json`、`config.example.json`，构建审计写入 `logs/build-YYYY-MM-DD.jsonl`；`build_test.ps1` 校验脚本自身的参数组合与产物清单。
- 本版本发布资产：9 个平台二进制、`SHA256SUMS`（含 9 个二进制与清单/示例配置的哈希，与上游口径一致）、`SHA256SUMS.txt`（保留 fork 既有的 `<hash>  <文件名>` 格式，覆盖全部 9 个二进制，便于沿用既有校验脚本）、`build-manifest.json`、`config.example.json`。

### 新增：网页配置完整 JSON 编辑器（上游 0d2b9d6f）

风险确认 + 版本冲突检测的完整配置编辑器（等价于直接编辑服务器上的 `config.json`）：可热配置立即生效，端口重启后生效，日志保留规则可能触发不可撤销的删除；管理 Key 变更后旧会话失效，清空管理 Key 需额外确认。

### 合并冲突处理（fork 适配）

- `main.go` 版本常量：合并时取上游 `1.14.1`，随后 bump 到 `1.14.2`。
- `main.go` 出站请求头：保留 fork 的 `backendHeaders(req, acc.Auth, prof, r.Header, sess)` 单写入方签名，继续移除上游在调试分支对 `X-Trace-ID` / `X-Parent-Request-ID` 的出站覆盖（由 `TestDebugLoggingDoesNotRewriteUpstreamTraceHeaders` 固化该契约）。
- `README.md`：配置生效方式取上游「热生效」表述，Go 工具链要求取上游 `go.mod` 口径，并补 v1.14.2 fork 版本说明。
- `ensureUpstreamMaxTokens` / `clientDeclaredOutputBudget` 参数改为 `any`：同时支持保序 JSON（`*jsonObject`）与上游的 `map[string]any`，Chat / Responses 两个保序入口走 `*jsonObject` 分支，Anthropic 入口继续走 map 分支，保序语义不被打断。
- 上游新增测试 `maxtokens_test.go` 的 `profileCN.Origin` 改为 fork 的 `PortalOrigin`。

### 版本

`1.13.17` → `1.14.2`。

## 验证

- `go vet ./...` 通过。
- `go test ./...` 全绿（约 7 秒）。
- `node web/app_test.cjs` 14/14 通过（上游前端用例）。
- `build.ps1` 全 9 目标构建成功，产物、`SHA256SUMS`、`build-manifest.json` 已随 release 发布。

> 说明：本机无可用上游登录凭据，无法对真实上游做端到端对话验证；上述验证均以本地假上游 + 单元测试完成。
