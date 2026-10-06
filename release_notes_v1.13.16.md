## 合并上游 v1.13.15

本版本合并 `CangShui/workbuddy-gateway` 的 v1.13.15（v1.13.7–v1.13.15 累计），并保留 fork 全部私有能力（OpenAI 兼容思考等级归一化、客户端指纹与字节级对齐、保序 JSON、会话作用域链路 ID、SSE 心跳保真、反审查净化、单账号串行化）。

### Anthropic 协议原生入口（v1.13.7–v1.13.8）

`/v1/messages`（SSE 流式 + 非流式聚合）与 `/v1/messages/count_tokens`，Claude Code 等 Anthropic 客户端免外部翻译层直连，鉴权兼容 `x-api-key` 头。Anthropic named tool choice 适配上游 string-only schema。

### DeepSeek 推理回放与多轮推理历史回填（v1.13.7）

Responses 请求中 `type: "reasoning"` 项的 `content` / `summary` 在转换时挂到对应 assistant 消息的 `reasoning_content`；出站前对 DeepSeek 模型自动核对助手历史，保留已有推理、从 `reasoning` 字段复制、为空推理补空白占位（防止 11155）。

### 多模态 Responses 工具结果（v1.13.15）

`function_call_output` 中的 `input_image` / `image_url` 块不再作为 Base64 文本回放，提升为独立 user 多模态消息，与 Anthropic tool_result 的图片处理一致。并行批次的图片消息移至全部 tool 结果之后，避免打断调用/结果配对（11148）。

### 凭据生命周期管理（v1.13.9–v1.13.12）

- 刷新后覆盖 / 删除凭据前先做只读校验，校验失败不误删可用凭据。
- 主动定时 keepalive：按 `-keepalive-hours` 在固定时刻刷新全部账号，并跟踪 refresh-token 过期时间提前预警。
- 账号套餐从当前 entitlements 推导，保留未知 tier；区分国内四档标签与国际站标签；兼容生产 API 的数值型 billing account ID。

### 网关加固与运维（v1.13.13–v1.13.15）

- 出站上游调用统一走 `doUpstreamRequest`，网络瞬断且请求头未写出时自动换连接重试（默认 2 次，`upstream.transientRetries` 可配）；请求头已写出时不重放，避免重复执行模型请求。
- 禁用状态的凭据占位不再参与免费站点选择。
- 可按 `priceProbe.enabled: false` 关闭自动模型价格探测，适配受控部署。
- 免费模型展示数量改从模型统计表推导，不再单独维护。

### 合并冲突处理（fork 适配）

- 上游全面改用 `map[string]any`，fork 保留 `*jsonObject` 保序 JSON：为 `prepareSystemPromptForUpstream`、`repairReasoningHistory`、`applyThinkingRules`、`sanitizeMessages`、`convertResponsesToolOutput`、`logResponsesToolOutputConversion` 添加 `any` 双类型入口 + `toMap` / `messageToMap` / `messageFieldHas` / `ensureLeadingSystemMessageOrdered` / `thinkingExplicitlyEnabledOrdered` / `mergeResponsesAssistant` / `reasoningReplayTextOrdered` 适配器，保序语义不被破坏。
- `doUpstreamRequest` 扩展 `sessionScope` 参数，保留 fork 的会话作用域链路 ID（`backendHeaders` 5 参签名不变）；移除上游在调试分支对 `X-Trace-ID` / `X-Parent-Request-ID` 出站头的覆盖，链路头继续只由 `setTraceHeaders` 写入。
- `profileCN.Origin` / `profileINTL.Origin` 统一改为 fork 的 `PortalOrigin`（`plans.go` 与全部测试）。
- `keepalive` 测试的 `X-Auth-Refresh-Source` 期望与上游实现对齐为 `plugin`。
- 流式测试 chunk 补 `finish_reason`，匹配上游「无 finish_reason 拒绝当成成功」的新行为。
- 推理合并后的 assistant 消息形态按上游测试口径更新（同一轮回合的推理、正文与并行工具调用合并为一条 assistant）。

### 版本

取上游 `1.13.15` 后 bump 至 `1.13.16`（fork 已领先上游）。

## 验证

- `go build ./...` 通过。
- `go test ./...` 全绿。

> 说明：本机无可用上游登录凭据，无法对真实上游做端到端对话验证；上述验证均以本地假上游 + 单元测试完成。
