package main

// 上游请求头「顺序」对齐层。
//
// HTTP/1.1 的头部顺序不承载语义，但它是可静态识别的机器特征：两个客户端只要头部
// 字节不同即可被判为不同实现。
//   - 官方客户端（Node/axios + bundled OpenAI SDK）按**属性插入顺序**写出头部；
//   - Go 的 net/http 把 Host / User-Agent / Content-Length 单独前置，其余头部经
//     http.Header（map）按**规范化键名字典序**写出。
//
// 因此即便头部集合与键名大小写完全一致，网关上行的头部顺序仍与客户端截然不同 ——
// 这是「无需解析语义、比对两次请求字节即可判定」的特征。
//
// 复刻点选在 conn 层（见 orderedHeaderConn）：net/http 写头部的位置不可拦截，但其写出
// 的字节必经 net.Conn.Write。把 conn 包一层，在每个请求的头部块写出前按客户端实测顺序
// 重排：
//   - 直连 https：Go 未配置代理时使用 Transport.DialTLSContext，钩子拿到的正是明文
//     HTTP/1.1（TLS 已由本层完成）；
//   - 明文 http：走 Transport.DialContext（测试与本地回环监听即此路径）；
//   - 配置了 -proxy：Go 自行 CONNECT + TLS，不存在明文层，顺序复刻不生效（头部集合与
//     键名大小写仍保持对齐，见 README「客户端指纹对齐」）。
//
// 安全前提：只重排「能确认是 HTTP/1.1 请求头」的字节，任何无法确认的字节一律原样透传
// （见 orderedHeaderConn.Write 的状态机），因此对代理 CONNECT、TLS 密文、chunked 正文、
// 分段写出的头部既不改写也不滞留。

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// chatCompletionsPath 是模型请求路径（国内站与国际站共用同一协议路径）。
	chatCompletionsPath = "/v2/chat/completions"
	// maxOrderedHeadBytes 是头部块缓冲上限：超过即判定为非 HTTP 请求头，转入透传。
	maxOrderedHeadBytes = 64 << 10
)

// modelWireHeaderOrder 是官方客户端发往 /v2/chat/completions 的头部**线上顺序**
// （此处为小写匹配键，实际写出的键名大小写由 setHeaderExact 决定）。
//
// 基线取自两处实测，二者结构完全一致、互相印证：
//  1. 本机安装的桌面客户端（WorkBuddyAI 5.7.6 + bundled CLI 2.156.0）模型请求抓包；
//  2. 一份按线上字节顺序记录的 5.5.2 桌面客户端模型请求抓包（含凭据，本地研究产物、
//     不入仓库；该抓包同时给出 Content-Length / Host / Connection 三个传输层头的位置：
//     恒在最后三位）。
//
// 该顺序的成因（客户端源码逐条可定位，见 bundled CLI 的 dist/codebuddy-lite-wb.mjs）：
//   - 头部对象按属性插入顺序写出：SDK 默认头 → 应用自建头表（会话 / Agent / IDE /
//     隐私标记）→ 模型请求处理器链（priority 升序，GzipRequestProcessor priority=9999
//     恒为最后一个，追加 Content-Encoding）→ OTel + B3 传播头
//     （runWithGalileoModelRequestSpan 在处理器链之后、派发之前注入）；
//   - 随后 AuthenticationHttpInterceptor 依次追加 Authorization / X-User-Id /
//     X-Enterprise-Id / X-Department-Info / X-Tenant-Id / X-Domain；
//   - 其后 UA / 产品类拦截器追加 X-Product / User-Agent；
//   - 最后 Node http 模块补齐 Content-Length / Host / Connection。
//
// 因此 Content-Encoding 位于传播头**之前**、鉴权头之前；X-Domain 位于鉴权块末位
// （capture2 与 5.7.6 抓包在这一点上一致）。
var modelWireHeaderOrder = []string{
	// SDK 默认头
	"accept", "content-type", "x-requested-with",
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
	"x-stainless-package-version", "x-stainless-retry-count",
	"x-stainless-runtime", "x-stainless-runtime-version",
	// 应用自建头表
	"x-conversation-id", "x-conversation-request-id",
	"x-agent-intent", "x-agent-purpose",
	"x-ide-type", "x-ide-name", "x-ide-version",
	"x-private-data", "x-codebuddy-request",
	"x-request-id", "x-conversation-message-id", "x-root-request-id",
	"x-agent-type",
	// 处理器链末位：GzipRequestProcessor
	"content-encoding",
	// OTel + B3 传播头
	"traceparent", "b3", "x-b3-traceid", "x-b3-parentspanid", "x-b3-spanid",
	"x-b3-sampled", "x-trace-id",
	// AuthenticationHttpInterceptor（按源码插入顺序）
	"authorization", "x-user-id", "x-enterprise-id", "x-department-info",
	"x-tenant-id", "x-no-authorization", "x-no-user-id", "x-no-enterprise-id",
	"x-no-department-info", "x-domain",
	// 产品 / UA 拦截器
	"x-product", "user-agent",
	// 传输层（Node http 模块最后补齐）
	"content-length", "host", "connection",
}

// wireOrderForPath 返回该请求路径的客户端头部顺序；无实测基线的路径返回 nil。
//
// 仅模型请求有线上顺序基线：控制面（axios 族）与守护进程（undici）两族各自的头部对象
// 由不同调用点按不同源码顺序构造，尚无逐端点实测顺序，故不重排（其头部集合与键名大小写
// 已对齐，见 README）。
func wireOrderForPath(path string) []string {
	if path == chatCompletionsPath {
		return modelWireHeaderOrder
	}
	return nil
}

// connWriteState 是 orderedHeaderConn 的写状态。
type connWriteState uint8

const (
	// connStateHead：期待请求头（连接建立后，以及每个请求正文写满之后）。
	connStateHead connWriteState = iota
	// connStateBody：正文透传，按 Content-Length 剩余字节计数。
	connStateBody
	// connStateRaw：已判定字节流不是可确认的 HTTP/1.1 请求头，本连接永久透传。
	connStateRaw
)

var (
	crlf       = []byte("\r\n")
	headEndTag = []byte("\r\n\r\n")
)

// orderedHeaderConn 在 TLS 之上把 HTTP/1.1 请求头按官方客户端顺序重排后写出。
//
// 正文只按字节计数透传（绝不解析）：Go 的请求体可能**包含** CRLFCRLF 字节序列
// （如 JSON 正文里的换行），若在正文中二次搜索头部结束标记就会改写正文 —— 计数透传
// 从根本上避免该风险，并且让 keep-alive 复用同一连接的下一个请求也能被正确重排。
type orderedHeaderConn struct {
	net.Conn
	state    connWriteState
	buf      []byte
	bodyLeft int64
}

func newOrderedHeaderConn(c net.Conn) *orderedHeaderConn {
	return &orderedHeaderConn{Conn: c}
}

func (c *orderedHeaderConn) Write(p []byte) (int, error) {
	switch c.state {
	case connStateRaw:
		return c.Conn.Write(p)
	case connStateBody:
		n, err := c.Conn.Write(p)
		if n > 0 {
			c.bodyLeft -= int64(n)
			if c.bodyLeft <= 0 {
				c.bodyLeft = 0
				c.state = connStateHead
			}
		}
		return n, err
	}
	c.buf = append(c.buf, p...)
	if idx := bytes.Index(c.buf, headEndTag); idx >= 0 {
		return c.flushHead(idx+len(headEndTag), len(p))
	}
	// 尚未凑齐完整头部块。只有在字节流「看起来是 HTTP 请求头起始」时才继续缓冲：
	// 请求行会被任意切分，但方法 token 及其后的空格最多 8 字节，前缀不匹配即判定为
	// 非 HTTP 请求头（TLS 密文在此被立即识别），不会滞留在缓冲区里等一个永远不会
	// 出现的 CRLFCRLF。
	if !looksLikeHTTPRequestStart(c.buf) {
		return c.drainRaw(len(p))
	}
	if len(c.buf) > maxOrderedHeadBytes {
		return c.drainRaw(len(p))
	}
	return len(p), nil
}

// flushHead 在缓冲的第 headLen 字节处得到一个完整请求头块，重排后写出。
// wrote 是本次 Write 的入参长度，成功时原样返回（调用方视为全部被接受）。
func (c *orderedHeaderConn) flushHead(headLen, wrote int) (int, error) {
	head, rest := c.buf[:headLen], c.buf[headLen:]
	c.buf = nil

	out, bodyLen, ok := reorderRequestHead(head)
	if !ok {
		// 头部块无法解析：本连接永久透传，已缓冲字节原样写出。
		c.state = connStateRaw
		if err := writeAll(c.Conn, head); err != nil {
			return 0, err
		}
		if len(rest) > 0 {
			if err := writeAll(c.Conn, rest); err != nil {
				return 0, err
			}
		}
		return wrote, nil
	}
	if len(rest) > 0 {
		out = append(out, rest...)
	}
	if err := writeAll(c.Conn, out); err != nil {
		return 0, err
	}
	switch {
	case bodyLen < 0:
		// chunked 正文长度不可预知：本连接后续一律透传。
		c.state = connStateRaw
	case bodyLen > int64(len(rest)):
		c.state, c.bodyLeft = connStateBody, bodyLen-int64(len(rest))
	default:
		// 头部与完整正文（或空正文）在同一次写中到达。
		c.state = connStateHead
	}
	return wrote, nil
}

// drainRaw 转入永久透传，并原样写出已缓冲的字节。
func (c *orderedHeaderConn) drainRaw(wrote int) (int, error) {
	buf := c.buf
	c.buf = nil
	c.state = connStateRaw
	if len(buf) > 0 {
		if err := writeAll(c.Conn, buf); err != nil {
			return 0, err
		}
	}
	return wrote, nil
}

// writeAll 写完全部字节；任一步失败即返回错误（此时该连接已不可用）。
func writeAll(c net.Conn, b []byte) error {
	for len(b) > 0 {
		n, err := c.Write(b)
		b = b[n:]
		if err != nil {
			return err
		}
	}
	return nil
}

// httpMethodPrefixes 是判定期望的请求行前缀（含方法后的空格）。
var httpMethodPrefixes = [][]byte{
	[]byte("GET "), []byte("POST "), []byte("PUT "), []byte("PATCH "),
	[]byte("DELETE "), []byte("HEAD "), []byte("OPTIONS "), []byte("CONNECT "),
}

// looksLikeHTTPRequestStart 判断缓冲内容是否可能是 HTTP 请求头的开头。
// 内容不足一个完整方法 token 时按前缀匹配（请求行会被任意切分）。
func looksLikeHTTPRequestStart(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	for _, m := range httpMethodPrefixes {
		if len(b) <= len(m) {
			if bytes.HasPrefix(m, b) {
				return true
			}
			continue
		}
		if bytes.HasPrefix(b, m) {
			return true
		}
	}
	return false
}

// reorderRequestHead 重排一个完整 HTTP/1.1 请求头块（含结尾的 CRLFCRLF）。
//
//	bodyLen：Content-Length 声明的正文长度；-1 表示 chunked。
//	ok=false：头部块不满足 HTTP/1.1 形态（调用方必须原样透传，不得改写字节）。
//
// 未命中顺序规则（或规则不含该头名）时按原顺序写出，字节与输入一致。
func reorderRequestHead(head []byte) (out []byte, bodyLen int64, ok bool) {
	if len(head) < len(headEndTag) || !bytes.HasSuffix(head, headEndTag) {
		return nil, 0, false
	}
	lines := bytes.Split(head[:len(head)-len(headEndTag)], crlf)
	if len(lines) == 0 || !isHTTPRequestLine(lines[0]) {
		return nil, 0, false
	}
	order := wireOrderForPath(requestPath(lines[0]))

	type headerLine struct {
		lower string
		raw   []byte
	}
	headers := make([]headerLine, 0, len(lines)-1)
	for _, ln := range lines[1:] {
		i := bytes.IndexByte(ln, ':')
		if i <= 0 || !isToken(ln[:i]) {
			// 非法头行（含 obs-fold 续行）一律放弃重排，交由调用方原样透传。
			return nil, 0, false
		}
		lower := strings.ToLower(string(ln[:i]))
		switch lower {
		case "content-length":
			v, err := strconv.ParseInt(strings.TrimSpace(string(ln[i+1:])), 10, 64)
			if err != nil || v < 0 {
				return nil, 0, false
			}
			bodyLen = v
		case "transfer-encoding":
			bodyLen = -1
		}
		headers = append(headers, headerLine{lower: lower, raw: ln})
	}

	res := make([]byte, 0, len(head))
	res = append(res, lines[0]...)
	res = append(res, crlf...)
	used := make([]bool, len(headers))
	for _, name := range order {
		for i, hl := range headers {
			if used[i] || hl.lower != name {
				continue
			}
			used[i] = true
			res = append(res, hl.raw...)
			res = append(res, crlf...)
		}
	}
	// 规则未覆盖的头（如未来新增字段）按原有相对顺序置于其后，保证输出确定。
	for i, hl := range headers {
		if used[i] {
			continue
		}
		res = append(res, hl.raw...)
		res = append(res, crlf...)
	}
	res = append(res, crlf...)
	return res, bodyLen, true
}

// isHTTPRequestLine 判断请求行形态：METHOD SP target SP HTTP/1.x
func isHTTPRequestLine(ln []byte) bool {
	f := bytes.SplitN(ln, []byte(" "), 3)
	if len(f) != 3 || !isToken(f[0]) || len(f[1]) == 0 {
		return false
	}
	return bytes.HasPrefix(f[2], []byte("HTTP/1."))
}

// requestPath 从请求行取出不含查询串的路径。
func requestPath(ln []byte) string {
	f := bytes.SplitN(ln, []byte(" "), 3)
	if len(f) < 2 {
		return ""
	}
	p := string(f[1])
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return p
}

// isToken 判断是否为 RFC 9110 的 token（头名与方法名的字符集）。
func isToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' ||
			c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_' ||
			c == '`' || c == '|' || c == '~':
		default:
			return false
		}
	}
	return true
}

// installWireOrder 给 transport 装上头部顺序复刻钩子。
//
// DialContext：明文 http（含测试用的原始 TCP 监听）在此包一层。
// DialTLSContext：直连 https 时 net/http 优先使用它（配置了代理时不使用），返回的 conn
// 之上即为明文 HTTP/1.1 头部。本层负责 TLS 握手，行为与 net/http 自身一致：
// ServerName 取地址主机、沿用 transport 的 TLSClientConfig、握手超时取 TLSHandshakeTimeout。
// 不设置 ALPN → 与官方客户端一致停留在 HTTP/1.1（h2 的 HPACK 会使键名大小写与顺序消失）。
func installWireOrder(t *http.Transport) {
	dial := t.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newOrderedHeaderConn(conn), nil
	}

	// 已配置代理（或调用方自带 TLS 拨号钩子）时不动手：前者 net/http 不会调用
	// DialTLSContext，后者需要保留调用方的语义。
	if t.Proxy != nil || t.DialTLS != nil || t.DialTLSContext != nil {
		return
	}
	tlsCfg := t.TLSClientConfig
	handshakeTimeout := t.TLSHandshakeTimeout
	if handshakeTimeout == 0 {
		handshakeTimeout = 10 * time.Second
	}
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := &tls.Config{}
		if tlsCfg != nil {
			cfg = tlsCfg.Clone()
		}
		if cfg.ServerName == "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				conn.Close()
				return nil, err
			}
			cfg.ServerName = host
		}
		tlsConn := tls.Client(conn, cfg)
		hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(hctx); err != nil {
			conn.Close()
			return nil, err
		}
		return newOrderedHeaderConn(tlsConn), nil
	}
}
