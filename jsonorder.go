package main

// 本文件实现「保序 JSON 对象」，用于网关重编码上游请求体时复刻客户端 JSON.stringify 的输出口径。
//
// 背景：客户端（Node/Electron）以 JSON.stringify 序列化请求体，该函数有两个可静态识别的特征：
//  1. 键顺序 = 属性插入顺序（实测 body 以 "model" 开头，而非字典序）；
//  2. 仅转义 " \ 与控制字符，< > & 原样输出（正文中的 <user_query> / <content_policy> 标签保持可读）。
//
// 网关用 encoding/json 反序列化为 map[string]any 后重新编码，会同时破坏这两点：
//   - map 编码按键名字典序输出 ⇒ messages 排在 model 之前；
//   - 默认 HTML 安全转义 ⇒ < 变成 \u003c。
//
// 两者都是无需解析语义、仅比对字节即可判定的机器特征，故此处用保序对象 + 自实现转义替代。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

const hexDigits = "0123456789abcdef"

// jsonObject 是保序的 JSON 对象。键顺序即插入顺序，重复键保留首次出现的位置（值取最后一次）。
type jsonObject struct {
	keys []string
	vals map[string]any
}

func newJSONObject() *jsonObject {
	return &jsonObject{vals: map[string]any{}}
}

// newObject 按 k1,v1,k2,v2,... 的顺序构造对象，便于在调用点显式表达线上键序。
// 参数个数为奇数或键不是字符串时 panic——属编程错误，仅在构造期暴露。
func newObject(kv ...any) *jsonObject {
	if len(kv)%2 != 0 {
		panic("newObject: 参数必须成对出现")
	}
	o := newJSONObject()
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			panic("newObject: 键必须是字符串")
		}
		o.Set(key, kv[i+1])
	}
	return o
}

// Get 读取键值。不存在时返回 nil, false。
func (o *jsonObject) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Set 写入键值：已存在的键保留原位置，新键追加到末尾。
func (o *jsonObject) Set(key string, v any) {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Delete 移除键；不存在时为空操作。
func (o *jsonObject) Delete(key string) {
	if _, exists := o.vals[key]; !exists {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys 返回键名切片副本（调用方修改不影响内部状态）。
func (o *jsonObject) Keys() []string {
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Len 返回键数量。
func (o *jsonObject) Len() int { return len(o.keys) }

// appendTo 按键序把对象写入 buf。
func (o *jsonObject) appendTo(buf *bytes.Buffer) error {
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		appendJSONString(buf, k)
		buf.WriteByte(':')
		if err := appendJSONValue(buf, o.vals[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// MarshalJSON 使 jsonObject 可被标准库直接编码（键序仍由本类型保证）。
func (o *jsonObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := o.appendTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// appendJSONString 按 JavaScript JSON.stringify 的口径转义字符串。
//
// 与 Go encoding/json 默认行为的差异：后者对 < > & 做 HTML 安全转义（\u003c \u003e \u0026），
// 而 JSON.stringify 不转义。字节流按 UTF-8 逐字节处理，非 ASCII 字节（>= 0x80）原样输出。
func appendJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if start < i {
			buf.WriteString(s[start:i])
		}
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0x0F])
		}
		start = i + 1
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
}

// appendJSONValue 按 JSON.stringify 口径写入任意值。
func appendJSONValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		appendJSONString(buf, t)
	case json.Number:
		// 保留客户端原始数字字面量（1.0 不会退化成 1，1e5 不会展开）
		buf.WriteString(t.String())
	case *jsonObject:
		return t.appendTo(buf)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := appendJSONValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case float64:
		buf.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	default:
		// 兜底：其余类型（含 map[string]any、[]string 等网关内部构造值）走标准库，
		// 但必须关闭 HTML 转义以保持与客户端一致。
		var tmp bytes.Buffer
		enc := json.NewEncoder(&tmp)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	}
	return nil
}

// marshalJSON 以客户端口径序列化（不转义 HTML，无尾随换行）。
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := appendJSONValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeOrderedJSON 解析顶层为对象的 JSON，并保留键顺序与数字字面量。
func decodeOrderedJSON(data []byte) (*jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("顶层必须是 JSON 对象")
	}

	obj := newJSONObject()
	if err := obj.decodeMembers(dec); err != nil {
		return nil, err
	}
	// 拒绝对象之后的尾随内容（与 encoding/json 的严格解析保持一致）
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JSON 对象之后存在多余内容")
		}
		return nil, err
	}
	return obj, nil
}

// decodeMembers 读取对象成员直到 '}'，并消费该 '}'。
func (o *jsonObject) decodeMembers(dec *json.Decoder) error {
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return fmt.Errorf("对象键必须是字符串")
		}
		v, err := decodeOrderedValue(dec)
		if err != nil {
			return err
		}
		o.Set(key, v)
	}
	_, err := dec.Token() // 消费 '}'
	return err
}

// decodeOrderedValue 递归解析任意 JSON 值。
func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string / json.Number / bool / nil
	}
	switch d {
	case '{':
		o := newJSONObject()
		if err := o.decodeMembers(dec); err != nil {
			return nil, err
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil { // 消费 ']'
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("意外的 JSON 分隔符 %v", d)
}
