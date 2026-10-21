# v1.14.21（place32 fork）

集成上游 `CangShui/workbuddy-gateway` 至提交 `ceb2ac8a35dc3a6645ef1bf07930ffd28d595ae4`，并保留本 fork 的协议与会话行为。

## 上游更新

- 三种协议入口仅对 system 消息开头的 `You are an AI agent powered by DeepSeek Harness` 做等义前缀改写；其余文本和前导空白保持不变。
- 新增 opt-in 完整请求诊断：配置 `debug.enabled` 与 `debug.fullRequests` 后记录模型 API 入站请求、实际出站请求及上游错误；已知凭据字段会脱敏。完整对话、代码、工具输入和图片仍是敏感数据，默认关闭，排障后应关闭并保护日志文件。
- 网页模型列表使用 `stats.json` 汇总近 5 小时的请求数、首 Token、总耗时和 Token 数据。
- 网页扫码登录显示倒计时，支持复制链接并明确处理超时；结束会话短暂保留以便浏览器读取终态。删除失效账号时同步清除标记并从账号池/状态快照移除。
- 更新用户手册及 MoRanYue 贡献署名。

## Fork 保留项

- 国内站与国际站客户端指纹、线上请求头顺序、请求体压缩与响应解压。
- 思考参数归一化、保序 JSON、会话链路不变量、SSE 心跳、反审查净化和单账号串行化。
- 跨协议请求共用的会话规则，以及 fork 的认证与错误处理约束。

## 发布

推送 `v1.14.21` 标签后由 `.github/workflows/release.yml` 执行测试、9 个平台/架构构建并上传二进制、配置示例、构建清单与 SHA256 校验文件。升级前请备份配置与凭据；完整请求日志默认关闭，相关配置和隐私限制见[使用手册](docs/user-guide.md#json-调试日志)。
