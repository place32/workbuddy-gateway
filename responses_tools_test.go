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

func TestResponsesNamespaceCustomAndSearchToolsReachUpstream(t *testing.T) {
	body := decodeCompatOrdered(t, []byte(`{
		"input":"hi",
		"tools":[
			{"type":"function","name":"plain","description":"plain tool","parameters":{"type":"object","properties":{}}},
			{"type":"namespace","name":"mcp__demo","description":"demo namespace","tools":[
				{"type":"function","name":"alpha","description":"alpha tool","parameters":{"type":"object","properties":{"x":{"type":"string"}}}},
				{"type":"function","name":"beta","description":"beta tool","parameters":{"type":"object"}}
			]},
			{"type":"custom","name":"apply_patch","description":"patch tool","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
			{"type":"web_search","external_web_access":true}
		]
	}`))
	ordered, tools, err := responsesToChatRequestDetailed(body, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	chat := ordered.toMap()
	got, _ := chat["tools"].([]any)
	if len(got) != 5 {
		t.Fatalf("tool count=%d, want 5: %#v", len(got), got)
	}
	names := []string{}
	for _, raw := range got {
		item := raw.(map[string]any)
		if item["type"] != "function" {
			t.Fatalf("upstream tool is not a function: %#v", item)
		}
		names = append(names, item["function"].(map[string]any)["name"].(string))
	}
	want := []string{"plain", "mcp__demo__alpha", "mcp__demo__beta", "apply_patch", "web_search"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names=%v want=%v", names, want)
	}
	if tools["mcp__demo__alpha"].Namespace != "mcp__demo" || tools["mcp__demo__alpha"].Name != "alpha" {
		t.Fatalf("namespace mapping lost: %#v", tools["mcp__demo__alpha"])
	}
	if tools["apply_patch"].Kind != responsesToolCustom || tools["web_search"].Kind != responsesToolWebSearch {
		t.Fatalf("custom/search kinds lost: %#v", tools)
	}
	custom := got[3].(map[string]any)["function"].(map[string]any)
	params, _ := custom["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"].(map[string]any)["input"] == nil {
		t.Fatalf("custom input schema missing: %#v", params)
	}
}

func TestResponsesNamespaceAndCustomHistoryReachUpstream(t *testing.T) {
	ordered, _, err := responsesToChatRequestDetailed(decodeCompatOrdered(t, []byte(`{
		"input":[
			{"role":"user","content":"go"},
			{"type":"function_call","call_id":"c1","name":"alpha","namespace":"mcp__demo","arguments":"{\"x\":\"1\"}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"c2","output":"done"}
		]
	}`)), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	chat := ordered.toMap()
	messages, _ := chat["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("messages=%d, want 5: %#v", len(messages), messages)
	}
	first := messages[1].(map[string]any)
	calls := first["tool_calls"].([]any)
	if got := calls[0].(map[string]any)["function"].(map[string]any)["name"]; got != "mcp__demo__alpha" {
		t.Fatalf("namespace history name=%v", got)
	}
	custom := messages[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if custom["name"] != "apply_patch" || custom["arguments"] != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom history lost: %#v", custom)
	}
	if messages[2].(map[string]any)["tool_call_id"] != "c1" || messages[4].(map[string]any)["tool_call_id"] != "c2" {
		t.Fatal("custom/function output linkage lost")
	}
}

func TestResponsesAggregateRestoresNamespaceAndCustomItems(t *testing.T) {
	toolMap := map[string]responsesToolMeta{
		"mcp__demo__alpha": {UpstreamName: "mcp__demo__alpha", Name: "alpha", Namespace: "mcp__demo", Kind: responsesToolNamespace},
		"apply_patch":      {UpstreamName: "apply_patch", Name: "apply_patch", Kind: responsesToolCustom},
	}
	out, err := chatCompletionToResponsesWithTools([]byte(`{
		"id":"chatcmpl-1","created":1,
		"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"mcp__demo__alpha","arguments":"{\"x\":1}"}},
			{"id":"c2","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}
		]}}]
	}`), "m", toolMap)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	items := response["output"].([]any)
	first := items[0].(map[string]any)
	if first["type"] != "function_call" || first["name"] != "alpha" || first["namespace"] != "mcp__demo" {
		t.Fatalf("namespace item=%#v", first)
	}
	second := items[1].(map[string]any)
	if second["type"] != "custom_tool_call" || second["name"] != "apply_patch" || second["input"] != "patch" {
		t.Fatalf("custom item=%#v", second)
	}
}

func TestResponsesStreamRestoresNamespaceAndCustomEvents(t *testing.T) {
	isolatePolicies(t)
	resetStatsState(t)
	toolMap := map[string]responsesToolMeta{
		"mcp__demo__alpha": {UpstreamName: "mcp__demo__alpha", Name: "alpha", Namespace: "mcp__demo", Kind: responsesToolNamespace},
		"apply_patch":      {UpstreamName: "apply_patch", Name: "apply_patch", Kind: responsesToolCustom},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = withResponsesToolMap(req, toolMap)
	rec := httptest.NewRecorder()
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"mcp__demo__alpha","arguments":"{\"x\":1}"}},{"index":1,"id":"c2","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	streamResponsesResponse(rec, req, resp, "m", 1, &Account{Path: "test.json"}, &profileCN, time.Now())
	text := rec.Body.String()
	for _, want := range []string{`"type":"function_call"`, `"name":"alpha"`, `"namespace":"mcp__demo"`, `"type":"custom_tool_call"`, `"name":"apply_patch"`, `"input":"patch"`, "response.custom_tool_call_input.done"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %s\n%s", want, text)
		}
	}
}

func TestResponsesNamedToolChoiceUsesUpstreamString(t *testing.T) {
	ordered, _, err := responsesToChatRequestDetailed(decodeCompatOrdered(t, []byte(`{
		"input":"run",
		"tools":[
			{"type":"function","name":"plain","parameters":{"type":"object"}},
			{"type":"namespace","name":"mcp__demo","tools":[{"type":"function","name":"ping","parameters":{"type":"object"}}]}
		],
		"tool_choice":{"type":"function","name":"ping","namespace":"mcp__demo"}
	}`)), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	chat := ordered.toMap()
	if chat["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%#v, want required string", chat["tool_choice"])
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("selected tools=%d, want 1: %#v", len(tools), tools)
	}
	name := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	if name != "mcp__demo__ping" {
		t.Fatalf("selected tool=%v", name)
	}
}

func TestResponsesToolConversionPreservesOrderedSchema(t *testing.T) {
	request := decodeCompatOrdered(t, []byte(`{
		"input":"run",
		"tools":[{"type":"namespace","name":"demo","tools":[{
			"type":"function","name":"ordered","strict":false,
			"parameters":{"type":"object","properties":{"z":{"type":"number"},"a":{"type":"string","description":"<value>&"}},"required":["z","a"]}
		}]}]
	}`))
	chat, _, err := responsesToChatRequestDetailed(request, "m")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := marshalJSON(chat)
	if err != nil {
		t.Fatal(err)
	}
	want := `"function":{"name":"demo__ordered","parameters":{"type":"object","properties":{"z":{"type":"number"},"a":{"type":"string","description":"<value>&"}},"required":["z","a"]},"strict":false}`
	if !strings.HasPrefix(string(raw), `{"model":"m","messages":`) || !strings.Contains(string(raw), want) {
		t.Fatalf("ordered schema, explicit false or raw text changed: %s", raw)
	}
}

func TestResponsesCustomToolChoiceAndUndefinedSelection(t *testing.T) {
	for _, name := range []string{"apply_patch", "missing"} {
		t.Run(name, func(t *testing.T) {
			request := decodeCompatOrdered(t, []byte(`{
				"input":"run","tools":[{"type":"custom","name":"apply_patch"}],
				"tool_choice":{"type":"custom","name":"`+name+`"}
			}`))
			chat, tools, err := responsesToChatRequestDetailed(request, "m")
			if name == "missing" {
				if err == nil {
					t.Fatal("undefined selected tool must be rejected")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if messageField(chat, "tool_choice") != "required" || tools[name].Kind != responsesToolCustom {
				t.Fatalf("custom selection lost: %#v", chat)
			}
		})
	}
}
