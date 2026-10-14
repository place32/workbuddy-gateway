## 国际站客户端指纹对齐：Profile 对齐桌面客户端 5.7.6、请求头拆分为三套家族、新增头部线上顺序复刻

本版本只改**网关向上游发出的请求**，不新增配置项、不改下游接口、不改调度与凭据逻辑。升级后无需修改 `config.json`。

### 背景

- 国际站账号触发风控。可比对的特征分两类：**自造字段**（客户端不会发出的头）与**缺失/错配字段**（客户端必然发出的头或形态被改掉）。
- fork 此前把国际站基线定为**内置 CLI 的裸身份**（`X-IDE-*` = `CLI` / `CLI` / `2.137.1`，UA = `CLI/2.137.1 WorkBuddy AI/2.137.1`）。真实国际版客户端是**桌面宿主 + 内置 CLI**，宿主会注入自己的产品标识，裸 CLI 身份与之不符。
- 本机安装的是国际版客户端 **WorkBuddyAI 5.7.6**（内置 CLI 包版本 **2.156.0**），本版本据此重设国际站 Profile，并把「头集合一致」推进到「**线上字节一致**」。

### 国际站 Profile：`CLI/2.137.1` → `WorkBuddy 5.7.6 + CLI/2.156.0`

| 项 | 旧值（裸 CLI） | 新值（桌面宿主注入身份） |
|---|---|---|
| `User-Agent` | `CLI/2.137.1 WorkBuddy AI/2.137.1` | `WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0` |
| `X-IDE-Type` / `X-IDE-Name` | `CLI` / `CLI` | `WorkBuddy` / `WorkBuddy` |
| `X-IDE-Version` | `2.137.1` | `5.7.6` |
| `x-stainless-runtime-version` | `v22.22.2` | `v22.21.1`（**改为按站点取值**，见下） |

依据是桌面宿主源码 `buildWorkbuddyClientInfoEnv`：宿主以 `CLIENT_INFO_*` 注入 `platform` / `ideType` = `WorkBuddy`、`platformVersion` / `productVersion` / `pluginVersion` = `app.getVersion()`（`5.7.6`）、`userAgentExtension` = `CLI/2.156.0`，随后用自带 Electron 二进制以 `ELECTRON_RUN_AS_NODE=1` 启动 CLI。故 UA 的三段均取自宿主，而 `x-stainless-runtime-version` 是**宿主内置 Node** 的版本（5.7.6 → Electron 37.10.3 → `v22.21.1`），不是系统 Node 版本。

**国内站 Profile 不变**（桌面 5.5.2 + CLI 2.137.1，`X-IDE-*` = `CodeBuddy`）：本机只安装了国际版客户端，国内站无同源实测，不做臆测性改动。

`x-stainless-runtime-version` 由全局常数改为**按站点取值**（`upstreamProfile.NodeVersion`）：该值是「运行 bundled OpenAI SDK 的 Node 进程」版本，随宿主客户端自带的 Electron 走 —— 国际站取 5.7.6 的实测值 `v22.21.1`，国内站保留 5.5.2 宿主抓包的实测值 `v22.22.2`。若沿用单一常数（无论取哪一站的值），另一站的模型请求都会带上对方客户端的 Node 版本。

### 请求头拆分为三套家族

客户端用三套 HTTP 栈，头集合互不相同。旧实现把「模型请求头集合」当通用头表用，等于往控制面与守护进程接口上发了带 `X-IDE-*` / `x-stainless-*` / `traceparent` / `X-Conversation-*` 的请求 —— 官方客户端不可能产生的组合。

| 家族 | 实现 | 覆盖请求 | 关键差异 |
|---|---|---|---|
| 模型请求 | `backendHeaders` | `/v2/chat/completions` | 唯一的完整头集合（SDK 头 + 会话/Agent 头 + IDE 头 + 传播头） |
| 控制面 | `controlHeaders` | 登录四步、续期、账号快照、额度 / 套餐 / 签到探测 | axios 默认头（`Accept: application/json, text/plain, */*`）、`X-Product`；**无** `X-IDE-*` / `x-stainless-*` / `traceparent` / `X-Conversation-*` |
| 守护进程 | `daemonHeaders` | `/v2/enterprises/personal/models` | undici 自动头（`user-agent: node`、`accept-encoding: gzip, deflate`、`accept-language: *`、`sec-fetch-mode: cors`）、`b3` 三段、键名 `X-Trace-Id`；**无** `X-IDE-*` / `x-stainless-*` / `X-Product` / `traceparent` |

调用点按「客户端里这条请求由哪个栈发出」重新归位：

- 凭据只读校验（`/billing/meter/get-user-resource-summary`）→ 控制面（旧实现混用模型头集合，并自造 `X-Client-Platform: web`）。
- 实时模型目录（`/v2/enterprises/personal/models`）→ 守护进程（旧实现用 `backendHeaders`）。
- 额度 / 套餐 / 每日签到三个控制台探测 → 控制面。

同时修正/补齐控制面细节：

- **移除**自造头 `X-Client-Platform: web`：客户端全量安装目录（含 `app.asar`）字节级 0 命中。
- `X-Auth-Refresh-Source`：`workbuddy` → **`plugin`**（客户端 `refreshSession` 实测值）。
- `X-Tenant-Id` 与 `X-Enterprise-Id` 同值，仅企业账号发送（`AuthenticationHttpInterceptor` 在账号存在 `enterpriseId` 时无条件补该头）。
- `X-Domain` 在控制面请求上取账号 `domain`，缺失时回退站点主机名（客户端 `enterpriseHeaders()` 用 `URI.parse(endpoint).authority`）。
- 控制面请求 `X-Request-ID` 与 `X-Trace-ID` 同值（客户端同一 trace 内一致）。
- 控制面响应自行解压：模型请求的 transport 设了 `DisableCompression`（模型请求实测不带 `Accept-Encoding`），控制面走 axios 族会带 `gzip, compress, deflate, br`，故 `gzip` / `deflate` 由标准库处理，其余编码**明确报错**而不是静默解析失败。

登录链路四步（`auth/state` → `auth/token` → `login/account` → `auth/token/refresh`）的头集合按 bundled CLI 源码逐步对齐，包括未鉴权时的 `X-No-Authorization` / `X-No-User-Id` / `X-No-Enterprise-Id` / `X-No-Department-Info` 四项、`X-Refresh-Token`、以及「`X-Domain` 只出现在 `auth/state` 与 `login/account`」这类逐步差异。

### 新增：请求头线上顺序复刻（`wireorder.go`）与 `Connection`

「头集合与键名大小写一致」仍不等于「字节一致」。`net/http` 会把 `Host` / `User-Agent` / `Content-Length` 单独前置，其余头部经 `http.Header`（map）按**规范化键名字典序**写出，而客户端按**属性插入顺序**写出 —— 两者头部字节序列截然不同，无需解析语义即可判定。

- 顺序基线取自两处互相印证的实测（结构完全一致）：本机 5.7.6 客户端的模型请求抓包，以及一份按**线上字节顺序**记录的 5.5.2 桌面客户端抓包（抓包含凭据，属本地研究产物、不入仓库）。成因在 bundled CLI 源码中逐条可定位：SDK 默认头 → 应用自建头表 → 处理器链（`GzipRequestProcessor` priority=9999 末位追加 `Content-Encoding`）→ OTel + B3 传播头 → 鉴权拦截器（`Authorization` … `X-Domain`）→ `X-Product` / `User-Agent` → Node http 补齐 `Content-Length` / `Host` / `Connection`。
- 复刻点选在 **conn 层**：`net/http` 写头部的位置不可拦截，但其字节必经 `net.Conn.Write`，故在 `Transport.DialContext`（明文）与 `DialTLSContext`（直连 https，本层自行完成 TLS 握手、不设 ALPN 以停在 HTTP/1.1）上包一层，在头部块写出前重排。
- **只重排能确认是 HTTP/1.1 请求头的字节**：代理 CONNECT、TLS 密文、无法确认的字节流一律原样透传；正文只按 `Content-Length` 计数透传、绝不解析（正文可能含 `CRLFCRLF`），keep-alive 复用连接的后续请求同样被正确重排。头部块超 64 KiB、请求行非法、头名非 RFC 9110 token 一律不改写。
- 仅 `/v2/chat/completions` 有逐字节顺序基线，故只对该路径生效；控制面与守护进程两族保持「集合 + 键名大小写」对齐。
- **配置 `-proxy` 时不生效**：Go 自行 CONNECT + TLS，不存在明文层（头集合与键名大小写仍保持对齐）。
- 模型请求与守护进程请求补发 `Connection: keep-alive`（客户端由 Node http 模块补齐，Go transport 默认不写；HTTP/1.1 默认即长连接，语义不变）。

### 验证

- `go vet ./...` 与 `go test ./...` 全绿；新增 `wireorder_test.go` 覆盖重排结果、分段写出的头部、chunked / 正文含 `CRLFCRLF` 的计数透传、非 HTTP 字节流的原样透传，以及**明文监听与自建 TLS 拨号两条路径**的线上字节断言（测试直接在 wire 上读出头块逐字节比对）。
- 头部家族断言落在既有测试上：凭据校验与额度/套餐探测必须带控制面头集合且不得带 `X-IDE-*` / `x-stainless-*` / `traceparent` / `X-Conversation-*`，模型目录请求必须走守护进程头集合。
- 新增 `TestStainlessNodeVersionFollowsProfile`：两站模型请求的 `x-stainless-runtime-version` 必须各取本站实测值（`v22.22.2` / `v22.21.1`），锁定「单一常数覆盖两站」的回归。
- 对真实上游的连通验证：请求经新拨号层抵达 `https://www.workbuddy.ai` 并被正常处理（返回业务 JSON，而非连接层错误或畸形请求响应），说明 TLS 握手、HTTP/1.1 请求行与头部块均被接受。

### 已知限制与观察

- 当前国际站凭据在**模型接口**上被上游拒绝：`403 {"code":11140,"msg":"request illegal", displayMsg=内容未通过安全审核}`；同一凭据在**控制面**（`/v2/plugin/accounts`、`/billing/meter/get-user-resource-summary`）仍返回 `200` 且能取到账号与额度数据。该现象在**本次改动之前**即以同一形态出现（见 `logs/gateway-2026-10-14.log` 11:31 记录，当时二进制尚未包含本版本改动），故与本版本的头部顺序/头集合改动无因果关系。要验证「对齐后的指纹能否通过模型接口」，需要在风控未命中的账号上重新登录后复测。
- 国内站 Profile 仍为 5.5.2 基线（本机无 5.7.6 国内版客户端可抓包）；若腾讯同时更新国内版客户端，需重取基线。
- 配置 `-proxy` 时头部**顺序**不生效（见上）。

### 版本号

`main.go` 版本常量：`1.14.2` → `1.14.3`。
