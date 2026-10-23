# v1.14.30（place32 fork）

合并上游 `CangShui/workbuddy-gateway` 至 `ec34778`，包含 v1.14.29 功能与 PR #26 的 Windows Winsock RST 兼容修复。使用独立 fork 版本号，不改写已有的上游 v1.14.29 标签。

## 上游更新

- Responses 支持 namespace 子工具、custom 工具及 web_search 的请求映射；流式与聚合响应恢复工具身份及 custom 输入。工具执行仍由客户端负责，不等价于 OpenAI 托管搜索。
- 命名 tool_choice 转为上游接受的 required 字符串，仅保留被选中的工具；保留显式 reasoning.summary=detailed。
- 国内模型请求改走 www.workbuddy.cn；登录、刷新与额度接口保留原站点。国际站仍走 www.workbuddy.ai，本地测试覆盖地址不受影响。
- 身份展示支持 identity.enabled、target 和 replacements；默认替换为 WorkBuddy，只处理 system/developer/assistant 文本，不改用户、工具结果、推理正文或工具参数。
- 新用户首次启动生成默认 config.json；旧配置缺少 identity 时自动补齐，已有 identity 和其他配置值保留。非法配置交由原校验流程拒绝。
- Windows forcibly closed / established connection was aborted 被识别为瞬时网络错误；重试仍受原有安全边界与预算限制。

## 合并处理与 Fork 保留

- 对 Responses 新转换器及工具 schema 适配保序 jsonObject，保留 JSON 键序、数值、显式 false 与原始文本，不整树转 map。
- 保留国内/国际站已验证的请求指纹、头部大小写与线上顺序、OTel/B3 传播和会话链路 ID；不覆盖既有 Agent 元数据，不重新生成请求身份。
- 两站 legacy 与 official 均保留站点 gzip 契约和响应解压；修正上游新增日志对 fork 压缩行为的不适用描述。
- 保留不自动注入思考参数、反审查净化、账号串行调度及既有 SSE 心跳/重放约束；GUI 环境句在品牌替换前净化。
- 补充配置 null 回归：升级初始化不再对空 map 写入而 panic，仍由配置校验明确拒绝非法文件。

## 升级提醒

升级前备份配置与凭据。旧配置在启动时可能被补充 identity 段并重新格式化；默认身份替换为文本级规则，正文里的品牌名称也会替换。需要回退可显式配置 identity.enabled=false。

本次发布提供 Windows 386/amd64/arm64、Linux 386/amd64/armv7/arm64、macOS amd64/arm64 共 9 个二进制，以及 config.example.json、build-manifest.json、SHA256SUMS 和 SHA256SUMS.txt。构建清单记录最终源提交；资产校验后再公开发布。
