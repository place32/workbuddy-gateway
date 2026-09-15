package main

import (
	"encoding/json"
	"testing"
)

// 保序层的契约：键序保留、HTML 敏感字符不转义、数字字面量不丢精度、非对象顶层被拒。
//
// 这些性质是「上游请求体与客户端 JSON.stringify 输出字节对齐」的基础；
// 端到端断言见 TestUpstreamWireBodyMatchesClientEncoding，此处覆盖该测试触达不到的边界。
func TestOrderedJSONContract(t *testing.T) {
	t.Run("键序保留且重复键取末值", func(t *testing.T) {
		o, err := decodeOrderedJSON([]byte(`{"model":"m","messages":[],"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := marshalJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"model":"m","messages":[],"stream":true}`; string(out) != want {
			t.Errorf("got %s want %s", out, want)
		}
	})

	t.Run("删除键后其余键序不变", func(t *testing.T) {
		o, err := decodeOrderedJSON([]byte(`{"a":1,"b":2,"c":3}`))
		if err != nil {
			t.Fatal(err)
		}
		o.Delete("b")
		o.Set("d", 4)
		out, _ := marshalJSON(o)
		if want := `{"a":1,"c":3,"d":4}`; string(out) != want {
			t.Errorf("got %s want %s", out, want)
		}
	})

	// 客户端正文含 <user_query> 等标签，JSON.stringify 不会转义；Go 默认会转成 \u003c。
	t.Run("HTML 敏感字符与 Unicode 不转义", func(t *testing.T) {
		o := newObject("k", "a<b>c&d\"e\\f\ng中文😀\u2028h")
		out, err := marshalJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"k":"a<b>c&d\"e\\f\ng中文😀` + "\u2028" + `h"}`; string(out) != want {
			t.Errorf("got  %s\nwant %s", out, want)
		}
		var back map[string]string
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("output is not valid JSON: %v", err)
		}
		if back["k"] != "a<b>c&d\"e\\f\ng中文😀\u2028h" {
			t.Errorf("round-trip changed value: %q", back["k"])
		}
	})

	// 若经 float64 中转，超出 2^53 的整数会被静默改写，构成对客户端的语义篡改。
	t.Run("数字字面量原样保留", func(t *testing.T) {
		const in = `{"big":12345678901234567890,"e":1e5,"neg":-0.5,"z":0}`
		o, err := decodeOrderedJSON([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		out, err := marshalJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Errorf("got %s want %s", out, in)
		}
	})

	// 顶层非对象必须被拒：处理链路按对象语义访问 model / messages。
	t.Run("拒绝非对象顶层与尾随内容", func(t *testing.T) {
		for _, in := range []string{`[]`, `"s"`, `123`, `null`, `{"a":1}{"b":2}`, `{"a":1}x`, ``, `{`, `{"a":}`} {
			if _, err := decodeOrderedJSON([]byte(in)); err == nil {
				t.Errorf("expected error for %q", in)
			}
		}
	})

	t.Run("嵌套结构与数组元素保序", func(t *testing.T) {
		in := `{"messages":[{"content":"x","role":"user"},{"content":"y","role":"assistant"}]}`
		o, err := decodeOrderedJSON([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := marshalJSON(o)
		if string(out) != in {
			t.Errorf("got %s want %s", out, in)
		}
	})
}

// Responses 链路转出的 chat 请求体必须以 model 开头（对齐客户端 JSON.stringify 的插入顺序）。
func TestResponsesBodyKeyOrder(t *testing.T) {
	chat, err := responsesToChatRequest(mustOrdered(t, `{"model":"m","instructions":"sys","input":"hi","temperature":0.5}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	out, err := marshalJSON(chat)
	if err != nil {
		t.Fatal(err)
	}
	keys := chat.Keys()
	if len(keys) == 0 || keys[0] != "model" {
		t.Errorf("first key = %v, want model", keys)
	}
	if len(keys) < 2 || keys[1] != "messages" {
		t.Errorf("second key = %v, want messages", keys)
	}
	if want := `{"model":"m","messages":[`; len(out) < len(want) || string(out[:len(want)]) != want {
		t.Errorf("body does not start with %s: %s", want, out)
	}
	t.Logf("responses body: %s", out)
}
