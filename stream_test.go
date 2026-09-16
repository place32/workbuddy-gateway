package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamChatResponseSSEFraming(t *testing.T) {
	chunk := `{"choices":[{"index":0,"delta":{"content":"你好"}}]}`
	for _, tc := range []struct {
		name   string
		prefix string
		data   string
	}{
		{"heartbeat", ": heartbeat\n\n", "data: " + chunk},
		{"metadata", "event: message\nid: 123\nretry: 1000\n\n", "data: " + chunk},
		{"invalid JSON", "data: not-json\n\n", "data: " + chunk},
		{"bare JSON compatibility", "", chunk},
		{"repeated data prefix compatibility", "", "data: data: " + chunk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := tc.prefix + tc.data + "\n\ndata: [DONE]\n\n"
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			rec := httptest.NewRecorder()
			streamChatResponse(rec, resp, 1, &Account{Path: "test"}, &upstreamProfile{Label: "test"}, time.Now())
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("Content-Type = %q", got)
			}
			var payloads []string
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "[DONE]" && !json.Valid([]byte(data)) {
					t.Fatalf("client cannot parse SSE data as JSON: %q", data)
				}
				payloads = append(payloads, data)
			}
			if len(payloads) != 2 || payloads[1] != "[DONE]" {
				t.Fatalf("expected one chunk and DONE, got %v", payloads)
			}
			var got, want any
			_ = json.Unmarshal([]byte(payloads[0]), &got)
			_ = json.Unmarshal([]byte(chunk), &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("chunk changed: got %s, want %s", gotJSON, wantJSON)
			}
			if tc.name == "heartbeat" && !strings.HasPrefix(rec.Body.String(), ": heartbeat\n\n") {
				t.Fatalf("heartbeat must remain an SSE comment: %q", rec.Body.String())
			}
		})
	}
}
