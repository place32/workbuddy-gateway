# WorkBuddy Local Gateway

纯 Go、零 CGO 依赖的跨平台单二进制网关，支持国内站与国际站、多账号调度、OpenAI / Anthropic 协议及可选网页控制台。

**v1.14.20（place32 fork）**：集成上游 `98056c5`，保留本 fork 的思考参数归一化、客户端指纹与保序 JSON、会话链路不变量、SSE 心跳、反审查净化与单账号串行化。新增用量统计、账号告警、网页登录等使用说明及合并后的会话规则见[完整手册](docs/user-guide.md)。

## 快速开始

需要 `go.mod` 声明的 Go 1.26.5 或更新工具链。在项目根目录执行：

```bash
go build -o workbuddy-gateway .
./workbuddy-gateway login
./workbuddy-gateway serve -webui
```

Windows 可使用 `.\workbuddy-gateway.exe`。管理端口默认 `8316`，模型 API 默认 `8317`。

- 首次使用可复制 `config.example.json` 为 `config.json`，并按需要设置管理 Key 和 API 鉴权。
- **后台价格探测默认关闭**，只有显式传入 `-allow-price-probes` 才开启；主动生成请求可能消耗积分。
- 网页配置页支持风险确认后的完整 JSON 编辑；监听端口修改需重启服务。
- `config.json` 和账号凭据含敏感信息，已由 Git 忽略，不要公开分享。

## 文档

| 内容 | 位置 |
|---|---|
| 完整使用手册、配置、客户端接入与部署 | [docs/user-guide.md](docs/user-guide.md) |
| 目录职责与命名约定 | [docs/project-layout.md](docs/project-layout.md) |
| 贡献者及方案署名 | [docs/contributors.md](docs/contributors.md) |
| 测试入口与说明 | [tests/README.md](tests/README.md) |

## 构建与测试

```bash
go vet ./...
go test ./...
```

PowerShell：

```powershell
.\scripts\test.ps1                 # Go、网页和构建脚本回归
.\build.ps1                       # 校验并构建全部 9 个平台/架构目标
.\build.ps1 -Only linux -Architecture amd64
```

Go 单元测试保持与 `package main` 源码同目录，以保留私有函数测试和标准 `go test ./...` 行为；它们不进入生产构建。独立网页与构建脚本测试集中在 `tests/`。完整文档集中在 `docs/`，根目录保留构建入口与配置示例。

- **Windows ARM32 不受当前 Go 工具链支持**，不能用 ARM64 文件替代。Linux ARM32 可用 `-ArmVersion 5/6/7` 选择，文件名与最低 ARM 版本一致。
- 默认先运行 `go vet`、`go test`；不受调用者遗留的 `GOOS` / `GOARCH` 影响。各目标使用 `CGO_ENABLED=0`，x64 基线为 `GOAMD64=v1`、ARM64 基线为 `GOARM64=v8.0`。成功或失败后恢复原构建环境和工作目录。
- 默认输出到 `dist/`，同时生成 `SHA256SUMS`、`build-manifest.json` 与空密钥的 `config.example.json`；校验和覆盖二进制、清单与示例配置。
- `-OutputDir` 必须是项目内的独立子目录；`-Clean` 仅清理已知构建文件，不递归删除目录、配置凭据或其他文件。构建日志写入 `logs/build-YYYY-MM-DD.jsonl`。

### 自动发布（GitHub Actions）

`.github/workflows/release.yml` 在**推送 `v*` 标签**时自动构建并发布，无需本地跑构建：

```bash
# 版本号先与 main.go 的 version 常量一致，再打标签推送
git tag -a v1.14.20 -m "v1.14.20: merge upstream 98056c5"
git push origin main
git push origin refs/tags/v1.14.20:refs/tags/v1.14.20
```

只推送本次新建的明确标签；不要使用 `--follow-tags` 或 `--tags`，以免将本地已有的上游标签发布到 fork。

流程（`windows-latest`，复用 `build.ps1`，保证与本地发布口径一致）：

1. 校验标签与 `main.go` 的 `version` 常量一致，不一致直接失败（避免发出与标签不符的包）；
2. `go vet` + `go test` + 9 目标交叉编译 → `dist/`；
3. 按 fork 既有格式补出只含 9 个二进制的 `SHA256SUMS.txt`；
4. 上传工作流产物，并创建/更新 Release：资产为 9 个二进制 + `SHA256SUMS` + `SHA256SUMS.txt` + `build-manifest.json` + `config.example.json`，说明取自 `release_notes_<tag>.md`（缺失时用 GitHub 自动生成）。Release 已存在时覆盖上传资产，便于重跑失败任务。

也可在 Actions 页面用 `workflow_dispatch` 对**已推送的旧标签**补发包（输入标签名，如 `v1.14.3`）。

---

## 免责声明

本项目仅用于个人学习与技术研究。腾讯 CodeBuddy（含国内站与国际站 workbuddy.ai）的接口协议与风控策略可能随时变化；请遵守腾讯服务条款，自行承担使用风险。本仓库不包含任何官方未公开的密钥或凭据。
