package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 上游请求头「顺序」对齐回归
//
// net/http 把 Host / User-Agent / Content-Length 单独前置，其余头部按规范化键名字典序
// 写出；官方客户端按属性插入顺序写出。顺序不影响语义，但**只比对线上字节的两次请求**
// 即可据此判定两条请求出自不同实现，故必须逐项复刻。
// -----------------------------------------------------------------------------

// clientModelHeaderOrder 是官方客户端发往 /v2/chat/completions 的线上头部顺序基线
// （键名大小写逐字取自抓包）。
//
// 来源与成因：
//   - 应用层块（Accept → X-Agent-Type）逐字取自桌面客户端 5.7.6 + bundled CLI 2.156.0
//     的模型请求抓包（与 main_test.go 的 realClientChatHeaders 同源、同序）；
//   - Content-Encoding 由 GzipRequestProcessor 追加（模型请求处理器链 priority 升序，
//     该处理器 priority=9999 恒为最后一个），位于传播头之前；
//   - traceparent / b3 / X-B3-* / X-Trace-ID 由 runWithGalileoModelRequestSpan 在处理器链
//     之后、派发之前注入（源码 injectOtelSpanHeaders）；
//   - Authorization / X-User-Id / [-Enterprise-Id / -Department-Info / -Tenant-Id] / X-Domain
//     由 AuthenticationHttpInterceptor 追加，其后是产品 / UA 拦截器的 X-Product / User-Agent；
//   - Content-Length / Accept-Encoding / Host / Connection 由 Node http 模块最后补齐 ——
//     位置取自本机 5.7.6 客户端线上字节抓包：`Accept-Encoding: gzip, compress, deflate, br`
//     位于 Content-Length 之后、Host 之前（该值来自 axios 默认头，由 Node http 适配器
//     在传输层补写；`.client_ref/capture2.jsonl` 那份 5.5.2 抓包未见该头，属抓包层级差异，
//     以本机 5.7.6 线上字节为准）。
//
// 本用例断言的是**网关 → 上游**这一跳：下游入站头经 net/http 服务端解析后已丢失原始顺序，
// 该跳的顺序无法用于断言。
var clientModelHeaderOrder = []string{
	"Accept", "Content-Type", "x-requested-with",
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
	"x-stainless-package-version", "x-stainless-retry-count",
	"x-stainless-runtime", "x-stainless-runtime-version",
	"X-Conversation-ID", "X-Conversation-Request-ID",
	"X-Agent-Intent", "X-Agent-Purpose",
	"X-IDE-Type", "X-IDE-Name", "X-IDE-Version",
	"X-Private-Data", "x-codebuddy-request",
	"X-Request-ID", "X-Conversation-Message-ID", "X-Root-Request-ID",
	"X-Agent-Type",
	"Content-Encoding",
	"traceparent", "b3", "X-B3-TraceId", "X-B3-ParentSpanId", "X-B3-SpanId",
	"X-B3-Sampled", "X-Trace-ID",
	"Authorization", "X-User-Id", "X-Domain",
	"X-Product", "User-Agent",
	"Content-Length", "Accept-Encoding", "Host", "Connection",
}

// wireHeaderNames 按线上顺序取出头名。
func wireHeaderNames(t *testing.T, raw string) []string {
	t.Helper()
	idx := strings.Index(raw, "\r\n\r\n")
	if idx < 0 {
		t.Fatalf("malformed wire request: %q", truncateForLog(raw))
	}
	lines := strings.Split(raw[:idx], "\r\n")
	names := make([]string, 0, len(lines)-1)
	for _, ln := range lines[1:] {
		i := strings.Index(ln, ":")
		if i <= 0 {
			t.Fatalf("malformed header line %q", ln)
		}
		names = append(names, ln[:i])
	}
	return names
}

// 网关发给上游的模型请求，其头部**顺序**必须与官方客户端逐项一致。
func TestUpstreamWireHeaderOrderMatchesClient(t *testing.T) {
	raw := captureUpstreamWire(t, `{"model":"default-model","messages":[{"role":"user","content":"hi"}]}`)
	got := wireHeaderNames(t, raw)

	if len(got) != len(clientModelHeaderOrder) {
		t.Errorf("wire carries %d headers, client baseline has %d\n got: %v",
			len(got), len(clientModelHeaderOrder), got)
	}
	for i := 0; i < len(got) && i < len(clientModelHeaderOrder); i++ {
		if got[i] != clientModelHeaderOrder[i] {
			t.Fatalf("header order diverges at position %d (0-based):\n got: %v\nwant: %v",
				i, got, clientModelHeaderOrder)
		}
	}
	t.Logf("wire header order: %s", strings.Join(got, " → "))
}

// 顺序复刻只在能确认的 HTTP/1.1 请求头上生效：TLS 密文等无法确认的字节必须原样透传，
// 既不改写也不滞留（否则会破坏握手或连接复用）。
func TestOrderedHeaderConnPassesThroughTLSBytes(t *testing.T) {
	conn, peer := orderedTestConn(t)

	// TLS handshake record 前缀 + 一段含 CRLFCRLF 的载荷。
	payload := append([]byte{0x16, 0x03, 0x01, 0x02, 0x00}, []byte("binary\r\n\r\npayload\r\n\r\n")...)

	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		wrote <- err
	}()
	got := readUntilIdle(t, peer)
	if err := <-wrote; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("non-HTTP bytes must pass through verbatim:\n got: %q\nwant: %q", got, payload)
	}
	if conn.state != connStateRaw {
		t.Errorf("conn state = %d, want raw passthrough", conn.state)
	}
}

// 分段写出的请求头要能完整重排；正文（可能包含 CRLFCRLF）必须逐字节透传；
// keep-alive 复用同一条连接时，后续请求同样要重排。
func TestOrderedHeaderConnReordersSegmentedHead(t *testing.T) {
	conn, peer := orderedTestConn(t)

	head := synthHead("POST", chatCompletionsPath,
		"Host: example.com",
		"Content-Length: 5",
		"X-Product: SaaS",
		"Accept: application/json",
	)
	// 客户端顺序：Accept → X-Product → Content-Length → Host
	wantHead := synthHead("POST", chatCompletionsPath,
		"Accept: application/json",
		"X-Product: SaaS",
		"Content-Length: 5",
		"Host: example.com",
	)
	body := []byte("a\r\n\r\nb") // 正文含 CRLFCRLF —— 绝不能被当作头部结束标记
	second := synthHead("POST", chatCompletionsPath,
		"Host: example.com",
		"Content-Length: 0",
		"Accept: application/json",
	)
	wantSecond := synthHead("POST", chatCompletionsPath,
		"Accept: application/json",
		"Content-Length: 0",
		"Host: example.com",
	)

	go func() {
		// 头部任意切分（首片不足 8 字节，第二片跨过请求行）
		_, _ = conn.Write(head[:6])
		_, _ = conn.Write(head[6:31])
		_, _ = conn.Write(head[31:])
		_, _ = conn.Write(body)
		_, _ = conn.Write(second)
	}()

	got := readUntilIdle(t, peer)
	want := make([]byte, 0, len(wantHead)+len(body)+len(wantSecond))
	want = append(want, wantHead...)
	want = append(want, body...)
	want = append(want, wantSecond...)
	if !bytes.Equal(got, want) {
		t.Errorf("segmented head/body/keep-alive mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// 无实测顺序基线的路径（控制面 / 守护进程家族）必须保持字节原样，不得被本层改动。
func TestOrderedHeaderConnLeavesOtherPathsUnchanged(t *testing.T) {
	conn, peer := orderedTestConn(t)

	head := synthHead("POST", "/v2/plugin/auth/state",
		"Host: example.com",
		"Content-Length: 2",
		"X-Domain: www.workbuddy.ai",
		"Accept: application/json, text/plain, */*",
	)
	go func() { _, _ = conn.Write(head) }()
	got := readUntilIdle(t, peer)
	if !bytes.Equal(got, head) {
		t.Errorf("non-model path must be byte-identical:\n got: %q\nwant: %q", got, head)
	}
}

// chunked 正文长度不可预知：重排头部后该连接必须整体转入透传，不能把 chunk 数据当成头部。
func TestOrderedHeaderConnGoesRawAfterChunkedHead(t *testing.T) {
	conn, peer := orderedTestConn(t)

	head := synthHead("POST", chatCompletionsPath,
		"Host: example.com",
		"Transfer-Encoding: chunked",
		"Accept: application/json",
	)
	// chunk 数据里含一个形态完整的「请求头」——若状态机误判就会把它重排，暴露 bug。
	chunks := []byte("5\r\nab\r\n\r\n\r\nPOST /v2/chat/completions HTTP/1.1\r\nHost: x\r\nAccept: application/json\r\n\r\n0\r\n\r\n")
	// Transfer-Encoding 不在客户端顺序表内 → 按已文档化的回退规则置于表内头之后。
	wantHead := synthHead("POST", chatCompletionsPath,
		"Accept: application/json",
		"Host: example.com",
		"Transfer-Encoding: chunked",
	)
	go func() {
		_, _ = conn.Write(head)
		_, _ = conn.Write(chunks)
	}()
	got := readUntilIdle(t, peer)
	want := append(append([]byte{}, wantHead...), chunks...)
	if !bytes.Equal(got, want) {
		t.Errorf("chunked body must pass through verbatim:\n got: %q\nwant: %q", got, want)
	}
}

// 配置代理时 net/http 自行 CONNECT + TLS（不使用 DialTLSContext），本层不得覆盖调用方语义。
func TestInstallWireOrderSkipsTLSDialerWhenProxied(t *testing.T) {
	tr := &http.Transport{}
	pURL, err := url.Parse("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr.Proxy = http.ProxyURL(pURL)
	installWireOrder(tr)
	if tr.DialTLSContext != nil {
		t.Error("proxied transport must not get a custom DialTLSContext")
	}
	if tr.DialContext == nil {
		t.Error("DialContext wrapper must still be installed")
	}
}

// -----------------------------------------------------------------------------
// 测试辅助
// -----------------------------------------------------------------------------

// orderedTestConn 返回（包装后的写入端，原始读取端）。
func orderedTestConn(t *testing.T) (*orderedHeaderConn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return newOrderedHeaderConn(client), server
}

// readUntilIdle 读到空闲为止（空闲即 250ms 无新数据）。
func readUntilIdle(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var out []byte
	tmp := make([]byte, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, err := c.Read(tmp)
		out = append(out, tmp[:n]...)
		if err != nil {
			return out
		}
	}
}

// 直连 https（Transport.DialTLSContext 分支）的线上头部顺序同样必须与客户端一致。
//
// 明文用例覆盖不到该分支：net/http 仅在未配置代理时使用 DialTLSContext，而生产流量正是
// 走这里（本层自行 TLS 握手后在其上重排头部）。用自签证书的裸 TLS 监听取线上字节。
func TestUpstreamWireHeaderOrderOverTLSMatchesClient(t *testing.T) {
	certSrv := httptest.NewUnstartedServer(nil)
	certSrv.StartTLS() // 仅取其自签证书（含 127.0.0.1 SAN）
	defer certSrv.Close()

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawLn.Close()
	tlsLn := tls.NewListener(rawLn, &tls.Config{Certificates: certSrv.TLS.Certificates})

	wireCh := make(chan string, 1)
	go func() {
		conn, err := tlsLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		wireCh <- readWireRequest(conn)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 0\r\n\r\n"))
	}()

	origBase := profileINTL.Base
	profileINTL.Base = "https://" + rawLn.Addr().String()
	defer func() { profileINTL.Base = origBase }()

	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	ht, ok := cfg.HttpClient.Transport.(*hotConfigTransport)
	if !ok {
		t.Fatalf("unexpected transport type %T", cfg.HttpClient.Transport)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certSrv.Certificate())
	ht.transport.TLSClientConfig.RootCAs = pool // 仅测试用：信任自签证书

	acc := &Account{
		Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "wire-token", Domain: "www.workbuddy.ai"},
			Account: StoredAccount{UID: "u1"},
			Edition: "intl",
		},
		Edition: "intl",
	}
	downstream := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	resp, cancel, err := doUpstreamRequest(downstream, acc, &profileINTL,
		[]byte(`{"model":"m","messages":[]}`), 1, "trace", sessionScope{})
	if err != nil {
		t.Fatalf("doUpstreamRequest over TLS: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	var raw string
	select {
	case raw = <-wireCh:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS upstream never received request")
	}
	got := wireHeaderNames(t, raw)
	if len(got) != len(clientModelHeaderOrder) {
		t.Fatalf("TLS wire carries %d headers, client baseline has %d\n got: %v",
			len(got), len(clientModelHeaderOrder), got)
	}
	for i := range got {
		if got[i] != clientModelHeaderOrder[i] {
			t.Fatalf("TLS wire header order diverges at position %d:\n got: %v\nwant: %v",
				i, got, clientModelHeaderOrder)
		}
	}
	t.Logf("TLS wire header order: %s", strings.Join(got, " → "))
}

// synthHead 构造一个请求头块（含结尾 CRLFCRLF）。
func synthHead(method, path string, headerLines ...string) []byte {
	return []byte(method + " " + path + " HTTP/1.1\r\n" + strings.Join(headerLines, "\r\n") + "\r\n\r\n")
}
