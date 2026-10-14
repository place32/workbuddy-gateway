## 消除模型请求 `Accept-Encoding` 的主动偏差 + 控制面请求形状修正

本版本只改**网关向上游发出的请求**与**上游响应的读取方式**：不新增配置项、不改下游接口、不改调度与凭据逻辑。升级后无需修改 `config.json`。

### 控制面请求形状修正

- **刷新链路补上 body `{}`**：客户端 `refreshSession` 是 `post('/v2${prefixPath}/auth/token/refresh', {})`，axios 因 body 存在而补 `Content-Type: application/json`，线上 `Content-Length: 2`；网关此前发空 body（`Content-Length: 0` 且无 `Content-Type`）。现按客户端形状发送，并新增线上请求形状断言。
- **控制面 GET 不发送 `Content-Type`**：axios 只在带 body 的调用上补该头。实测 CLI 2.156.0 的 `GET /v2/plugin/accounts` 头部为 `Accept` / `X-Requested-With` / `X-Domain` / `Authorization` / 链路头 / `X-Product` / `User-Agent` / `X-User-Id` / `Accept-Encoding`，**无** `Content-Type`、无 `Content-Length`。故 `controlHeaders` 改为按「请求是否带 body」判定：`auth/state`、`auth/token/refresh`、额度/套餐/签到等 POST 带，`auth/token` 轮询与 `login/account` 两个 GET 不带（此前一律带，属「多出客户端没有的头」）。
- 登录链路四步（`auth/state` → `auth/token` → `login/account` → `auth/token/refresh`）的头集合按 bundled CLI 源码逐步对齐，包括未鉴权时的 `X-No-Authorization` / `X-No-User-Id` / `X-No-Enterprise-Id` / `X-No-Department-Info` 四项、`X-Refresh-Token`，以及「`X-Domain` 只出现在 `auth/state` 与 `login/account`」这类逐步差异。

### 模型请求 `Accept-Encoding`：从主动偏差改为完整对齐

上一版本把该头列为**唯一的主动偏差**（客户端发送、网关不发送），本版本消除它。

**依据（仓库内可复核）**：`.client_ref/cap/intl_chat_capture.jsonl`（5 次模型请求）与 `intl_toolturn_capture.jsonl`（72 次模型请求）是本机 5.7.6 客户端发往 `/v2/chat/completions` 的**线上字节**抓包。每条模型请求 39 个头，`Accept-Encoding: gzip, compress, deflate, br` 位于第 36 槽（0 基），即 `Content-Length`（35）与 `Host`（37）之间。更早的一批抓包（`.client_ref/capture2.jsonl` 等，模型请求 38 个头）没有该头 —— 那批抓包钩在 Node `https.request` / 头部对象层，看不到适配器后补的传输层头；钩子层以上得到的**头集合**与**顺序**结论不受影响。

- **网关现在显式声明同一值**：`backendHeaders` 补 `Accept-Encoding: gzip, compress, deflate, br`，`wireorder.go` 的顺序基线相应加入该槽位（`Content-Length → Accept-Encoding → Host → Connection`）。
- **响应侧自行解压**：`transport` 保持 `DisableCompression: true` —— 否则 Go 会自行补出值不同的 `Accept-Encoding: gzip` 并接管解压，与客户端形状不符。故按 `Content-Encoding` 解压由网关完成：`gzip` / `deflate` 用标准库，`br` 用纯 Go 的 `andybalholm/brotli`（新增依赖，无 CGO，不影响 9 目标交叉编译）。多段编码（RFC 9110）按声明**逆序**解码；未支持的编码（如 `zstd`）返回 `502 upstream_encoding_unsupported`，**绝不把压缩字节当明文喂给下游**。
- **解压层位置**：包在空闲看门狗**外侧** —— 卡死判定仍按原始字节的到达节奏，下游与聚合逻辑始终读到明文 SSE。
- **控制面与错误响应复用了同一套解压**：`readControlBody` 改由 `wrapUpstreamDecoding` 实现，语义不变（此前已支持 `gzip` / `deflate`、其余编码报错），额外获得 `br` 支持；模型请求的**非 2xx 错误正文**也一并解压（`readDecodedAll`）—— 否则限流/额度/鉴权这几个依赖正文关键字的判定会全部落空，可识别的上游错误会被降级成「未知错误」。

抓包只记录请求侧头部，**上游是否真的以 `br` 压缩模型响应流未获确认**；实现不依赖该假设：不压缩、`gzip`、`deflate`、`br` 四种响应形态都能正确落地。

### 验证

- `go vet ./...` 与 `go test ./...` 全绿。
- 新增 `TestCompressedUpstreamResponseIsDecoded`：本地假上游用 `br` / `gzip` / `deflate` / 不压缩四种方式返回同一段 SSE，断言下游收到明文事件与 `[DONE]`；并断言未支持编码 `zstd` 返回 `502 upstream_encoding_unsupported`（而非把压缩字节当明文）。
- 新增 `TestCompressedUpstreamErrorResponseIsDecoded`：假上游用 gzip 返回 `400 {"code":11140,...}`，断言网关解压后再判定错误类型，下游拿到上游原文（压缩字节会让限流/额度/鉴权分类落空）。
- 三处头部基线补上 `Accept-Encoding` 并断言值等于客户端值：`realClientChatHeaders`（指纹基线）、`clientModelHeaderOrder`（线上顺序基线，含两条 TLS / 明文线上字节用例）、端到端冒烟 `TestSmokeEndToEndUpstreamHeaders`。
- `TestLoginControlPlaneHeadersMatchClient` 改为按各步**真实方法与 body** 构造请求：`auth/state`（POST + body）必须带 `Content-Type: application/json`，`auth/token` 轮询与 `login/account`（GET、无 body）必须**不带** `Content-Type`，锁住「控制面 GET 多出 Content-Type」这一回归。
- 新增 `TestRefreshRequestMatchesClientBody`：起一个本地假上游，断言刷新请求的线上形状是 `post(url, {})` —— body 恰为 `{}`、`Content-Length: 2`、`Content-Type: application/json`，且头集合为控制面族（`X-Refresh-Token` / `X-Auth-Refresh-Source: plugin` / `X-Domain` / `Authorization` / `X-User-Id` / `X-Product` 齐全，`X-IDE-*` / `x-stainless-*` / `traceparent` / `X-Conversation-*` / `x-codebuddy-request` 一律缺席）。
- 用注入宿主身份的 bundled CLI 2.156.0 对国际站默认端点做了一次独立线上字节复核（明文抓包）：`X-IDE-*` / UA / `x-stainless-runtime-version` / gzip 请求体 / 头部键名大小写 / 头部顺序均与网关基线一致。该次复核中 `x-requested-with` 走规范化大小写、且头部块前置一个空 `X-B3-ParentSpanId`，属「CLI 在无宿主父 span 的调用路径」下的差异；桌面宿主链路（有会话父 span）产出的是小写键与带值父 span，即网关复刻的那一态。

### 依赖与构建

新增 `github.com/andybalholm/brotli v1.2.6`（纯 Go，无 CGO）。`go.sum` 已随仓库更新；受限网络环境下 `go mod download` 可用国内镜像（`GOPROXY=https://goproxy.cn,direct`）。

### 已知限制与观察

- **封禁链路仍待用「未命中风控的账号」复测**：本版本只改网关发出的请求与响应读取方式，能否通过模型接口只能在未被风控命中的账号上验证；已命中的凭据在控制面（`/v2/plugin/accounts`、额度接口）仍返回 200，说明封禁作用在模型接口的准入判定上。
- 国内站 Profile 仍为 5.5.2 基线（本机无 5.7.6 国内版客户端可抓包）；若腾讯同时更新国内版客户端，需重取基线。
- 配置 `-proxy` 时头部**顺序**不生效（头部集合、键名大小写与压缩协商仍保持对齐）。

### 版本号

`main.go` 版本常量：`1.14.3` → `1.14.4`。
