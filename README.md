# WorkBuddy Local Gateway
<img width="1378" height="328" alt="image" src="https://github.com/user-attachments/assets/ae5a3b1c-a46c-4c5e-8fd3-05a7e4c23e74" />



基于腾讯 **CodeBuddy** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。无 Web UI，仅通过命令行（CLI）完成登录、凭据续期与服务控制。

**同时支持两个上游站点**（同一套协议，凭据按站点隔离，账号池可混挂轮询）：

| 站点 | 上游 | 登录方式 | 登录命令 |
|---|---|---|---|
| 国内站 | `copilot.tencent.com` | 微信 / 企业微信扫码 | `login` |
| 国际站 | `www.workbuddy.ai` | 浏览器内登录（邮箱 / 验证码 / SSO 等） | `login -intl` |

## 核心特性

- **国内 / 国际双站反代**：国际站 `www.workbuddy.ai` 与国内站走同一套 `/v2/plugin/*` 协议，凭据文件通过 `edition` 字段区分站点，刷新 / 对话自动路由到各自上游。
- **模型完全透传**：客户端（OpenAI SDK、Cursor、Claude Code、DSH 等）传什么 `model`（如 `default-model`、`gpt-5.5`、`gpt-5.3-codex`、`gemini-3.1-pro`、`deepseek-v3-2-volc`、`kimi-k2.5`…），网关原样透传至腾讯上游，无白名单限制；`/v1/models` 返回官方客户端实际可用的模型清单。
- **纯 CLI 控制**：终端内嵌 ASCII 二维码，国内站微信 / 企业微信扫码登录；`status` / `refresh` / `serve` 子命令完成全部管理。
- **多账号池 + 轮询负载均衡**：支持同时挂载多个 CodeBuddy 账号（`-auth` 逗号分隔或 `-auth-dir` 目录），请求按轮询（round-robin）均匀分配到各账号，保持多账号额度使用一致；**国内站与国际站账号可混挂在同一池中**。
- **凭据热加载（免重启）**：`serve` 运行期间自动扫描凭据来源（默认每 5 秒，`-reload-interval` 可调）：新增凭据文件自动入池、重新登录/手动更新凭据原地生效、删除凭据自动移出，全程无需重启服务。
- **429 频率限制自动冷却**：任一账号触发上游频率限制（HTTP 429 / code 6004，中英文消息均可识别）时，自动解析消息中的重置时间（中文如 `将在 2026-09-04 07:48:15 UTC+8 重置`，英文如 `will reset at 2026-09-05 01:57:00 UTC+8`），将该账号屏蔽至重置时间；冷却期间自动改用其他账号代偿，冷却到期自动恢复。
- **授权失效自动禁用**：账号授权过期、被撤销或令牌刷新失败（HTTP 401/403 / invalid token / 登录已过期）时，自动将该账号**禁止调度并删除凭据文件**，同时写入持久化失效标记；`status` / 启动日志会明确提示该账号失效原因与重新登录命令，重新 `login` 后自动恢复调度。
- **后台自动续期**：运行期间每 5 分钟检查所有账号 Token，距过期不足 15 分钟自动刷新并持久化回各自凭据文件。
- **OpenAI 兼容协议**：`POST /v1/chat/completions`（SSE 流式 + 非流式聚合）、`POST /v1/responses`（OpenAI Responses API，支持流式语义事件、非流式与 function tools）、`GET /v1/models`、`GET /health`。
- **深度思考透传规则**：仅当客户端显式请求 `reasoning_effort` 时转发；绝不强制注入，避免触发上游内容安全策略。
- **上游指纹对齐**：请求头逐项对齐官方客户端（详见[客户端指纹对齐](#客户端指纹对齐)），消除自造头与缺失头构成的可识别特征。
- **会话结构自动归一化**：自动保证首条消息为 `system`，修复部分非 harness 客户端（以 `assistant` 续写或以 `tool` 回传工具结果开头）触发的上游 `first message is not system prompt` (code 11128) 报错；同时兼容 OpenAI 新版 `developer` 角色。
- **单账号串行化**：同一账号请求自动排队，避免并发双发触发上游风控；不同账号之间可并行。

## 快速上手

### 1. 扫码登录

首次使用或凭据失效时执行（需微信 / 企业微信扫码）：

```bash
# Windows
.\workbuddy-gateway-windows-amd64.exe login

# Linux / macOS
./workbuddy-gateway-linux-amd64 login
./workbuddy-gateway-darwin-arm64 login

#指定保存凭据到某个目录（多账号模式下建议使用）
workbuddy-gateway login -auth /opt/workbuddy-gateway/workbuddy001.json
```

终端将打印 ASCII 二维码与浏览器直达链接，扫码后凭据自动保存到当前目录 `workbuddy.json`（请勿提交到代码仓库）。

> 提示：建议在 **CodeBuddy 控制台（网页端）** 扫码登录获取更高权限评级的 Token；若使用 `login` 命令扫码，账号渠道可能受限（`azp=invite`）。

### 1b. 登录国际站（workbuddy.ai）

```bash
# 登录国际站账号（保存到指定文件以便与国内站账号区分）
workbuddy-gateway login -intl
workbuddy-gateway login -intl -auth workbuddy-intl.json
```

国际站登录流程与国内站不同：网关生成登录链接后，**需要你在浏览器里完成登录**（支持邮箱、验证码、SSO 等方式），流程如下：

1. 终端打印二维码与直达链接（手机扫码或电脑打开均可）；
2. 在浏览器中完成登录，页面显示 **Login Successful** 即可（无需理会跳转 App 的提示）；
3. 网关自动轮询拿到 Token 并保存到凭据文件（`edition: "intl"`），等待窗口 15 分钟。

启动服务无需任何额外参数——凭据文件内已记录站点标识，`serve` / `refresh` 会自动路由到对应上游；**登录成功后新账号由凭据热加载自动加入运行中的账号池，无需重启服务**：

```bash
workbuddy-gateway serve    # 自动发现目录下所有 workbuddy*.json（含国际站账号，混挂轮询）
```

> 说明：国际站 `www.workbuddy.ai` 与国内站 `copilot.tencent.com` 是同一协议的两套部署（登录 platform 为 `workbuddy-ai`，等待授权时轮询返回 code 11217）。国际站账号是否可用各模型、额度与风控策略由腾讯国际站侧决定。

### 2. 查看凭据状态

```bash
workbuddy-gateway status
```

### 3. 启动本地网关

```bash
# 默认监听 127.0.0.1:8317
workbuddy-gateway serve

# 自定义端口 / 地址 / 详细日志
workbuddy-gateway serve -port 8317 -verbose

# 通过出口代理转发（可选，降低上游风控概率）
workbuddy-gateway serve -proxy http://127.0.0.1:7890

# 开启客户端鉴权
workbuddy-gateway serve -api-key sk-localsecret
```

## 多账号池与 429 自动冷却

### 配置多个账号

网关支持同时挂载多个 CodeBuddy 账号，请求按**轮询（round-robin）**方式均匀分发，保持各账号额度消耗一致。三种配置方式：

```bash
# 方式一（推荐）：自动发现 — 把多个凭据文件放进工作目录即可，无需任何参数
# 网关启动时自动加载当前目录下所有 workbuddy*.json（如 workbuddy.json、workbuddy2.json…）
cd /opt/workbuddy-gateway
workbuddy-gateway serve -addr 0.0.0.0 -port 8317
# 启动日志会显示: 已就绪账号池: 2 个账号 (有效 2, 失效 0)

# 方式二：-auth 逗号分隔多个凭据文件
workbuddy-gateway serve -auth workbuddy.json,workbuddy-2.json,workbuddy-3.json

# 方式三：-auth-dir 指定凭据目录（自动加载目录下所有 workbuddy*.json）
mkdir -p auths
workbuddy-gateway login -auth auths/workbuddy-1.json   # 依次为每个账号扫码登录
workbuddy-gateway login -auth auths/workbuddy-2.json
workbuddy-gateway serve -auth-dir ./auths
```

### 429 频率限制自动冷却

当某个账号触发上游频率限制（HTTP 429，消息形如 `您的使用量已超出频率限制，将在 2026-09-04 07:48:15 UTC+8 重置`）时，网关会：

1. **自动解析消息中的重置时间**，立即将该账号屏蔽（冷却）至该时间点；
2. **自动改用下一个可用账号重试**当前请求（代偿），无需客户端干预；
3. 冷却期间该账号不参与轮询，**冷却到期后自动恢复**；
4. 若消息中无法解析重置时间，默认冷却 60 秒后重试；
5. 当所有账号均处于冷却状态时，返回 HTTP 429 并附上最早解封时间。

```bash
# 查看各账号状态（含冷却状态与解封时间）
workbuddy-gateway status

# 手动刷新所有账号令牌
workbuddy-gateway refresh
```

### 授权失效自动禁用

当账号出现以下任一情况时，网关会**自动禁用该账号调度并删除其凭据文件**：

- 令牌刷新失败且返回授权类错误（HTTP 401/403、`invalid token`、`unauthorized`、`登录已过期` 等）；
- 上游请求返回 401/403（token 被撤销或已过期）；
- 凭据缺少 RefreshToken 且已无法刷新。

处理流程：

1. 将该账号标记为 **授权失效**，立即移出轮询调度；
2. **删除对应的凭据文件**（如 `workbuddy.json`），并写入持久化失效标记（`workbuddy.json.disabled`）；
3. 控制台（`status` / 启动日志 / 运行日志）明确显示失效原因，并给出重新登录命令；
4. 失效期间其余账号正常代偿；**重新执行 login 后自动清除失效标记并恢复调度**。

```bash
# 查看失效账号与原因
workbuddy-gateway status
# 输出示例：
# --- 账号 #1 ---
# 凭据文件:     workbuddy-2.json
# 站点:         国内站 (copilot.tencent.com)
# 用户昵称:     tester
# 用户 UID:     uid-xxx
# 账号状态:     授权失效（禁止调度）
# 失效原因:     令牌刷新失败 (HTTP 401): invalid token
# 处理建议:     凭据文件已删除，请重新执行: workbuddy-gateway login -auth workbuddy-2.json

# 失效账号重新登录后自动恢复
workbuddy-gateway login -auth workbuddy-2.json
workbuddy-gateway status   # 该账号恢复为可用
```

### 与单账号模式的兼容性

- 不传 `-auth` / `-auth-dir` 时，自动发现当前工作目录下所有 `workbuddy*.json`；目录中只有一个凭据文件时行为与旧版完全一致；
- 只有一个账号时，请求始终使用该账号，429 冷却逻辑同样生效（冷却期间请求将返回 429 提示）；
- 账号之间使用独立的串行锁：同一账号请求严格排队，不同账号可并行，兼顾风控与吞吐。

### 凭据热加载（免重启增减账号）

`serve` 运行期间默认每 5 秒扫描一次凭据来源（`-reload-interval` 可调，设为 `0` 关闭），自动完成：

- **新增账号**：把新的 `workbuddy*.json` 放进凭据目录（或对运行中的网关执行 `login -auth 新文件.json`），几秒内自动加入轮询池；
- **重新登录 / 更新凭据**：对已有凭据文件重新 `login` 后，网关原地替换凭据并自动恢复调度（含曾因授权失效被禁用的账号）；
- **删除账号**：删除凭据文件即自动移出轮询池（已写入失效标记的账号保留提示，重新登录后自动恢复）。

日志示例：

```text
[Reload] 发现新账号凭据 workbuddy4.json（国际站），已自动加入账号池
[Reload] 账号 workbuddy2.json 重新登录成功，已自动恢复调度
[Reload] 凭据文件 workbuddy3.json 已删除，已移出账号池
```

## 前台实时监控 (monitor)

网关以 systemd / 后台方式运行时，可用 **`monitor` 命令在前台实时查看所有账号的最新状态与最近日志**（Ctrl+C 退出）：

```bash
# 基本用法：每 3 秒刷新展示账号池状态（可用/冷却/过期/失效 + Token 完整有效期）
cd /opt/workbuddy-gateway        # 必须与 serve 同一工作目录（读取 workbuddy-status.json）
workbuddy-gateway monitor

# 自定义刷新间隔（秒）
workbuddy-gateway monitor -interval 2

# 同时展示 systemd 服务最近日志（Linux）
workbuddy-gateway monitor -journal workbuddy-gateway

# 或展示指定日志文件最近内容
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# 控制每次展示的日志行数
workbuddy-gateway monitor -journal workbuddy-gateway -lines 8
```

展示内容（实时刷新）：

```text
================ WorkBuddy 实时监控 ================
按 Ctrl+C 退出 | 状态文件: workbuddy-status.json
---------------------------------------------------------------
更新时间: 2026-09-04 09:35:12
账号池: 共 2 个 | 可用 1 | 冷却 0 | 额度耗尽 1 | 过期 0 | 失效 0
+------+----------------------+--------------------------+----------+------------+---------------------+------------+------------+------------+------------+
| 序号 | 凭据文件             | 账号                     | 站点     | 状态       | Token 有效期        | 总额度     | 已用       | 剩余       | 付费用户   |
+------+----------------------+--------------------------+----------+------------+---------------------+------------+------------+------------+------------+
| 1    | workbuddy.json       | Abandon                  | 国内站   | 可用       | 2026-09-10 20:43:00 | 2000       | 1500       | 500        | 否         |
| 2    | workbuddy2.json      | 啊水                     | 国际站   | 额度耗尽   | 2027-09-05 01:57:00 | 1100       | 1100       | 0          | 否         |
+------+----------------------+--------------------------+----------+------------+---------------------+------------+------------+------------+------------+
最近日志 (journalctl -u workbuddy-gateway):
  9月 04 09:34:31 ... [Cooldown] 账号 workbuddy2.json 触发频率限制...
---------------------------------------------------------------
```

> 原理：`serve` 后台每 3 秒（及状态变化时）将账号池实时状态原子写入同目录 `workbuddy-status.json`，`monitor` 前台读取该文件并周期刷新展示；Token 每 5 分钟检查一次，距离过期不足 15 分钟时自动刷新；账号额度在服务启动、凭据池发生变化时立即查询，此后每 1 分钟更新。新一轮扫描开始时会取消上一轮尚未完成的请求，避免扫描堆积。额度支持小数并按上游原值展示；额度为 0 的账号冻结调度，扫描发现剩余额度大于 0 后自动恢复。运行日志同时写入 `logs/gateway-YYYY-MM-DD.log`，也可通过 `journalctl` 查看。额度表格中的数值来自用户中心只读计费接口；首次查询失败时显示 `-`。

## 客户端接入

网关启动后服务地址为 `http://127.0.0.1:8317/v1`。

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

DSH (`~/.dsh/settings.yaml`)：

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

## 各平台使用方法

### Windows

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-windows-amd64.exe`。
2. 在 PowerShell / CMD 中进入文件所在目录：
   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```
3. 如需开机自启：`Win+R` → `shell:startup`，将 exe 的快捷方式放入启动文件夹即可（命令行加 `serve` 参数需通过快捷方式"目标"追加）。

### Linux (amd64 / arm64)

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载对应架构二进制，赋执行权限并放入 PATH：
   ```bash
   # x86_64
   wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
   sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway
   # ARM64 (树莓派 / 飞腾 / Apple silicon 云主机等)
   wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
   sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway
   ```
2. 登录并启动：
   ```bash
   workbuddy-gateway login      # 终端二维码扫码（可用 tmux 保持）
   workbuddy-gateway serve -addr 127.0.0.1 -port 8317
   ```

#### Linux 注册为 systemd 服务（推荐，开机自启 + 崩溃自动拉起）

创建 `/etc/systemd/system/workbuddy-gateway.service`：

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy/Hunyuan OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 二进制与 workbuddy.json 所在目录；请按实际部署路径修改
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 127.0.0.1 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

部署文件：

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/      # 对应架构的二进制
# 首次登录（会生成 workbuddy.json）
sudo /opt/workbuddy-gateway/workbuddy-gateway login
# 登录第二个账号（可选）：生成 workbuddy2.json
sudo /opt/workbuddy-gateway/workbuddy-gateway login -auth /opt/workbuddy-gateway/workbuddy2.json
```

> **多账号自动发现**：服务通过 `WorkingDirectory` 固定在 `/opt/workbuddy-gateway`，
> 无需修改 ExecStart——把多个凭据文件（`workbuddy.json`、`workbuddy2.json`…）放进该目录，
> 即可自动组成轮询池；凭据热加载会让新增/更新/删除的凭据文件免重启生效。确认方式：
> ```bash
> sudo journalctl -u workbuddy-gateway | grep 账号池
> # 输出示例: 已就绪账号池: 2 个账号 (有效 2, 失效 0)
> ```

启用并启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway     # 查看状态
sudo journalctl -u workbuddy-gateway -f     # 查看实时日志
```

常用运维命令：

```bash
sudo systemctl restart workbuddy-gateway    # 重启（如更换凭据后）
sudo systemctl stop workbuddy-gateway       # 停止
sudo systemctl disable workbuddy-gateway    # 取消开机自启
```

如需对外开放（例如给局域网其他设备使用），将 `-addr` 改为 `0.0.0.0`，**并务必**配合 `-api-key` 设置访问密钥：

```bash
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317 -api-key sk-changeme
```

### macOS (Apple Silicon / Intel)

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-darwin-arm64`（M 系列）或 `workbuddy-gateway-darwin-amd64`（Intel）。
2. 首次运行需移除隔离属性：
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

- `workbuddy.json` 包含真实 CodeBuddy 访问凭据（Access Token / Refresh Token），**严禁提交到 Git 仓库或公开分享**；本仓库 `.gitignore` 已将其排除。
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时请改用 `-addr 0.0.0.0` 并配合 `-api-key` 鉴权，或置于反向代理（如 nginx）之后。
- 若不再需要某账号的授权，请删除对应 `workbuddy.json` 并在 CodeBuddy 控制台撤销应用授权。

## 命令行速查

```
命令:  serve | login | status | refresh | monitor | version | help
选项:  -addr <ip> · -port <port> · -auth <path> · -auth-dir <dir> · -intl · -reload-interval <sec> · -api-key <key> · -proxy <url> · -verbose
       （-intl 仅 login 生效：登录国际站 www.workbuddy.ai；-reload-interval 默认 5，0 关闭热加载）
monitor: -interval <sec> · -journal <svc> · -logfile <path> · -lines <n>
```

## 从源码构建

需要 Go 1.26+：

```bash
git clone https://github.com/CangShui/workbuddy-gateway.git
cd workbuddy-gateway
CGO_ENABLED=0 go build -ldflags="-s -w" -o workbuddy-gateway .
```

## 免责声明

本项目仅用于个人学习与技术研究。腾讯 CodeBuddy（含国内站与国际站 workbuddy.ai）的接口协议与风控策略可能随时变化；请遵守腾讯服务条款，自行承担使用风险。本仓库不包含任何官方未公开的密钥或凭据。
