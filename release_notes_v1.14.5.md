## 国内站指纹按本机 5.5.3 客户端重新对齐（模型请求 / 控制面 / 请求体压缩）

本版本把**国内站**（`copilot.tencent.com`）的指纹基线从「5.5.2 桌面宿主抓包」换成**本机安装的 5.5.3 客户端**（`D:\workbuddy`，productName `WorkBuddy`，内置 CLI 包版本 `2.137.1`）的实测实发。国际站（5.7.6 / CLI 2.156.0）不变。

### 依据（可复核）

| 证据 | 内容 |
|---|---|
| `.client_ref/cap/cn_chat_capture.jsonl` | 本机 5.5.3 国内站链路的**线上字节**抓包（sink 侧记录，`Host` / `Content-Length` 齐备，不是钩子层级盲区）。6 条请求：模型请求 1 条（38 头）、`/v3/config` 2 条、`/v2/report` 3 条（控制面 axios 族） |
| `.client_ref/cn/main__client-info-env.js`、`.client_ref/cn/main__workbuddy-product-config.js` | 该安装的桌面源码（`@genie/workbuddy-desktop` 5.5.3）：`WORKBUDDY_PLATFORM = "WorkBuddy"`，`CLIENT_INFO_*` 的版本取 `app.getVersion()` = `5.5.3`，UA 扩展段 `CLI/2.137.1` |
| `D:\workbuddy\resources\app.asar.unpacked\cli\product.json` | `productName: "WorkBuddy"`、`authentication.attributes.platform: "workbuddy"` |

### 行为变化（两个站点逐项对照）

| 项 | 旧（v1.14.4） | 新（v1.14.5） |
|---|---|---|
| `User-Agent` | `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1` | `WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1` |
| `X-IDE-Type` / `X-IDE-Name` / `X-IDE-Version` | `CodeBuddy` / `CodeBuddy` / `5.5.2` | `WorkBuddy` / `WorkBuddy` / `5.5.3` |
| `x-stainless-runtime-version` | `v22.22.2` | `v22.21.1`（与 5.7.6 宿主同值） |
| `auth/state` 的 `platform` 查询参数 | `VSCode` | `workbuddy`（取 `product.json` 的 `authentication.attributes.platform`） |
| 模型请求体 | 不压缩 | **gzip**（`Content-Encoding: gzip`，与体积无关） |
| `Accept-Encoding`（模型请求 + 控制面） | 发送 `gzip, compress, deflate, br` | **不发送**（模型请求 39 → 38 头；尾部即 `Content-Length → Host → Connection`） |

国际站维持 v1.14.3 / v1.14.4 的三族头集合、`Accept-Encoding: gzip, compress, deflate, br`、`platform=workbuddy-ai` 与 `wireorder` 顺序基线。

### 实现

- `upstreamProfile` 的国内站字段按实测值改写（`Platform` / `PlatformName` / `PlatformVersion` / `ProductName` / `NodeVersion` / `AcceptEncoding` / `GzipBody`），并逐条注明证据来源；两站 `NodeVersion` 现为同值。
- 控制面 `Accept-Encoding` 改为按站点取值（原为固定值），国内站缺省不发送。
- `wireorder.go` 的顺序表未改：该表按**头名**匹配，缺席的头自然不写出，故一张表同时覆盖两站（国内站尾部少一个 `accept-encoding` 槽位）。

### 验证

- `go vet ./...` 通过；`go test ./...` 全绿（新增 `TestCNModelRequestMatchesClient`、`TestCNControlPlaneOmitsAcceptEncoding` 覆盖国内站的线上头集合、请求体 gzip 与「控制面不发 `Accept-Encoding`」）。
- 与抓包的**集合级 diff**：网关国内站模型请求 36 个头（`req.Header` 层）对照抓包 38 个，差异仅为传输层 `Host` / `Content-Length`（Go 由 transport 写出，不在 `req.Header`）与抓包侧的 `Content-Encoding`（网关在压缩请求体时补），以及抓包为未鉴权态而缺席的 `X-Domain`；`accept-encoding` 两侧同为「无」。
- 因国内站模型请求体改为 gzip，测试假上游统一走 `gunzipUpstreamBody`（按 gzip 魔数判定，未压缩原样返回）后解析，重试重放断言改为「解压后内容一致」。

### 版本

`1.14.4` → `1.14.5`。无配置变化，升级即生效。

> 说明：抓包与源码样本均属本地研究产物（含凭据/许可信息），不入仓库；`.client_ref/` 已在 `.gitignore` 内。
