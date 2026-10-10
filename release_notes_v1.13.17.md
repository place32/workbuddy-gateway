## 合并上游 webui-dev 与配置化鉴权

本版本合并 `CangShui/workbuddy-gateway` 的 webui-dev 分支（PR #12，两个上游提交），并保留 fork 全部私有能力（OpenAI 兼容思考等级归一化、客户端指纹与字节级对齐、保序 JSON、会话作用域链路 ID、SSE 心跳保真、反审查净化、单账号串行化）。

### 新增：只读网页管理台（上游 PR #12，LuoXingchen935）

- `-webui` 启用独立 Web 端口（默认 `http://127.0.0.1:8316/ui/`），纯静态页面 + `/admin/api/*` JSON 接口，仅读取状态快照与日志，零数据库依赖。
- 实时展示：账号状态与配额、模型统计与价格、流量趋势、凭据生命周期、原始 JSON 日志浏览。
- 本机（或 SSH 本地端口转发）可一键生成 32 字符管理 Key 并写入 `config.json`；也可直接编辑 `gateway.adminKey` 手动配置。

### 新增：`config.json` 的 `gateway` 段驱动端口与鉴权（上游 6565407）

- `apiPort`（默认 8317）、`webPort`（默认 8316）全部配置化，`-port` 仍可显式覆盖 API 端口。
- `adminKey` 与 `apiKey` 相互独立：管理 Key 登录网页控制台；`apiKeyEnabled` 开启后模型 API 才校验 `apiKey`。
- 移除 `-api-key` 启动参数（保留同名静默占位兼容旧 systemd 单元）。
- **升级顺序**：新二进制能读旧配置（无 `gateway` 段），但旧二进制遇到含 `gateway` 段的新配置会启动失败——请先替换二进制、再添加 `gateway` 配置。

### 安全与健壮性修复（上游 6565407）

- 配置页与日志接口返回脱敏副本，管理 Key / 模型 Key 不再出现在页面；日志保留错误码、原因与 TraceID。
- 日志读取改为线性扫描 + 1 MiB 预算，消除旧 `tailLines` 逐字节前插的高分配开销（新增 `logtail.go`）。
- 状态快照缺失或损坏时返回 503，前端保留上次数据而非显示零账号。
- 管理端使用独立中间件链：同源校验、管理鉴权、CSP / no-store 等安全头（新增 `websecurity.go`）。
- 网页控制台端口绑定失败会释放已绑定的监听，不会启动半套服务。

### 代码结构

新增文件：`gatewayconfig.go`（gateway 段解析/校验）、`servers.go`（双端口服务装配）、`webui.go`（管理 API）、`websecurity.go`（安全中间件）、`logtail.go`（线性日志读取）、`web/`（前端静态资源）、`config.example.json` 与对应回归测试（`gatewayconfig_test.go`、`configexample_test.go`、`webui_test.go`、`web/app_test.cjs`）。

### 合并冲突处理（fork 适配）

本次合并且并**无冲突**，`ort` 策略自动完成。fork 保留的私有能力（保序 JSON、会话作用域链路 ID、UA 指纹对齐等）与上游变更区域无重叠，全部原样保留。

### 版本

`1.13.16` → `1.13.17`。

## 验证

- `go build ./...` 通过。
- `go test ./...` 全绿（4.3s）。

> 说明：本机无可用上游登录凭据，无法对真实上游做端到端对话验证；上述验证均以本地假上游 + 单元测试完成。
