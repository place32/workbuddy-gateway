package main

import (
	"log"
	"net/http"
	"strings"
)

// Only rewrite the blocked prompt prefix at byte offset zero. Leading whitespace,
// content later in the message, and all user/assistant messages remain untouched.
var blockedSystemPromptPrefixes = []string{
	"You are an AI agent powered by DeepSeek Harness",
}

const blockedSystemPromptPrefixReplacement = "You are an AI agent powered by a large language model"

// Match case-insensitively only at the beginning; preserve the suffix byte-for-byte.
func sanitizeSystemPromptPrefix(text string) (string, bool) {
	for _, prefix := range blockedSystemPromptPrefixes {
		if len(text) < len(prefix) || !strings.EqualFold(text[:len(prefix)], prefix) {
			continue
		}
		return blockedSystemPromptPrefixReplacement + text[len(prefix):], true
	}
	return text, false
}

// Process string content or only the first text block in a content-block array.
// Supporting ordered objects keeps the fork's JSON key-order invariant intact.
func sanitizeSystemMessageContent(content any) (any, bool) {
	switch value := content.(type) {
	case string:
		return sanitizeSystemPromptPrefix(value)
	case []any:
		for _, raw := range value {
			text, ok := messageField(raw, "text").(string)
			if !ok {
				continue
			}
			rewritten, changed := sanitizeSystemPromptPrefix(text)
			if changed {
				setMessageField(raw, "text", rewritten)
			}
			return value, changed
		}
	}
	return content, false
}

// Record only the number of rewrites and rule metadata; never include prompt text.
func logSystemPrefixSanitized(r *http.Request, requestID uint64, modified int) {
	if modified <= 0 {
		return
	}
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[提示词前缀净化] traceId=%s requestId=%d 规则=system开头DeepSeekHarness身份 修改消息数=%d 结果=已改写为中性前缀 说明=仅偏移0，正文其余部分逐字保留，不记录提示词正文",
		trace, requestID, modified)
	debugEvent(r, "info", "system_prompt_prefix_sanitized", map[string]any{
		"rule":               "deepseek_harness_system_prefix",
		"modified_messages":  modified,
		"result":             "rewritten",
		"business_impact":    "避免上游11128渠道校验；未改动非开头正文",
		"prompt_body_logged": false,
	})
}
