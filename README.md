# WorkBuddy Local Gateway

<img width="917" height="754" alt="image" src="https://github.com/user-attachments/assets/7dcfc461-1357-4991-9565-279047687898" />


基于腾讯 **CodeBuddy** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。无 Web UI，全部通过命令行（CLI）完成登录、凭据续期与服务控制。

**同时支持两个上游站点**（同一套 `/v2/plugin/*` 协议，凭据按站点隔离，账号池可混挂轮询）：

| 站点 | 上游 | 登录方式 | 登录命令 |
|---|---|---|---|
| 国内站 | `copilot.tencent.com` / `www.codebuddy.cn` | 微信 / 企业微信扫码 | `login` |
| 国际站 | `www.workbuddy.ai` | 浏览器内登录（邮箱 / 验证码 / SSO） | `login -intl` |

---

## 目录

- [核心特性](#核心特性)
- [命令总览](#命令总览)
- [serve](#serve)
- [login](#login)
- [status](#status)
- [refresh](#refresh)
- [monitor](#monitor)
- [probe](#probe)
- [reset](#reset)
- [version / help](#version--help)
- [多账号池](#多账号池)
- [模型列表与倍率](#模型列表与倍率)
- [客户端接入](#客户端接入)
- [思考等级传参](#思考等级传参)
- [各平台部署](#各平台部署)
- [客户端指纹对齐](#客户端指纹对齐)
- [安全提示](#安全提示)
- [从源码构建](#从源码构建)

---

## 核心特性

- **国内 / 国际双站反代**：两个站点走同一套协议，凭据通过 `edition` 字段区分，刷新与对话自动路由到各自上游。
- **模型完全透传**：客户端传什么 `model` 就原样中继到上游，无白名单限制。`/v1/models` 仅用于客户端自动补全，不影响实际转发。
- **模型列表双来源合并**：实时接口 + npm 静态目录，按 ID 去重、接口优先；失败用本地缓存，两边都失败且无缓存时该站点本轮不展示模型（不影响调用）。
- **模型倍率与价格探测**：促销生效时展示 `credits` × factor；促销过期或接口无有效倍率时由余额未耗尽的同站点账号实测（启动即探测、重置后立即探测、每模型 12 小时一轮）。
- **多账号池 + 轮询负载均衡**：`-auth` 逗号分隔或 `-auth-dir` 目录，请求按 round-robin 分发；国内站与国际站账号可混挂。
- **模型级隔离**：`6004` 只冷却触发它的账号 + 模型，`14018` 只阻断该账号的当前收费模型，不再因为一个模型拖垮整个账号。
- **免费站点优先**：同一模型若「一个站点免费、另一个站点收费」，优先使用免费站点账号直至其受限；两个站点都收费（仅倍率不同）时不做倾斜，正常轮询。
- **免费/收费学习**：按「账号 + 模型」从响应 `usage.credit` 学习；`credit=0` 且样本足够（`total_tokens ≥ 100`）才判定免费，避免小样本误判。
- **国内站每日自动签到**：服务启动、凭据热加载时立即补签，之后每天 `UTC+8 09:00` 自动签到；国际站跳过。
- **凭据热加载（免重启）**：默认每 5 秒扫描凭据来源，新增 / 更新 / 删除凭据免重启生效。
- **授权失效自动禁用**：401/403 / `invalid token` / 登录过期时禁止调度、删除凭据文件并写入失效标记，重新 `login` 后自动恢复。
- **后台自动续期**：每 5 分钟检查 Token，距过期不足 15 分钟自动刷新并写回凭据文件。
- **流式分片规范化**：把上游每个分片携带的 `finish_reason:""` 归一化为 `null`，避免 Anthropic 翻译层误判 `stop_reason` 导致工具不执行。
- **OpenAI 兼容协议**：`/v1/chat/completions`（SSE 流式 + 非流式聚合）、`/v1/responses`（Responses API）、`/v1/models`、`/health`。
- **深度思考等级遵循 OpenAI 兼容规范**：对外接受 Chat Completions 的顶层 `reasoning_effort`、Responses 的 `reasoning.effort` / `reasoning.summary` / `text.verbosity`（含 `none` 关闭语义），归一化为上游认识的扁平字段。**仅当客户端显式传参时转发，绝不强制注入**，避免触发上游内容安全策略（详见[思考等级传参](#思考等级传参)）。
- **上游指纹对齐**：请求头逐项对齐官方客户端（详见[客户端指纹对齐](#客户端指纹对齐)），消除自造头与缺失头构成的可识别特征。
- **反审查净化**：自动改写 Claude Code 等框架被上游逐字拉黑的固定 Prompt 语句。
- **主动探测模型属性 (probe)**：余额耗尽的账号不会被正常调度，因而学不到「该账号该模型是否免费」。`probe` 子命令可主动探测并写入账本。
- **会话结构自动归一化**：自动保证首条消息为 `system`，修复部分非 harness 客户端（以 `assistant` 续写或以 `tool` 回传工具结果开头）触发的上游 `first message is not system prompt` (code 11128) 报错；同时兼容 OpenAI 新版 `developer` 角色。
- **单账号串行化**：同一账号请求自动排队，避免并发双发触发上游风控；不同账号之间可并行。
- **一键重置 (`reset`)**：清空除登录凭据外的全部本地数据（状态快照 / 模型缓存 / 日志 / 失效标记），并立即重新拉取模型目录与倍率。
- **纯 CLI 控制**：终端内嵌 ASCII 二维码，国内站微信 / 企业微信扫码登录；`status` / `refresh` / `serve` 子命令完成全部管理。

---

## 命令总览

```text
workbuddy-gateway [command] [options]

命令:
  serve     启动本地网关（默认命令，不带子命令时等同 serve）
  login     登录并获取 / 更新凭据
  status    查看账号池状态
  refresh   手动刷新所有账号访问令牌
  monitor   前台实时监控：账号表格 + 模型统计附表 + 最近日志
  probe     主动探测账号对指定模型的免费 / 收费属性（需 serve 运行中）
  reset     清空除登录凭据外的全部本地数据，并重新拉取模型与倍率
  version   查看版本信息
  help      查看帮助
```

全局选项（对所有命令可用）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `-addr <ip>` | `127.0.0.1` | 网关监听地址 |
| `-port <port>` | `8317` | 网关监听端口 |
| `-auth <path>` | 自动发现 | 凭据文件路径，支持逗号分隔多个 |
| `-auth-dir <dir>` | 空 | 凭据目录，自动加载目录内所有 `workbuddy*.json` |
| `-api-key <key>` | 空 | 设置后调用网关必须携带 `Authorization: Bearer <key>` |
| `-proxy <url>` | 空 | 上游请求代理，如 `http://127.0.0.1:7890`、`socks5://...` |
| `-verbose` | `false` | 输出详细调试日志 |
| `-intl` | `false` | 仅 `login` 生效：登录国际站 |
| `-reload-interval <sec>` | `5` | 凭据热加载扫描间隔，`0` 关闭 |
| `-models-refresh <min>` | `60` | 模型目录刷新间隔，`0` 关闭 |

---

## serve

启动本地网关，默认命令。

```bash
# 默认监听 127.0.0.1:8317，自动加载当前目录下所有 workbuddy*.json
workbuddy-gateway serve

# 自定义端口与监听地址
workbuddy-gateway serve -port 9000 -addr 0.0.0.0

# 显式指定多个凭据文件（逗号分隔，轮询）
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 目录模式：加载目录内所有 workbuddy*.json
workbuddy-gateway serve -auth-dir ./auths

# 上游走代理 + 开启客户端鉴权 + 详细日志
workbuddy-gateway serve -proxy http://127.0.0.1:7890 -api-key sk-xxx -verbose

# 关闭凭据热加载
workbuddy-gateway serve -reload-interval 0

# 关闭模型目录自动刷新
workbuddy-gateway serve -models-refresh 0
```

启动后提供的端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions`、`/chat/completions` | Chat Completions，支持 SSE 流式与非流式 |
| POST | `/v1/responses`、`/responses` | OpenAI Responses API |
| GET | `/v1/models`、`/models` | 模型列表，响应头 `X-Model-Source` 标注来源 |
| GET | `/health`、`/ping` | 健康检查，返回 `version`、`model_count`、`model_source` |
| POST | `/admin/probe` | 供 `probe` 命令调用，**仅接受回环来源** |
| GET | `/` | 简单文本说明 |

后台任务（`serve` 启动后自动运行）：

| 任务 | 周期 | 说明 |
|---|---|---|
| Token 续期检查 | 5 分钟 | 距过期不足 15 分钟自动刷新 |
| 额度扫描 | 5 分钟 | 每凭据 10 秒超时，超时保留旧值；`剩余=0` 标记付费耗尽 |
| 模型目录刷新 | 60 分钟 | 实时接口 + npm 目录，合并去重后写缓存 |
| 模型价格探测 | 30 分钟检查 / 每模型 12 小时一轮 | 单轮最多 5 个，仅探测需要确认的模型 |
| 每日签到 | 每天 `UTC+8 09:00` | 仅国内站 |
| 状态快照 | 3 秒 | 写 `workbuddy-status.json` 供 `monitor` 读取 |
| 凭据热加载 | 5 秒 | 扫描凭据新增 / 更新 / 删除 |

---

## login

登录并保存凭据。国内站输出终端 ASCII 二维码；国际站在浏览器内完成。

```bash
# 国内站（微信 / 企业微信扫码）
workbuddy-gateway login

# 保存到指定文件（多账号推荐）
workbuddy-gateway login -auth workbuddy2.json

# 国际站（浏览器内完成，邮箱 / 验证码 / SSO）
workbuddy-gateway login -intl
workbuddy-gateway login -intl -auth workbuddy-intl.json
```

说明：

- 默认保存到 `workbuddy.json`；`-auth` 可指定其他路径。
- 国际站凭据写入 `edition: "intl"`，与国内站凭据可混挂在同一账号池。
- 重新登录会覆盖原凭据并自动清除该账号的失效标记，无需重启服务（热加载会生效）。

---

## status

查看账号池状态，包含站点、冷却、额度与 Token 过期时间。

```bash
workbuddy-gateway status
```

输出示例：

```text
================== WorkBuddy 账号池状态 ==================
账号总数: 2

--- 账号 #1 ---
凭据文件:     workbuddy.json
站点:         国内站 (copilot.tencent.com)
用户昵称:     user-a
用户 UID:     uid-xxx
企业 ID:      (个人账号)
认证域名:     www.codebuddy.cn
冷却状态:     可用
Token 状态:   有效
过期时间:     2026-09-22 12:32:07 (剩余 119h30m0s)
```

---

## refresh

立即刷新所有账号的 Access Token（正常情况下由后台每 5 分钟自动检查，无需手动执行）。

```bash
workbuddy-gateway refresh
```

- 成功 / 失败 / 跳过（授权失效）会分别统计。
- 刷新失败若属于授权类错误，会禁用该账号并删除凭据文件。

---

## monitor

前台实时监控，周期刷新展示「账号表格 + 模型统计附表 + 最近日志」，`Ctrl+C` 退出。

```bash
# 必须在 serve 的工作目录执行（读取 workbuddy-status.json）
cd /opt/workbuddy-gateway
workbuddy-gateway monitor

# 附加展示 systemd 服务最近日志（Linux）
workbuddy-gateway monitor -journal workbuddy-gateway

# 附加展示指定日志文件
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# 调整刷新间隔与日志行数
workbuddy-gateway monitor -interval 2 -lines 20
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-interval <sec>` | `3` | 状态刷新间隔 |
| `-journal <svc>` | 空 | 同时展示 `journalctl -u <svc>` 最近日志 |
| `-logfile <path>` | 空 | 同时展示指定日志文件末尾内容 |
| `-lines <n>` | `15` | 每次展示的日志行数 |

**账号表格**

```text
账号池: 共 2 个 | 可用 1 | 冷却 0 | 付费耗尽 1 | 过期 0 | 失效 0
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 序号 | 凭据文件       | 账号       | 站点   | 状态       | Token 有效期        | 总额度   | 已用     | 剩余   | 付费用户   | 免费模型   | 模型冷却   |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 1    | workbuddy.json | user-a     | 国内站 | 可用       | 2026-09-22 12:32:07 | 2300     | 1200     | 1100   | 否         | 1          | 0          |
| 2    | workbuddy2.json| user-b     | 国际站 | 付费耗尽   | 2027-09-05 01:57:00 | 1100     | 1100     | 0      | 否         | 0          | 0          |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
```

状态取值：`可用`、`冷却`、`付费耗尽`、`已过期`、`失效`。

**模型统计附表**

```text
模型统计 (来源 live-api@2026-09-17 14:57):
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+
| 模型                       | 国内倍率         | 国际倍率         | 可用账号 | 请求     | 平均首字(5h)  | 平均总耗时(5h)  |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+
| hy3                        | 0.00x            | 0.00x            | 8        | 3        | 1.9s          | 2.3s            |
| deepseek-v4.1-flash        | 0.03x            | 0.00x            | 5        | 12       | 820ms         | 3.4s            |
| hy4-preview                | 0.00x            | 收费(倍率未知)   | 8        | 4        | 1.3s          | 4.1s            |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+
```

| 列 | 含义 |
|---|---|
| 模型 | 模型 ID |
| 国内倍率 | 国内站生效倍率（`credits` × 促销 factor）；免费显示 `0.00x`，促销过期或接口无有效倍率时显示 `-`，实测确认收费显示 `收费(倍率未知)` |
| 国际倍率 | 国际站同上 |
| 可用账号 | 当前可调度该模型的账号数（已计入账号冷却、模型冷却、模型额度阻断） |
| 请求 | 客户端请求次数 |
| 平均首字(5h) | 最近 5 小时滚动窗口内的平均首字响应时间（TTFT），按小时分桶、自动淘汰过期样本 |
| 平均总耗时(5h) | 最近 5 小时滚动窗口内的平均总耗时 |

---

## probe

免费 / 收费属性按「账号（含站点）+ 模型」学习，只有该账号真正请求过该模型才会写入账本。默认调度优先使用有余额账号，**余额耗尽的账号几乎不会被选中，也就学不到属性**。`probe` 用于主动补课。

> 注意：额度耗尽的账号会被上游整体拒绝（`14018 Credits exhausted`），此时连免费模型也会失败。要验证某模型是否免费，请使用**额度未耗尽**的账号。

```bash
# 探测全部账号，每个账号取模型目录前 5 个模型
workbuddy-gateway probe

# 只探测指定账号
workbuddy-gateway probe -auth workbuddy4.json

# 指定模型
workbuddy-gateway probe -auth workbuddy4.json -models hy3,deepseek-v4.1-flash

# 指定数量上限（默认 5，上限 50）
workbuddy-gateway probe -auth workbuddy4.json -limit 8
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-auth <path>` | 全部账号 | 只探测指定凭据（文件名或路径均可） |
| `-models <m1,m2>` | 目录前几个 | 指定要探测的模型 |
| `-limit <n>` | `5` | 未指定 `-models` 时探测的模型数量，上限 50 |

输出示例：

```text
正在请求 http://127.0.0.1:8317/admin/probe（账号=workbuddy4.json，模型=hy3）...

账号                   站点   模型     结果     credit  tokens  说明
workbuddy4.json        intl   hy3      paid     0.42    820     usage.credit=0.42，收费

汇总: paid=1
```

结果状态：

| 状态 | 含义 |
|---|---|
| `free` | `usage.credit=0` 且 `total_tokens ≥ 100`，已学习为免费 |
| `paid` | `usage.credit > 0`，已学习为收费 |
| `unknown` | 未返回 `credit`，或 `credit=0` 但样本过小 |
| `quota` | `14018` 额度耗尽，记为该账号该模型收费并阻断该模型 |
| `rate_limited` | `6004` 模型级限流，只冷却该模型 |
| `auth_failed` | 授权失效（probe 不会自动禁用账号） |
| `skipped` | 账号失效或无凭据 |
| `error` | 网络 / 协议错误 |

> 原理：`probe` 作为客户端调用运行中服务的 `/admin/probe`。账本保存在 `serve` 进程内存中，独立进程直接写状态文件会被服务快照覆盖，因此探测必须由运行中的服务执行。该接口仅接受回环来源；服务启用 `-api-key` 时同样需要鉴权。

---

## reset

清空**除登录凭据以外**的全部本地数据，并重新拉取模型与倍率。

```bash
workbuddy-gateway reset
```

清理范围：

- `workbuddy-status.json`（账号与模型状态快照）
- `wb-models-cache.json`（模型目录、倍率、价格探测结论）
- `*.disabled` / `*.json.disabled`（授权失效标记）
- `logs/`（运行日志）

保留：`workbuddy*.json` 登录凭据。

清理后会立即重新拉取模型目录与倍率。账号账本同时存在于 `serve` 进程内存中，若服务正在运行，请重启使其同步归零：

```bash
systemctl restart workbuddy-gateway
```

---

## version / help

```bash
workbuddy-gateway version    # 输出 WorkBuddy Local Gateway vX.Y.Z
workbuddy-gateway help       # 输出完整帮助
workbuddy-gateway -v         # 同 version
workbuddy-gateway -h         # 同 help
```

---

## 多账号池

三种配置方式：

```bash
# 方式一（推荐）：自动发现
# 把多个凭据文件放进工作目录，无需任何参数
workbuddy-gateway serve

# 方式二：-auth 逗号分隔
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 方式三：-auth-dir 目录
workbuddy-gateway serve -auth-dir ./auths
```

行为说明：

- **轮询**：请求按 round-robin 在可用账号间分发。
- **429 冷却**：`6004` 只冷却触发模型；无法归因到模型的 429 才进入账号级冷却，冷却到期自动恢复。
- **授权失效**：401/403 类错误禁用账号并删除凭据文件，同时写 `*.disabled` 标记；重新 `login` 后自动恢复。
- **额度耗尽**：`剩余=0` 标记「付费耗尽」，仍可服务已确认免费的模型。
- **热加载**：默认每 5 秒扫描，新增 / 更新 / 删除凭据免重启。
- **串行化**：同一账号请求严格排队，避免并发双发触发风控；不同账号可并行。

---

## 模型列表与倍率

**列表来源**：实时接口 `GET {Base}/v2/enterprises/personal/models` 与 npm 包静态目录，按模型 ID 去重、**接口优先**。

```text
两路都成功  → 合并去重
一路成功    → 使用成功那路
两路都失败  → 使用本地缓存 wb-models-cache.json
失败且无缓存→ 该站点本轮不展示模型（不影响模型调用）
```

**免费站点优先**：若某模型出现「一个站点免费、另一个站点收费」，调度优先使用免费站点的账号，直到该站点账号全部不可用（冷却 / 耗尽 / 失效）才回退到另一站点；若两个站点都免费或都收费（只是倍率不同），则不设优先，保持正常轮询。

**倍率**：

```text
1. 促销生效中：生效倍率 = credits × factor（factor=0 → 0.00x）
2. 促销已过期：接口 credits 不可信（上游常把促销价固化进 credits），
   探测出结果前显示 -，随后由实测决定
3. 模型不在接口目录中：同样交由实测决定
4. 无促销且 credits 有值：直接展示该倍率
```

**价格探测**：由「余额未耗尽」的同站点账号发一次最小请求实测。

```text
探测免费 → 展示 0.00x，并每 12 小时复测确认
探测收费 → 展示 收费(倍率未知)，直到接口重新给出未过期的 0.00x
14018 / 无 usage.credit / 样本过小 → 不覆盖，保持未知
```

探测调度：

| 时机 | 说明 |
|---|---|
| 服务启动 | 启动后约 20 秒执行首轮 |
| 首次 / 重置后 | 单轮最多 30 个，快速补齐结论 |
| 收敛后 | 单轮最多 5 个，每模型 12 小时最多一次 |
| 待探测未清空 | 用 2 分钟短间隔追赶，清空后回到 30 分钟 |
| 目录刷新成功 | 立即触发一轮 |
| 凭据变化 | 立即触发一轮（含「原本没有某站点账号、后来加入」的情况） |

仅探测被实际请求过、或接口明确需要确认的模型，避免无谓消耗额度。

`/v1/models` 响应头 `X-Model-Source` 与 `/health` 的 `model_source` 会标注目录来源。

---

## 客户端接入

网关启动后服务地址为 `http://127.0.0.1:8317/v1`。

curl：

```bash
curl -N -s http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"default-model","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

Python OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8317/v1", api_key="none")
resp = client.chat.completions.create(
    model="default-model",
    messages=[{"role": "user", "content": "写一个快速排序"}],
)
print(resp.choices[0].message.content)
```

DSH（`~/.dsh/settings.yaml`）：

```yaml
llm-pi-ai:
  providers:
    workbuddy-local:
      baseURL: http://127.0.0.1:8317/v1
      apiKeyEnv: LOCAL_API_KEY   # 任意字符串即可
      api: openai-completions
      models:
        - id: default-model
          contextWindow: 1000000
          maxTokens: 128000
```

## 思考等级传参

网关对外暴露的是 **OpenAI 兼容**的思考等级参数，无论下游客户端来自哪个 agent 生态，都按 OpenAI 规范传参即可；网关负责翻译成上游认识的扁平字段。

| 入口 | 客户端可传（OpenAI 兼容） | 上游实际收到 |
|---|---|---|
| `POST /v1/chat/completions` | 顶层 `reasoning_effort` | 扁平 `reasoning_effort` |
| `POST /v1/chat/completions` | 嵌套 `reasoning.effort` / `reasoning.summary`、`text.verbosity` | 扁平 `reasoning_effort` / `reasoning_summary` / `verbosity` |
| `POST /v1/responses` | `reasoning.effort` / `reasoning.summary`、`text.verbosity` | 同上（由网关摊平） |

归一化规则：

| 输入 | 处理 | 说明 |
|---|---|---|
| `none` / `off` / `disabled` / `""` / `null` | 删除字段 | 语义为关闭推理 |
| `auto` / `default` | 删除字段 | 交由上游按模型默认档位决定 |
| 其他非空字符串 | 转小写、去空白后原样转发 | 如 `HIGH` → `high`、`" high "` → `high` |
| 未传 | 不发送 | **绝不注入** |

档位取值本身不做白名单校验——各模型支持的档位由上游裁决（不支持时返回 `11150 not supported by the current model`）。常见取值：`minimal` / `low` / `medium` / `high` / `xhigh` / `max`。

> **为什么不注入**：官方客户端发往同一上游（`www.workbuddy.ai`）的报文不含 `reasoning_summary`，且强行注入思考字段会触发上游内容安全拦截（`code 11102` / `11128`）。网关因此只做「客户端显式传参 → 归一化转发」，不凭空补字段。

### 上游字段语义（实测）

以下为对 `https://www.workbuddy.ai/v2/chat/completions` 的实测结论（`stream` 必须为 `true`，非流式一律 `code 11101`）：

| 字段 | 上游行为 |
|---|---|
| `reasoning_effort`（扁平） | 唯一生效的思考开关。取值大小写/空白敏感，`HIGH`、`" high "` 均 `400 11150`；`off` / `disabled` / `auto` 同样 `11150`。`none` 被接受但**仍开启推理**（prompt_tokens 18→43），与 OpenAI「不推理」语义相反，故网关将其归一化为删除字段。 |
| `reasoning.effort`（嵌套） | **被完全忽略**（推理根本不开启），因此网关必须摊平。 |
| `reasoning_summary`（扁平） | 上游不校验取值（`auto` / `concise` / `detailed` / `bogus` / `none` / `null` 均 `200`），原样透传，网关不改写其取值。 |
| `verbosity`（扁平） | 上游不校验取值，与推理开关正交（无 `reasoning_effort` 时也接受）。 |
| `max_thinking_tokens` | 上游接受（`MAX_THINKING_TOKENS` 环境变量经客户端设置），网关透传。 |

---

## 各平台部署

### Windows

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-windows-amd64.exe`。
2. 在 PowerShell / CMD 中进入文件所在目录：

   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```

3. 开机自启：`Win+R` → `shell:startup`，把 exe 快捷方式放入启动文件夹，并在快捷方式“目标”后追加 `serve`。

### Linux

```bash
# x86_64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway

# ARM64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway

workbuddy-gateway login
workbuddy-gateway serve -addr 127.0.0.1 -port 8317
```

#### systemd 服务（推荐）

创建 `/etc/systemd/system/workbuddy-gateway.service`：

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

部署与启动：

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/
sudo /opt/workbuddy-gateway/workbuddy-gateway login
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway
sudo journalctl -u workbuddy-gateway -f
```

> `WorkingDirectory` 决定自动发现的凭据目录。把多个凭据文件放进该目录即可组成账号池，新增 / 更新 / 删除会自动热加载。

常用运维：

```bash
sudo systemctl restart workbuddy-gateway
sudo systemctl stop workbuddy-gateway
sudo systemctl disable workbuddy-gateway
```

对外开放时（例如局域网其他设备）把 `-addr` 改为 `0.0.0.0`，并**务必**设置 `-api-key`：

```ini
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317 -api-key sk-changeme
```

### macOS

1. 下载 `workbuddy-gateway-darwin-arm64`（Apple Silicon）或 `workbuddy-gateway-darwin-amd64`（Intel）。
2. 移除隔离属性：

   ```bash
   chmod +x workbuddy-gateway-darwin-arm64
   xattr -d com.apple.quarantine workbuddy-gateway-darwin-arm64 2>/dev/null || true
   ```

3. 登录与启动：

   ```bash
   ./workbuddy-gateway-darwin-arm64 login
   ./workbuddy-gateway-darwin-arm64 serve
   ```

4. 开机自启（launchd）：创建 `~/Library/LaunchAgents/com.workbuddy.gateway.plist`：

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>com.workbuddy.gateway</string>
     <key>ProgramArguments</key>
     <array>
       <string>/path/to/workbuddy-gateway-darwin-arm64</string>
       <string>serve</string>
       <string>-port</string><string>8317</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>WorkingDirectory</key><string>/path/to/workbuddy-gateway-dir</string>
   </dict>
   </plist>
   ```

   ```bash
   launchctl load ~/Library/LaunchAgents/com.workbuddy.gateway.plist
   ```

## 客户端指纹对齐

网关向上游发起请求时，会逐项对齐官方客户端（WorkBuddyAI desktop **5.5.2** + bundled CLI **2.137.1**）的请求头，避免因自造字段或缺失字段形成可静态识别的机器特征。

**对齐基线**：官方客户端发往 `/v2/chat/completions` 的真实请求头（抓包实测，共 38 项，含 3 项传输层头）。

### 关键结论与依据

| 项目 | 处理方式 | 依据 |
|---|---|---|
| `X-Client-ID` / `X-Client-Version` | **移除** | 客户端全量安装目录（含 `app.asar`）字节级 0 命中；旧值 `codebuddy-cli` / `2.143.1` 属网关自造 |
| `Origin` / `Referer` | **移除** | Node/Electron 运行时无浏览器语义，客户端实测不发送；发送反而是不一致特征 |
| `Accept` | `application/json`（单一值） | 客户端实测非浏览器默认的 `*/*` 列表 |
| `User-Agent` | `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1` | 客户端 UA 组装口径：`${platform}/${ver} ${name} AI/${ver} CLI/${cliVer}` |
| `X-Request-ID` | 32 位小写 hex，每请求唯一 | 客户端 `generateUUUID().replace(/-/g,"")` |
| `X-Trace-ID` | 与 `X-Request-ID` **解耦**，为 OTel traceId（全链路同值） | 实测 `X-Trace-ID` == `traceparent` 的 traceId，且同一 session 内稳定 |
| `traceparent` / `b3` / `X-B3-*` | 补齐（OTel + B3 双份） | 客户端同时发送两套传播头 |
| `X-Conversation-*` / `X-Root-Request-ID` / `X-Agent-*` | 补齐 | 客户端会话与 Agent 语义骨架，缺失即特征 |
| `x-stainless-*` | 合成（7 项，**全小写键名**） | 客户端 chat 请求由 bundled OpenAI Node SDK 发出，上游始终可见该指纹族；网关用 Go `net/http` 发起，不合成即为缺失。SDK 以全小写发出该族头 |
| `Accept-Encoding` | **不发送** | 客户端实测不发送；Go transport 默认自动补 `gzip`，故设 `DisableCompression: true` |
| `X-No-*` | 仅未鉴权时成组发送，值 `"true"` | 实测两种形态：已鉴权（`Authorization`+`X-User-Id`+`X-Domain`+`X-Product`，无任何 `X-No-*`）；未鉴权仅 `X-No-*` |
| `X-Refresh-Token` | 仅刷新链路发送，chat 不发送 | 客户端源码中该头只出现在 `auth/token/refresh` 与 `account/switch` 调用 |
| `x-codebuddy-request` | **不合成**，仅在客户端自带时透传 | 它是客户端**本地网关**安全头（源码 `GatewayLocalServer` 模块 `withSecurityHeader` 注入），语义上非上游必需 |

### 字节级对齐（v1.8.4 新增）

「头集合一致」不等于「字节一致」。以下三项偏差只在比对**线上原始字节**时可见，仅靠 `http.Header` 层面的比对无法发现。

| 偏差 | 现象 | 处理方式 |
|---|---|---|
| **键名大小写被规范化** | Go `http.Header.Set` 经 `textproto.CanonicalMIMEHeaderKey` 改写键名：`X-Request-ID`→`X-Request-Id`、`X-IDE-Type`→`X-Ide-Type`、`X-B3-TraceId`→`X-B3-Traceid`、`x-requested-with`→`X-Requested-With` | 新增 `setHeaderExact`/`getHeaderExact` 直接读写 map 键，复刻客户端原始大小写 |
| **请求体键序被字典序化** | 经 `map[string]any` 中转再 `json.Marshal` 会按键名排序输出，`messages` 跑到 `model` 之前；客户端 `JSON.stringify` 按属性插入顺序输出，`model` 恒在首位 | 新增保序 JSON 层（`jsonorder.go`），解析/修改/序列化全程保持键序 |
| **`<` `>` `&` 被 HTML 转义** | `json.Marshal` 默认把 `<` 转成 `\u003c`；客户端正文含 `<user_query>` / `<content_policy>` 等标签，`JSON.stringify` 原样输出 | 自实现转义（仅处理 `"` `\` 与控制字符），与 `JSON.stringify` 口径一致 |

**为什么必须保序保真**：这三项都无需解析语义、仅比对字节即可判定，是极强的机器特征。此外保序层用 `json.Number` 保留原始数字字面量，避免超出 2^53 的整数经 `float64` 中转被静默改写。

**关于 HTTP/2**：实测客户端走 Node 全局 `fetch`（undici），**默认 HTTP/1.1**（抓包中带 `Host` / `Connection: keep-alive`，这两个头在 HTTP/2 中非法）。故网关**不启用** HTTP/2 —— 启用反而会引入偏差，且 h2 的 HPACK 头压缩会让键名大小写与顺序信息消失。

### 会话作用域对齐（v1.8.5 新增）

链路 ID 不是「每请求一套」，而是**两个作用域**。客户端源码（`chat_headers_full` 实测片段）写得很直白：

```js
ey[CONVERSATION_ID_HEADER]         = ew.id                     // 会话
ey[CONVERSATION_REQUEST_ID_HEADER] = ew.conversationRequestId  // 会话
ey[CONVERSATION_MESSAGE_ID_HEADER] = ew.messageId              // 每请求
ey[REQUEST_ID_HEADER]              = ew.messageId              // 每请求
```

| 头 | 作用域 | 实测不变量（9 条会话 / 19 次请求） |
|---|---|---|
| `X-Conversation-ID` | 会话 | 会话内恒定，带连字符 UUID |
| `X-Conversation-Request-ID` | 会话 | 会话内恒定，32 位 hex |
| `X-Root-Request-ID` | 会话 | 会话内恒定，**等于** `X-Conversation-Request-ID` |
| `X-Trace-ID` | 会话 | 会话内恒定，**等于** `X-Conversation-Request-ID` |
| `X-B3-TraceId` | 会话 | 等于 `X-Trace-ID` |
| `X-Request-ID` | 每请求 | 每请求变化 |
| `X-Conversation-Message-ID` | 每请求 | 每请求变化，**等于** `X-Request-ID` |
| `traceparent` / `b3` / `X-B3-SpanId` / `X-B3-ParentSpanId` | 每请求 | span 部分每请求变化 |

网关此前把**四个会话级头也按请求新生成**，等于「同一会话的连续请求携带互不相同的会话链路 ID」—— 只要比对同一会话的两次请求即可判定，是稳定的机器特征。同时 `X-Conversation-Request-ID` 被错设为 `X-Request-ID`，单次请求内部即与客户端口径不符（前者应是会话级，后者是每请求级）。

**实现**：会话级 ID 由下游会话键（`X-Conversation-ID`，缺失时回退 `X-Conversation-Request-ID`）经 `SHA-256` 派生，而非「随机生成后缓存」。

| 方案 | 跨请求共享状态 | 并发 | TTL / 容量 | 重启后 |
|---|---|---|---|---|
| 随机生成 + map 缓存 | 需要 | 需加锁 | 需上限与过期 | 会话 ID 变化 |
| **哈希派生（采用）** | **无** | **无竞争** | **不需要** | **稳定** |

派生值取哈希前 16 字节（32 位 hex），与客户端 `X-Conversation-Request-ID` 同形态，且不泄露会话键本身（客户端传入 32 位 hex，网关发出的同样是 32 位 hex）。

**下游未携带会话标识时**（如第三方 OpenAI SDK 直连，只有 `messages` 没有会话头）：不臆造会话关联，会话级 ID 退回每请求新值 —— 与旧行为一致。把无会话语义的请求错误归并到同一条链路，反而会制造出「大量不相关请求共享同一 traceId」的异常模式。

**链路 ID 只有一个写入方**：这些头之间存在客户端保证的相等关系（`X-Trace-ID` == `X-Conversation-Request-ID` == `X-Root-Request-ID` == `X-B3-TraceId` == `traceparent` 的 trace 段 == `b3` 的 trace 段；span 段同理；`X-Request-ID` == `X-Conversation-Message-ID`）。

因此它们**不在透传白名单**里，而是由 `resolveLinkIDs` 统一决定：**下游给出同组任一项即采用该值，并据此补齐同组其余头**。

| 下游只提供 | 网关行为 |
|---|---|
| 仅 `traceparent` | 其 trace 段传播到 `X-Trace-ID` / `X-Conversation-Request-ID` / `X-Root-Request-ID` / `X-B3-TraceId`，span 段传播到 `b3` / `X-B3-SpanId` |
| 仅 `b3` | 同上，并取回 `X-B3-ParentSpanId` |
| 仅 `X-Request-ID` | 同步为 `X-Conversation-Message-ID` |
| 仅 `X-Conversation-Request-ID` | 原样采用，并补一个形态自洽的 `X-Conversation-ID` |
| 都不提供 | 全部新生成，组内自洽 |

逐头原样透传是错的：下游只给一部分时，网关会为其余头另生成值，产出**客户端不可能产生的组合**（如 `X-B3-SpanId` 与 `traceparent` 的 span 段不等）—— 这比单纯缺失更显眼。

> 补充：`X-Conversation-ID` 由客户端传入时会被原样采用，哈希派生只用于**下游缺失时**的合成，不会覆盖客户端真实会话键。

### 透传与合成的边界

下游客户端自带的身份头（`X-Agent-Intent`、`X-IDE-*`、`User-Agent` 等）**优先透传**，网关仅在缺失时回退合成值。链路 ID 类头由 `resolveLinkIDs` 单独处理（见上一节）。

鉴权类头（`Authorization`、`X-User-Id`、`X-Enterprise-Id`、`X-Domain`、`X-Product`、`X-Refresh-Token`）**一律由账号池生成，不接受下游覆盖**，以防串号或泄权。

### 回归保障

`main_test.go` 中的 `TestUpstreamFingerprintMatchesRealClient` 以真实抓包基线逐项校验（不多、不少、值一致、**键名大小写逐字一致**、链路 ID 形态与作用域自洽）；`TestSmokeEndToEndUpstreamHeaders` 驱动完整请求链路（含生产 transport）抓取实际发出请求头，覆盖单测无法触及的传输层差异。

会话作用域与链路 ID 自洽性由四个端到端用例断言（均驱动真实 `handleChatCompletions` → `upstreamChat` → 生产 transport 链路）：

- `TestSessionScopedLinkIDsAreStableWithinConversation` —— 同会话连续两次请求：四个会话级头逐字恒定，`X-Request-ID` / `X-Conversation-Message-ID` / 两个 span ID 各不相同；
- `TestSessionScopedLinkIDsDifferAcrossConversations` —— 不同会话不共享会话级 ID；
- `TestNoConversationKeyFallsBackToPerRequestIDs` —— 无会话键时不臆造关联，退回每请求新值；
- `TestConversationRequestIDOnlyIsAdoptedVerbatim` / `TestPartialLinkHeadersAreCompletedCoherently` —— 下游只提供部分链路头时，按不变量补齐同组伙伴（仅 `traceparent` / 仅 `b3` / 仅 `X-Request-ID` / 都不提供 四种情形）。

字节级偏差由裸 TCP 监听（`captureUpstreamWire`，不经 `net/http` 解析器重新规范化）断言：

- `TestUpstreamWireHeaderCasingMatchesClient` —— 线上键名大小写逐字匹配客户端基线，且规范化形态绝不出现在线上；
- `TestUpstreamWireBodyMatchesClientEncoding` —— 线上请求体以 `model` 开头、`messages` 在其后、`<` `>` `&` 未转义、无尾随换行；
- `TestOrderedJSONContract` / `TestResponsesBodyKeyOrder` —— 保序层的键序、转义、数字精度与非法输入边界。

## 安全提示

- `workbuddy*.json` 包含真实访问凭据（Access Token / Refresh Token），**严禁提交到 Git 或公开分享**；本仓库 `.gitignore` 已排除。
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时改用 `-addr 0.0.0.0` 并配合 `-api-key`，或置于反向代理之后。
- `/admin/probe` 仅接受回环来源调用。
- 不再需要某账号授权时，删除对应凭据文件并在 CodeBuddy 控制台撤销授权。

## 命令行速查

```
命令:  serve | login | status | refresh | monitor | probe | reset | version | help
选项:  -addr <ip> · -port <port> · -auth <path> · -auth-dir <dir> · -intl · -reload-interval <sec> · -api-key <key> · -proxy <url> · -verbose
       （-intl 仅 login 生效：登录国际站 www.workbuddy.ai；-reload-interval 默认 5，0 关闭热加载）
       另有 -models-refresh <min>（模型目录刷新间隔，默认 60，0 关闭）
monitor: -interval <sec> · -journal <svc> · -logfile <path> · -lines <n>
probe:   -auth <path> · -models <m1,m2> · -limit <n> · -addr/-port（需与运行中的 serve 一致）
reset:   清空除登录凭据外的本地数据（状态 / 缓存 / 日志 / 失效标记）
```

---

## 从源码构建

需要 Go 1.26+：

```bash
git clone https://github.com/CangShui/workbuddy-gateway.git
cd workbuddy-gateway

go vet ./...
go test ./...

# 当前平台
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o workbuddy-gateway .

# 交叉编译示例
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-linux-amd64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-windows-amd64.exe .
```

---

## 免责声明

本项目仅用于个人学习与技术研究。腾讯 CodeBuddy（含国内站与国际站 workbuddy.ai）的接口协议与风控策略可能随时变化；请遵守腾讯服务条款，自行承担使用风险。本仓库不包含任何官方未公开的密钥或凭据。
