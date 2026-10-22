package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

const (
	requestProfileLegacy   = "legacy"
	requestProfileOfficial = "official"
	// 固定为 2026-10-21 国内桌面实抓版本，不声称跟随官网最新版本。
	observedDesktopUA = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
)

var officialRequests atomic.Bool

type requestProfileContextKey struct{}

func normalizeRequestProfile(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", requestProfileLegacy:
		return requestProfileLegacy, nil
	case requestProfileOfficial:
		return requestProfileOfficial, nil
	default:
		return "", fmt.Errorf("upstream.requestProfile 只支持 legacy 或 official")
	}
}

func setRequestProfile(value string) {
	profile, _ := normalizeRequestProfile(value) // 整份配置已在应用前校验。
	officialRequests.Store(profile == requestProfileOfficial)
}

// 一次逻辑请求只取一次快照，避免热加载使正文、请求头和重试使用不同配置。
func requestProfileFor(r *http.Request) string {
	if r != nil {
		if value, ok := r.Context().Value(requestProfileContextKey{}).(string); ok {
			return value
		}
	}
	profile := requestProfileLegacy
	if officialRequests.Load() {
		profile = requestProfileOfficial
	}
	if r != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), requestProfileContextKey{}, profile))
	}
	return profile
}

// 三个入口在协议转译后共用此处。不要提升 user 角色，不替换客户端指令或工具。
func prepareUpstreamChatRequest(obj any, r *http.Request, requestID uint64, model string) {
	profile := requestProfileFor(r)
	trace := debugTraceID(r)
	applyThinkingRules(obj, model)
	if modified := sanitizeMessages(obj); modified > 0 {
		logSystemPrefixSanitized(r, requestID, modified)
	}
	// 三个协议都可能在这里合并文本块。先在合并前处理每个块的开头，
	// 再在 fallback/force 注入后补一次，避免后续块的前缀因拼接而丢失。
	logDSHPromptCompatibility(r, requestID, obj)
	if profile == requestProfileOfficial {
		mergeLeadingInstructionMessages(obj, r, requestID)
	}
	prepareSystemPromptForUpstream(obj, r, requestID, trace)
	logDSHPromptCompatibility(r, requestID, obj)
	ensureUpstreamMaxTokens(obj, r, requestID, trace, model)
	if profile == requestProfileOfficial {
		applyOfficialBodyDefaults(obj, r, requestID, model)
	}
	report := repairToolMessageSequence(obj)
	logToolSequenceRepair(r, trace, requestID, model, report)
	logReasoningHistoryRepair(r, requestID, model, repairReasoningHistory(obj))
}

func applyOfficialBodyDefaults(obj any, r *http.Request, id uint64, model string) {
	var added []string
	put := func(key string, value any) {
		if messageField(obj, key) == nil {
			setMessageField(obj, key, value)
			added = append(added, key)
		}
	}
	// Chat 新旧输出预算字段等价；只有没有 max_tokens 时才转换，冲突值不擅自覆盖。
	if value := messageField(obj, "max_completion_tokens"); value != nil && messageField(obj, "max_tokens") == nil {
		setMessageField(obj, "max_tokens", value)
		deleteMessageField(obj, "max_completion_tokens")
		added = append(added, "max_completion_tokens->max_tokens")
	}
	// 上游总是 SSE，独立的 usage 事件有助于三个下游协议得到相同计费数据。
	if value := messageField(obj, "stream_options"); value == nil {
		setMessageField(obj, "stream_options", newObject("include_usage", true))
		added = append(added, "stream_options.include_usage")
	} else if isMessage(value) {
		if messageField(value, "include_usage") == nil {
			setMessageField(value, "include_usage", true)
			added = append(added, "stream_options.include_usage")
		}
	}
	// 只有该模型有完整桌面样本。其他模型不猜测其 temperature/verbosity 支持范围。
	if normalizeModelName(model) == "deepseek-v4.1-flash" {
		if !clientDeclaredOutputBudget(obj) {
			put("max_tokens", 128000) // 目录尚未就绪时，使用已验证的样本输出上限。
		}
		if messageField(obj, "top_p") == nil {
			put("temperature", 1)
		}
		put("verbosity", "high")
	}
	sort.Strings(added)
	log.Printf("[出站兼容] traceId=%s requestId=%d profile=official 阶段=正文准备 模型=%s 补入字段=%v 结果=完成 说明=保留显式档位、用户角色、原文与工具；不复制官方私有提示词",
		debugTraceID(r), id, model, added)
	debugEvent(r, "debug", "official_request_body_prepared", map[string]any{
		"profile": requestProfileOfficial, "defaulted_fields": added,
		"reasoning_effort": messageField(obj, "reasoning_effort"),
		"business_impact":  "统一三个入口的已验证默认字段；不将high强制升级max，不提升user指令层级",
	})
}

// 仅合并连续前导、无附加元数据的纯文本 system。user、图片、cache_control、
// 中途插入的 system 及其他字段均不猜测或丢弃；强制提示词在合并后才追加。
func mergeLeadingInstructionMessages(obj any, r *http.Request, id uint64) {
	messages := messagesOf(obj)
	var texts []string
	count := 0
	for _, raw := range messages {
		if roleOfMessage(raw) != "system" || objectFieldCount(raw) != 2 {
			break
		}
		var parts []string
		switch content := messageField(raw, "content").(type) {
		case string:
			parts = []string{content}
		case []any:
			for _, raw := range content {
				if objectFieldCount(raw) != 2 || messageField(raw, "type") != "text" {
					parts = nil
					break
				}
				text, ok := messageField(raw, "text").(string)
				if !ok {
					parts = nil
					break
				}
				parts = append(parts, text)
			}
		}
		if parts == nil {
			break
		}
		texts = append(texts, parts...)
		count++
	}
	if count == 0 {
		return
	}
	first := messages[0]
	_, wasString := messageField(first, "content").(string)
	if count == 1 && wasString {
		return
	}
	setMessageField(first, "content", strings.Join(texts, "\n\n"))
	out := make([]any, 0, len(messages)-count+1)
	out = append(out, first)
	out = append(out, messages[count:]...)
	setMessageField(obj, "messages", out)
	log.Printf("[出站兼容] traceId=%s requestId=%d 阶段=指令结构 结果=合并连续前导纯文本system 合并消息数=%d 说明=保留文本顺序，不跨user边界，不记录正文", debugTraceID(r), id, count)
	debugEvent(r, "debug", "official_instruction_messages_merged", map[string]any{
		"messages_merged": count, "business_impact": "仅把已明确标注为system的连续纯文本合并；不提升user角色",
	})
}

// Inspect metadata without flattening ordered objects or allocating key copies.
func objectFieldCount(value any) int {
	switch obj := value.(type) {
	case *jsonObject:
		if obj != nil {
			return obj.Len()
		}
	case map[string]any:
		return len(obj)
	}
	return 0
}

func usesObservedDesktopHeaders(profile string, prof *upstreamProfile) bool {
	return profile == requestProfileOfficial && prof.Key == "cn"
}

// 只用于模型请求。登录/刷新/额度接口继续沿用原有鉴权逻辑。
// 不伪造 X-Device-Token、SDK/Node 运行时信息或其他无法取得的客户端凭证。
func applyObservedDesktopHeaders(req *http.Request, prof *upstreamProfile, profile string) {
	if !usesObservedDesktopHeaders(profile, prof) || req.Method != http.MethodPost || req.URL.Path != "/v2/chat/completions" {
		return
	}
	for _, name := range []string{
		"Origin", "Referer", "X-Client-ID", "X-Client-Version", "X-Refresh-Token",
		"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
	} {
		for key := range req.Header {
			if strings.EqualFold(key, name) {
				delete(req.Header, key)
			}
		}
	}
	setHeaderExact(req.Header, "Accept", "application/json")
	setHeaderExact(req.Header, "User-Agent", observedDesktopUA)
	setHeaderExact(req.Header, "X-IDE-Type", "WorkBuddy")
	setHeaderExact(req.Header, "X-IDE-Name", "WorkBuddy")
	setHeaderExact(req.Header, "X-IDE-Version", "5.8.3")
	setHeaderExact(req.Header, "X-Private-Data", "true")
	setHeaderExact(req.Header, "x-codebuddy-request", "1")
	// Keep the fork's request/session/trace ID relationships and agent metadata.
	// No observed sample justifies rewriting caller-supplied identities.
}

func encodeModelRequest(raw []byte, profile string, prof *upstreamProfile) ([]byte, error) {
	if len(raw) == 0 || !prof.GzipBody {
		return raw, nil
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
