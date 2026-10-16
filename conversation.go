package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	conversationIDHeader        = "X-Conversation-ID"
	conversationRequestIDHeader = "X-Conversation-Request-ID"
	conversationMessageIDHeader = "X-Conversation-Message-ID"
)

type conversationContextKey struct{}

type conversationHeaders struct {
	ID        string
	RequestID string
	MessageID string
	Source    string
	links     linkIDs
}

// Text session keys are caller-isolated; neither keys nor credentials are logged.
var conversationNamespace = uuid.MustParse("7518352d-f297-5ede-9583-a32f3413b74d")

func usableConversationSeed(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func normalizedConversationID(value, kind, caller string) string {
	if id, err := uuid.Parse(value); err == nil && id != uuid.Nil {
		if len(value) == 32 {
			return strings.ToLower(value)
		}
		return id.String()
	}
	return uuid.NewSHA1(conversationNamespace, []byte(caller+"\x00"+kind+"\x00"+value)).String()
}

func conversationHeadersFor(r *http.Request) *conversationHeaders {
	if r == nil {
		return nil
	}
	value, _ := r.Context().Value(conversationContextKey{}).(*conversationHeaders)
	return value
}

// Read original protocol metadata before translation, without re-encoding the body.
// Preserve the fork's linked trace/message invariants and freeze the final IDs for
// this logical call, including safe network retries and account/site fallback.
func prepareConversationContext(r *http.Request, body any) *conversationHeaders {
	if existing := conversationHeadersFor(r); existing != nil {
		return existing
	}
	in := make(http.Header)
	if r != nil {
		in = r.Header.Clone()
	}
	caller := apiKeyFingerprint(in.Get("Authorization"))
	if caller == "" {
		caller = apiKeyFingerprint(in.Get("x-api-key"))
	}
	metadata := messageField(body, "metadata")
	bodyValue := func(object any, names ...string) string {
		for _, name := range names {
			if value, ok := messageField(object, name).(string); ok {
				if seed := usableConversationSeed(value); seed != "" {
					return seed
				}
			}
		}
		return ""
	}
	source := "独立生成"
	seed := getHeaderExact(in, conversationIDHeader)
	if seed != "" {
		source = "客户端会话头"
	} else {
		if seed = usableConversationSeed(in.Get("X-Session-ID")); seed != "" {
			source = "客户端Session头"
		} else if seed = bodyValue(body, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
			source = "请求会话字段"
		} else if seed = bodyValue(metadata, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
			source = "请求metadata会话字段"
		}
		if seed != "" {
			seed = normalizedConversationID(seed, "conversation", caller)
			setHeaderExact(in, conversationIDHeader, seed)
		}
	}
	for _, field := range []struct {
		header string
		names  []string
		kind   string
	}{
		{conversationRequestIDHeader, []string{"conversation_request_id", "conversationRequestId"}, "request"},
		{conversationMessageIDHeader, []string{"conversation_message_id", "conversationMessageId"}, "message"},
	} {
		if getHeaderExact(in, field.header) != "" {
			continue
		}
		value := bodyValue(body, field.names...)
		if value == "" {
			value = bodyValue(metadata, field.names...)
		}
		if value != "" {
			setHeaderExact(in, field.header, normalizedConversationID(value, field.kind, caller+"\x00"+seed))
		}
	}
	ids := resolveLinkIDs(in, newSessionScope(clientConversationIDs(in)))
	h := &conversationHeaders{ID: ids.conversationID, RequestID: ids.traceID,
		MessageID: ids.requestID, Source: source, links: ids}
	if r == nil {
		return h
	}
	*r = *r.WithContext(context.WithValue(r.Context(), conversationContextKey{}, h))
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[会话头准备] traceId=%s 来源=%s 会话ID已就绪=true 轮次ID已就绪=true 消息ID已就绪=true 说明=同一逻辑请求的重试和换号沿用同一组标识；不记录会话键、提示词或令牌", trace, h.Source)
	return h
}
