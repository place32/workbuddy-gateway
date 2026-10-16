package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveCredentialPathRejectsEscapes(t *testing.T) {
	chdirTemp(t)
	path, err := resolveCredentialPath("workbuddy4.json")
	if err != nil {
		t.Fatalf("合法文件名被拒绝: %v", err)
	}
	if filepath.Base(path) != "workbuddy4.json" || !filepath.IsAbs(path) {
		t.Fatalf("应解析成运行目录下的绝对路径: %s", path)
	}
	for _, bad := range []string{"", "  ", "other.json", "../workbuddy1.json", "sub/workbuddy1.json",
		`sub\workbuddy1.json`, "workbuddy1.json/../workbuddy2.json", "workbuddy*.json"} {
		if _, err := resolveCredentialPath(bad); err == nil {
			t.Fatalf("应拒绝越界或非法的凭据名: %q", bad)
		}
	}
}

func TestSuggestCredentialFileNameSkipsUsed(t *testing.T) {
	chdirTemp(t)
	for _, name := range []string{"workbuddy1.json", "workbuddy3.json", "workbuddy3.json.disabled", "other.json"} {
		if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := suggestCredentialFileName(); got != "workbuddy2.json" {
		t.Fatalf("应跳过已占用的 1 与 3，实际 %s", got)
	}
}

func TestDeleteAccountCredentialGuardsAndRemoval(t *testing.T) {
	isolatedGatewayConfig(t)
	chdirTemp(t)
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "workbuddy7.json")
	if err := os.WriteFile(target, []byte(`{"auth":{"accessToken":"synthetic-not-a-token"},"account":{"uid":"u"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	old := accounts
	accounts = []*Account{{Path: target, Auth: &StoredAuth{}}}
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = old; accountMu.Unlock() })

	// 目录不能被当成凭据删除。
	if err := os.Mkdir(filepath.Join(dir, "workbuddy9.json"), 0700); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	accounts = append(accounts, &Account{Path: filepath.Join(dir, "workbuddy9.json"), Auth: &StoredAuth{}})
	accountMu.Unlock()
	if _, err := deleteAccountCredential("workbuddy9.json"); err == nil {
		t.Fatal("目录必须被拒绝，不能递归删除")
	}

	for _, bad := range []string{"", "../x.json", "other.json", "missing.json"} {
		if _, err := deleteAccountCredential(bad); err == nil {
			t.Fatalf("应拒绝删除: %q", bad)
		}
	}
	if _, err := deleteAccountCredential("workbuddy7.json"); err != nil {
		t.Fatalf("正常删除失败: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("凭据文件没有被删除")
	}
}

func TestAccountAddEndpointValidatesInputWithoutUpstream(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-account-admin"}); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t)
	handler := newWebUIHandler()
	if got := adminRequest(handler, "GET", "/admin/api/account-add", "", ""); got.Code != 401 {
		t.Fatalf("未鉴权必须拒绝: %d", got.Code)
	}
	got := adminRequest(handler, "GET", "/admin/api/account-add", "synthetic-account-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), "defaultFile") {
		t.Fatalf("读取默认名失败: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "/") {
		t.Fatal("默认名不能回传目录信息")
	}
	for _, body := range []string{`{"site":"mars","file":"workbuddy1.json"}`,
		`{"site":"cn","file":"../workbuddy1.json"}`, `{"site":"cn","file":"other.json"}`} {
		got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin", body)
		if got.Code != 400 {
			t.Fatalf("非法参数必须 400: %s -> %d %s", body, got.Code, got.Body.String())
		}
	}
	if err := os.WriteFile("workbuddy1.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin",
		`{"site":"cn","file":"workbuddy1.json"}`)
	if got.Code != 409 {
		t.Fatalf("同名凭据必须拒绝覆盖: %d %s", got.Code, got.Body.String())
	}
	got = adminRequest(handler, "POST", "/admin/api/account-delete", "synthetic-account-admin", `{"file":"../x.json"}`)
	if got.Code != 400 {
		t.Fatalf("删除越界路径必须 400: %d", got.Code)
	}
	got = adminRequest(handler, "DELETE", "/admin/api/account-delete", "synthetic-account-admin", "")
	// 管理写操作被同源 JSON POST 策略保护，DELETE 必须在进入业务前就被挡下。
	if got.Code == 200 {
		t.Fatalf("删除接口不应接受 DELETE: %d %s", got.Code, got.Body.String())
	}
}

func TestBeginAccountLoginUsesThreeControlPlaneRequests(t *testing.T) {
	isolatedGatewayConfig(t)
	initHTTPClient()
	accountMu.Lock()
	oldAccounts := accounts
	accounts = nil
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	for _, site := range []string{"cn", "intl"} {
		t.Run(site, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "workbuddy-login-"+site+".json")
			var mu sync.Mutex
			var requests []*http.Request
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, r.Clone(r.Context()))
				bodies = append(bodies, string(body))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				var data any
				switch {
				case strings.Contains(r.URL.Path, "/auth/state"):
					data = map[string]string{"state": "state-" + site, "authUrl": "https://login.example/" + site}
				case strings.Contains(r.URL.Path, "/auth/token"):
					data = map[string]any{"accessToken": "access-" + site, "refreshToken": "refresh", "expiresIn": 3600, "domain": map[string]string{"cn": "token.cn.example"}[site]}
				case strings.Contains(r.URL.Path, "/login/account"):
					data = map[string]string{"uid": "uid-" + site, "enterpriseId": "ent-" + site, "nickname": "name"}
				default:
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
			}))
			defer server.Close()
			prof := profileForEdition(site)
			oldBase := prof.Base
			prof.Base = server.URL
			defer func() { prof.Base = oldBase }()
			s, err := beginAccountLogin(site, target)
			if err != nil {
				t.Fatal(err)
			}
			defer s.cancel()
			deadline := time.After(6 * time.Second)
			for {
				s.mu.Lock()
				status := s.status
				s.mu.Unlock()
				if status != "pending" {
					if status != "done" {
						t.Fatalf("login status = %q", status)
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("login did not complete")
				case <-time.After(20 * time.Millisecond):
				}
			}
			mu.Lock()
			got := append([]*http.Request(nil), requests...)
			mu.Unlock()
			if len(got) != 3 {
				t.Fatalf("got %d upstream requests, want state/token/account", len(got))
			}
			if bodies[0] != "{}" || bodies[1] != "" || bodies[2] != "" {
				t.Fatalf("request bodies = %q, want [{}, empty, empty]", bodies)
			}
			if got[0].Header.Get("Content-Type") != "application/json" {
				t.Errorf("state Content-Type = %q", got[0].Header.Get("Content-Type"))
			}
			if got[0].Method != http.MethodPost || got[1].Method != http.MethodGet || got[2].Method != http.MethodGet {
				t.Fatalf("methods = %s, %s, %s", got[0].Method, got[1].Method, got[2].Method)
			}
			if got[0].Header.Get("X-Domain") != server.URL[strings.Index(server.URL, "://")+3:] {
				t.Fatalf("state X-Domain = %q", got[0].Header.Get("X-Domain"))
			}
			if got[1].Header.Get("X-Domain") != "" || got[1].Header.Get("Content-Type") != "" {
				t.Fatalf("token must omit domain/content type: domain=%q content-type=%q", got[1].Header.Get("X-Domain"), got[1].Header.Get("Content-Type"))
			}
			wantDomain := server.URL[strings.Index(server.URL, "://")+3:]
			if site == "cn" {
				wantDomain = "token.cn.example"
			}
			if got[2].Header.Get("Authorization") != "Bearer access-"+site || got[2].Header.Get("X-Domain") != wantDomain || got[2].Header.Get("Content-Type") != "" {
				t.Fatalf("account headers authorization=%q domain=%q content-type=%q", got[2].Header.Get("Authorization"), got[2].Header.Get("X-Domain"), got[2].Header.Get("Content-Type"))
			}
			for i, req := range got {
				for name, want := range map[string]string{
					"Accept":               "application/json, text/plain, */*",
					"X-Requested-With":     "XMLHttpRequest",
					"X-Product":            "SaaS",
					"User-Agent":           map[string]string{"cn": "WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1", "intl": "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0"}[site],
					"Accept-Encoding":      map[string]string{"intl": "gzip, compress, deflate, br"}[site],
					"X-No-User-Id":         "true",
					"X-No-Enterprise-Id":   "true",
					"X-No-Department-Info": "true",
				} {
					if actual := req.Header.Get(name); actual != want {
						t.Errorf("request %d %s = %q, want %q", i, name, actual, want)
					}
				}
				wantNoAuth, wantAuth := "true", ""
				if i == 2 {
					wantNoAuth, wantAuth = "", "Bearer access-"+site
				}
				if req.Header.Get("X-No-Authorization") != wantNoAuth || req.Header.Get("Authorization") != wantAuth {
					t.Errorf("request %d authorization bypass mismatch", i)
				}
				trace, span := req.Header.Get("X-Trace-ID"), req.Header.Get("X-B3-SpanId")
				if !isHexLen(trace, 32) || !isHexLen(span, 16) || req.Header.Get("X-Request-ID") != trace || req.Header.Get("X-B3-TraceId") != trace || req.Header.Get("b3") != trace+"-"+span+"-1-" || req.Header.Get("X-B3-Sampled") != "1" {
					t.Errorf("request %d trace headers are missing or inconsistent", i)
				}
				for name := range req.Header {
					lower := strings.ToLower(name)
					for _, prefix := range []string{"x-ide-", "x-agent-", "x-stainless-", "x-conversation-", "x-session-"} {
						if strings.HasPrefix(lower, prefix) {
							t.Errorf("request %d has model-only header %s", i, name)
						}
					}
				}
				for _, modelHeader := range []string{"X-IDE-Type", "X-Agent-Type", "X-Agent-Intent", "x-stainless-runtime-version", "traceparent"} {
					if req.Header.Get(modelHeader) != "" {
						t.Errorf("request %d has model header %s", i, modelHeader)
					}
				}
			}
		})
	}
}
