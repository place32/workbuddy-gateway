# v1.14.24（place32 fork）

合并上游 `CangShui/workbuddy-gateway` 至提交 `09ed560`（v1.14.24），包含 v1.14.23 与 v1.14.24 更新；保留本 fork 已有协议和会话约束。

## 上游更新

- 三协议入口共用出站准备链；可通过 `upstream.requestProfile: "official"` 启用连续前导纯文本 system 合并、已验证模型默认字段及国内 5.8.3 桌面档位。旧配置省略/null/空串仍为 `legacy`，示例配置显式采用 `official`；修改配置约 1 秒热生效，已在执行的模型请求保持原快照。
- 补齐 Responses `parallel_tool_calls`、`text.verbosity`，Messages `output_config.effort`、`disable_parallel_tool_use` 转换；保留显式 false/0、输出预算及关闭思考语义。
- Chat 流式等待首个 data 时默认每 15 秒发送 SSE 注释心跳；收到有效数据前遇瞬时网络错误或空闲超时，最多重放一次。已发送数据、客户端取消或永久扫描错误不重放；重放失败用 SSE 错误收尾，不伪造 `[DONE]`。
- DSH 身份前缀兼容扩展至 system/assistant 的标准文本块及配置注入内容；精确净化已知 GUI 环境句，保留用户/工具内容、代码与其他正文。
- 补充出站兼容、DSH 历史排障及贡献者说明；完整请求诊断增加压缩前正文和线上字节元数据。

## Fork 保留与冲突处理

- 保留 ordered JSON、原有思考等级归一化；客户端未声明时不自动注入 `reasoning_effort` / `reasoning_summary`。
- 保留国内/国际站客户端指纹、头部线上顺序、模型请求 gzip 和响应解压；统一编码只压缩一次，不因共享兼容层使 legacy 或国际站退回未压缩正文。
- 保留会话作用域与请求链路 ID 不变量、控制面登录三请求、反审查净化、账号串行调度及近 5 小时网页统计。
- 上游新增兼容函数适配 fork 的保序对象，不通过 map 重编码破坏字段顺序。

## 升级与发布

升级前备份配置与凭据。首字节前重放可能额外消耗一次上游额度；设置 `upstream.enableFirstByteRetry: false` 可关闭，详情见[手册](docs/user-guide.md)。完整请求诊断仍需显式开启，日志即使凭据脱敏仍包含敏感业务正文。

推送本次 `v1.14.24` 标签后，由现有 Release 工作流执行静态检查与测试、9 个平台/架构构建，发布二进制、`config.example.json`、`build-manifest.json`、`SHA256SUMS` 和 `SHA256SUMS.txt`。
