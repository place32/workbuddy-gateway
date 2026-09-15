package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 验证 429 消息中的重置时间解析
func TestParseResetTime(t *testing.T) {
	msg := `{"code":429,"message":"upstream 429: {\"code\":6004,\"msg\":\"您的使用量已超出频率限制，将在 2026-09-04 07:48:15 UTC+8 重置，您也可以切换其他模型继续使用。\",\"requestId\":\"149e3299-ddf3-4ad1-aaa8-cc752c37630a\"}","type":"upstream_error"}`
	got, ok := parseResetTime(msg)
	if !ok {
		t.Fatal("parseResetTime should succeed")
	}
	want := time.Date(2026, 9, 4, 7, 48, 15, 0, time.FixedZone("UTC+8", 8*3600))
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	t.Logf("parsed reset time: %v", got)
}

// 验证无法解析时返回 false
func TestParseResetTimeInvalid(t *testing.T) {
	if _, ok := parseResetTime("some random error"); ok {
		t.Fatal("should not parse random string")
	}
}

// 验证国际站英文限流消息的重置时间解析（通用兜底正则）
func TestParseResetTimeEnglish(t *testing.T) {
	cases := []string{
		`{"code":6004,"msg":"Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8."}`,
		`upstream 429: rate limit exceeded, reset at 2026-09-05 01:57:00`,
		`{"msg":"quota exceeded, will reset on 2026-09-05T01:57:00 UTC+8"}`,
	}
	want := time.Date(2026, 9, 5, 1, 57, 0, 0, time.FixedZone("UTC+8", 8*3600))
	for _, msg := range cases {
		got, ok := parseResetTime(msg)
		if !ok {
			t.Fatalf("parseResetTime(%q) should succeed", msg)
		}
		if !got.Equal(want) {
			t.Fatalf("parseResetTime(%q) = %v, want %v", msg, got, want)
		}
	}
}

// 验证站点 Profile 选择：空值/未知回退国内站，intl 系列值命中国际站
func TestProfileForEdition(t *testing.T) {
	cases := []struct {
		edition string
		wantKey string
	}{
		{"", "cn"},
		{"cn", "cn"},
		{"unknown", "cn"},
		{"intl", "intl"},
		{"INTL", "intl"},
		{" workbuddy.ai ", "intl"},
		{"international", "intl"},
	}
	for _, c := range cases {
		if got := profileForEdition(c.edition).Key; got != c.wantKey {
			t.Errorf("profileForEdition(%q).Key = %s, want %s", c.edition, got, c.wantKey)
		}
	}
	if p := profileForEdition("intl"); p.Base != "https://www.workbuddy.ai" || p.PlatformName != "WorkBuddy" {
		t.Errorf("intl profile base/platformName unexpected: %+v", p)
	}
	if p := profileForEdition("cn"); p.Base != "https://copilot.tencent.com" || p.Platform != "VSCode" {
		t.Errorf("cn profile base/platform unexpected: %+v", p)
	}
	// 两个站点共享同一客户端版本基线（desktop 5.5.2 + bundled CLI 2.137.1）
	for _, p := range []*upstreamProfile{&profileCN, &profileINTL} {
		if p.PlatformVersion != "5.5.2" || p.CliVersion != "2.137.1" {
			t.Errorf("profile %s client version baseline unexpected: %+v", p.Key, p)
		}
	}
}

// 验证各站点上游 URL 构建与旧版常量完全一致（国内站回归）+ 国际站正确
func TestUpstreamProfileURLs(t *testing.T) {
	cn := profileForEdition("cn")
	if got := cn.authStateURL(); got != "https://copilot.tencent.com/v2/plugin/auth/state?platform=VSCode" {
		t.Errorf("cn authStateURL = %s", got)
	}
	if got := cn.chatURL(); got != "https://copilot.tencent.com/v2/chat/completions" {
		t.Errorf("cn chatURL = %s", got)
	}
	if got := cn.tokenRefreshURL(); got != "https://copilot.tencent.com/v2/plugin/auth/token/refresh" {
		t.Errorf("cn tokenRefreshURL = %s", got)
	}
	if got := cn.quotaSummaryURL(); got != "https://www.codebuddy.cn/billing/meter/get-user-resource-summary" {
		t.Errorf("cn quotaSummaryURL = %s", got)
	}

	itl := profileForEdition("intl")
	if got := itl.authStateURL(); got != "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai" {
		t.Errorf("intl authStateURL = %s", got)
	}
	if got := itl.authTokenURL("abc-123"); got != "https://www.workbuddy.ai/v2/plugin/auth/token?state=abc-123" {
		t.Errorf("intl authTokenURL = %s", got)
	}
	if got := itl.loginAcctURL("abc-123"); got != "https://www.workbuddy.ai/v2/plugin/login/account?state=abc-123" {
		t.Errorf("intl loginAcctURL = %s", got)
	}
	if got := itl.chatURL(); got != "https://www.workbuddy.ai/v2/chat/completions" {
		t.Errorf("intl chatURL = %s", got)
	}
	if got := itl.quotaSummaryURL(); got != "https://www.workbuddy.ai/billing/meter/get-user-resource-summary" {
		t.Errorf("intl quotaSummaryURL = %s", got)
	}
}

// 验证账号站点路由：优先取凭据内 edition；失效标记恢复（Auth 为 nil）时取账号上的 edition
func TestAccountProfile(t *testing.T) {
	acc := &Account{Auth: &StoredAuth{Edition: "intl"}}
	if acc.Profile().Key != "intl" {
		t.Fatalf("expected intl via Auth.Edition, got %s", acc.Profile().Key)
	}
	// 失效标记恢复场景：凭据文件已删除
	acc2 := &Account{Edition: "intl"}
	if acc2.Profile().Key != "intl" {
		t.Fatalf("expected intl via Account.Edition, got %s", acc2.Profile().Key)
	}
	// 旧版凭据（无 edition 字段）回退国内站
	acc3 := &Account{Auth: &StoredAuth{}}
	if acc3.Profile().Key != "cn" {
		t.Fatalf("expected cn fallback, got %s", acc3.Profile().Key)
	}
}

// 验证限流识别（429 状态码 / code 6004 / 频率限制关键词，含中英文）
func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, "{}", true},
		{400, `{"code":6004,"msg":"x"}`, true},
		{400, "频率限制", true},
		{400, "frequency limit", true},
		{400, "rate limit exceeded", true},
		{400, "Rate Limit Exceeded", true},
		{400, "ratelimit", true},
		{400, "Too Many Requests", true},
		{400, "some other error", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isRateLimited(c.status, c.body); got != c.want {
			t.Errorf("isRateLimited(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 验证轮询选择：两个账号交替返回
func TestNextAccountRoundRobin(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a1, _ := nextAccount()
	a2, _ := nextAccount()
	a3, _ := nextAccount()
	if a1.Path != "a.json" || a2.Path != "b.json" || a3.Path != "a.json" {
		t.Fatalf("round robin failed: %s %s %s", a1.Path, a2.Path, a3.Path)
	}
	t.Logf("round-robin order: %s %s %s", a1.Path, a2.Path, a3.Path)
}

// 验证冷却屏蔽：冷却中的账号被跳过，由另一账号代偿
func TestNextAccountSkipsCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-cooldown), got %s", a.Path)
	}
	t.Logf("cooldown skip works, selected %s", a.Path)
}

// 验证全部冷却时返回错误
func TestNextAccountAllCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(2 * time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts in cooldown")
	}
	t.Logf("all-cooldown error: %v", err)
}

// 验证冷却到期后自动恢复
func TestNextAccountCooldownExpiry(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(-time.Minute)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "a.json" {
		t.Fatalf("expected a.json (cooldown expired), got %s", a.Path)
	}
	t.Logf("expired cooldown recovers, selected %s", a.Path)
}

func TestNextAccountSkipsQuotaExhausted(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "empty.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true},
		{Path: "available.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 10},
	}
	rrIndex = 0
	accountMu.Unlock()

	acc, err := nextAccount()
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "available.json" {
		t.Fatalf("expected available account, got %s", acc.Path)
	}
}

func TestNextAccountAllQuotaExhausted(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true},
		{Path: "b.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil || !strings.Contains(err.Error(), "额度均已耗尽") {
		t.Fatalf("expected quota exhausted error, got %v", err)
	}
}

func TestIsQuotaExhausted(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`, true},
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted"}}}`, true},
		{429, `{"code":6004,"msg":"rate limit"}`, false},
		{400, `{"code":14018,"msg":"额度已用尽"}`, true},
	}
	for _, tc := range cases {
		if got := isQuotaExhausted(tc.status, tc.body); got != tc.want {
			t.Errorf("isQuotaExhausted(%d, %q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// 验证授权失效识别（401/403 / invalid token / 登录过期等）
func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, "{}", true},
		{403, "{}", true},
		{400, `{"msg":"invalid token"}`, true},
		{400, "unauthorized", true},
		{400, "登录已过期", true},
		{400, "登录失效，请重新登录", true},
		{400, "some other error", false},
		{429, "频率限制", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isAuthFailure(c.status, c.body); got != c.want {
			t.Errorf("isAuthFailure(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 验证轮询跳过已失效（Disabled）账号
func TestNextAccountSkipsDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-disabled), got %s", a.Path)
	}
	t.Logf("disabled skip works, selected %s", a.Path)
}

// 验证全部失效时返回错误并提示重新登录
func TestNextAccountAllDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "expired"},
		{Path: "b.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts disabled")
	}
	t.Logf("all-disabled error: %v", err)
}

// 验证 disableAccount：标记失效、删除凭据文件、写入失效标记文件
func TestDisableAccount(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	// 写一个假的凭据文件
	if err := os.WriteFile(authPath, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	acc := &Account{Path: authPath, Auth: &StoredAuth{
		Auth:    StoredTokens{AccessToken: "x"},
		Account: StoredAccount{Nickname: "tester", UID: "uid-1"},
	}}

	disableAccount(acc, "令牌刷新失败 (HTTP 401): invalid token")

	accountMu.Lock()
	disabled := acc.Disabled
	reason := acc.DisabledReason
	accountMu.Unlock()
	if !disabled {
		t.Fatal("account should be marked disabled")
	}
	if reason == "" {
		t.Fatal("disabled reason should be recorded")
	}
	// 凭据文件应被删除
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("credential file should be deleted, stat err=%v", err)
	}
	// 失效标记文件应存在
	if _, err := os.Stat(markerPath(authPath)); err != nil {
		t.Fatalf("marker file should exist: %v", err)
	}
	t.Logf("disableAccount works: disabled=%v reason=%q", disabled, reason)
}

// 验证失效标记文件可被恢复为失效账号（Auth 为 nil），并提示重新登录
func TestLoadDisabledMarkers(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"

	// 构造失效标记文件
	marker := disabledMarker{
		Path:       authPath,
		Reason:     "授权失效（令牌刷新失败 HTTP 401）",
		DisabledAt: time.Now().Unix(),
		Nickname:   "tester",
		UID:        "uid-1",
	}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	// 模拟 -auth 显式指定该路径
	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = authPath
	cfg.AuthDir = ""
	cfg.AuthExplicit = true

	accounts = nil
	loadDisabledMarkers()

	if len(accounts) != 1 {
		t.Fatalf("expected 1 disabled account, got %d", len(accounts))
	}
	acc := accounts[0]
	if !acc.Disabled || acc.Auth != nil {
		t.Fatalf("expected disabled account with nil Auth, got disabled=%v authNil=%v", acc.Disabled, acc.Auth == nil)
	}
	if acc.Nickname != "tester" || acc.UID != "uid-1" {
		t.Fatalf("marker nickname/uid not restored: %+v", acc)
	}
	t.Logf("loadDisabledMarkers restores: %s (nickname=%s)", acc.Path, acc.Nickname)
}

// 验证登录成功后清除失效标记
func TestClearDisabledMarker(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	marker := disabledMarker{Path: authPath, Reason: "revoked"}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	clearDisabledMarker(authPath)
	if _, err := os.Stat(markerPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("marker file should be removed, stat err=%v", err)
	}
	t.Log("clearDisabledMarker works")
}

// 验证自动发现模式：未指定 -auth/-auth-dir 时，扫描当前目录下所有 workbuddy*.json
func TestCollectConfiguredAuthPathsAutoDiscover(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 模拟目录内多个凭据文件 + 非凭据文件 + 失效标记 + 运行时状态快照（应被排除）
	for _, name := range []string{"workbuddy.json", "workbuddy2.json", "workbuddy-3.json", "other.json", "workbuddy.json.disabled", "workbuddy-status.json"} {
		if err := os.WriteFile(name, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 3 {
		t.Fatalf("expected 3 workbuddy json files, got %d: %v", len(paths), paths)
	}
	want := []string{"workbuddy-3.json", "workbuddy.json", "workbuddy2.json"} // sort.Strings 排序结果
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("paths[%d] = %s, want %s (all: %v)", i, paths[i], p, paths)
		}
	}
	t.Logf("auto-discover paths: %v", paths)
}

// 验证自动发现模式：目录内没有任何凭据文件时回退到默认路径
func TestCollectConfiguredAuthPathsAutoDiscoverFallback(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 目录为空

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 1 || paths[0] != "workbuddy.json" {
		t.Fatalf("expected fallback to default workbuddy.json, got %v", paths)
	}
	t.Logf("fallback path: %v", paths)
}

// 验证凭据热加载：新增 / 更新 / 删除凭据文件均原地收敛账号池，无需重启
func TestReloadAccounts(t *testing.T) {
	dir := t.TempDir()

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
		accountMu.Lock()
		accounts = nil
		rrIndex = 0
		accountMu.Unlock()
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = dir
	cfg.AuthExplicit = false

	accountMu.Lock()
	accounts = nil
	rrIndex = 0
	accountMu.Unlock()

	pa := filepath.Join(dir, "workbuddy-a.json")

	// 1) 新增凭据文件 → 自动入池
	if err := os.WriteFile(pa, []byte(`{"auth":{"accessToken":"a1"},"account":{"nickname":"A"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !reloadAccounts() {
		t.Fatal("expected changed=true after adding new credential file")
	}
	if len(accounts) != 1 || accounts[0].Auth.Auth.AccessToken != "a1" {
		t.Fatalf("expected 1 account with a1, got %+v", accounts)
	}
	t.Logf("hot-add works: %s joined the pool", accounts[0].Path)

	// 2) 内容变更（含站点切换 cn -> intl）→ 原地替换凭据
	if err := os.WriteFile(pa, []byte(`{"auth":{"accessToken":"a2-longer"},"account":{"nickname":"A2"},"edition":"intl"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !reloadAccounts() {
		t.Fatal("expected changed=true after credential content change")
	}
	if len(accounts) != 1 || accounts[0].Auth.Auth.AccessToken != "a2-longer" {
		t.Fatalf("expected hot-reloaded a2 credential, got %+v", accounts)
	}
	if accounts[0].Profile().Key != "intl" {
		t.Fatalf("expected intl profile after reload, got %s", accounts[0].Profile().Key)
	}
	t.Logf("hot-update works: edition now %s", accounts[0].Profile().Key)

	// 3) 失效幻影账号：文件被删但账号 Disabled → 保留（用于提示重新登录）
	accountMu.Lock()
	accounts[0].Disabled = true
	accountMu.Unlock()
	if err := os.Remove(pa); err != nil {
		t.Fatal(err)
	}
	if reloadAccounts() {
		t.Fatal("disabled phantom account should be kept, expected no change")
	}
	if len(accounts) != 1 || !accounts[0].Disabled {
		t.Fatalf("disabled phantom should be kept, got %d accounts", len(accounts))
	}
	t.Log("disabled phantom kept after file deletion")

	// 4) 非失效账号文件被删 → 移出账号池
	accountMu.Lock()
	accounts[0].Disabled = false
	accounts[0].Auth = &StoredAuth{Auth: StoredTokens{AccessToken: "x"}}
	accountMu.Unlock()
	if !reloadAccounts() {
		t.Fatal("expected changed=true after credential file deletion")
	}
	if len(accounts) != 0 {
		t.Fatalf("expected empty pool after deletion, got %d", len(accounts))
	}
	t.Log("hot-remove works: pool emptied after file deletion")
}

// 验证 tailLines 读取文件末尾 N 行
func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/test.log"
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "line4" || lines[1] != "line5" {
		t.Fatalf("tailLines got %v", lines)
	}
	t.Logf("tailLines(2) = %v", lines)
}

// 验证状态快照写入：含 active / cooldown / disabled 三种状态
func TestWriteStatusSnapshot(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "alice", UID: "u1"},
		}, QuotaTotal: 2000, QuotaUsed: 1500, QuotaRemaining: 500, IsPaidUser: true, QuotaKnown: true},
		{Path: "b.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "bob", UID: "u2"},
		}, CooldownUntil: time.Now().Add(30 * time.Minute), CooldownMsg: "频率限制"},
		{Path: "c.json", Disabled: true, DisabledReason: "revoked", Nickname: "carol", UID: "u3"},
	}
	accountMu.Unlock()

	writeStatusSnapshot()

	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 3 {
		t.Fatalf("expected 3 accounts in snapshot, got %d", len(snap.Accounts))
	}
	states := map[string]bool{}
	for _, a := range snap.Accounts {
		states[a.State] = true
	}
	if !states["active"] || !states["cooldown"] || !states["disabled"] {
		t.Fatalf("expected all three states in snapshot, got %v", states)
	}
	if snap.Accounts[0].QuotaTotal != 2000 || snap.Accounts[0].QuotaUsed != 1500 || snap.Accounts[0].QuotaRemaining != 500 || !snap.Accounts[0].IsPaidUser || !snap.Accounts[0].QuotaKnown {
		t.Fatalf("quota fields not preserved in snapshot: %+v", snap.Accounts[0])
	}
	t.Logf("snapshot states: %v", states)
}

func TestWriteStatusSnapshotMarksExpiredToken(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: "expired.json", Auth: &StoredAuth{
		Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(-time.Hour).Unix()},
		Account: StoredAccount{Nickname: "expired-user"},
	}}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	writeStatusSnapshot()
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 1 || snap.Accounts[0].State != "expired" {
		t.Fatalf("expected expired state, got %+v", snap.Accounts)
	}
}

func TestRenderAccountTableUsesFullYearAndNoEmoji(t *testing.T) {
	expires := time.Date(2027, 9, 5, 1, 36, 55, 0, time.Local)
	table := renderAccountTable([]accountSnapshot{{
		Path:           "workbuddy4.json",
		Edition:        "intl",
		Nickname:       "user@example.com",
		State:          "quota_exhausted",
		TokenExpiresAt: expires.Unix(),
		QuotaTotal:     1100,
		QuotaUsed:      1100,
		QuotaRemaining: 0,
		IsPaidUser:     false,
		QuotaKnown:     true,
		QuotaExhausted: true,
	}})
	for _, want := range []string{"凭据文件", "workbuddy4.json", "国际站", "额度耗尽", "2027-09-05 01:36:55", "总额度", "已用", "剩余", "付费用户", "1100", "否"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "说明") {
		t.Fatalf("table should not contain removed description column:\n%s", table)
	}
	if strings.ContainsAny(table, "✅🔒❌🕐📊📜⚠️") {
		t.Fatalf("table should not contain emoji:\n%s", table)
	}
}

func TestParseQuotaSummary(t *testing.T) {
	data := []byte(`{"Packages":[{"CycleTotalCapacity":"1500","CycleUsedCapacity":"69.98999993","CycleRemainCapacity":"1430.01000007"},{"CycleTotalCapacity":"500","CycleUsedCapacity":"500","CycleRemainCapacity":"0"}],"IsPaidUser":true}`)
	total, used, remaining, paid, err := parseQuotaSummary(data)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2000 || used != 569.98999993 || remaining != 1430.01000007 || !paid {
		t.Fatalf("unexpected quota summary: total=%v used=%v remaining=%v paid=%v", total, used, remaining, paid)
	}
}

func TestFormatQuotaRoundsToTwoDecimals(t *testing.T) {
	cases := map[float64]string{
		4566:               "4566",
		72.01999992:        "72.02",
		4493.9800000800005: "4493.98",
		0.001:              "0",
	}
	for input, want := range cases {
		if got := formatQuota(input); got != want {
			t.Errorf("formatQuota(%v)=%q want %q", input, got, want)
		}
	}
}

func TestRenderAccountTableRowsHaveEqualDisplayWidth(t *testing.T) {
	table := renderAccountTable([]accountSnapshot{
		{Path: "workbuddy1.json", Nickname: "Abandon", Edition: "cn", State: "quota_exhausted", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 2000},
		{Path: "workbuddy2.json", Nickname: "啊水", Edition: "cn", State: "active", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 69.98999993, QuotaRemaining: 1930.01000007},
	})
	lines := strings.Split(table, "\n")
	want := displayWidth(lines[0])
	for i, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("line %d display width=%d want=%d:\n%s", i, got, want, table)
		}
	}
}

// 构造 messages 便于表驱动测试
func msg(role, content string) *jsonObject {
	return newObject("role", role, "content", content)
}

// mustOrdered 把 JSON 字面量解析为保序对象，用于构造贴近真实入参的测试输入。
func mustOrdered(t *testing.T, s string) *jsonObject {
	t.Helper()
	obj, err := decodeOrderedJSON([]byte(s))
	if err != nil {
		t.Fatalf("decodeOrderedJSON(%s): %v", s, err)
	}
	return obj
}

// 提取 messages 各条 role，便于断言
func rolesOf(obj *jsonObject) []string {
	raw, _ := obj.Get("messages")
	messages, _ := raw.([]any)
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, roleOfMessage(m))
	}
	return roles
}

// 验证会话结构归一化：修复上游 11128「first message is not system prompt」
func TestEnsureLeadingSystemMessage(t *testing.T) {
	cases := []struct {
		name       string
		messages   []any
		wantRoles  []string
		wantInject bool // 首条是否应为注入的保底 system
	}{
		{
			name:      "首条已是 system 保持原样",
			messages:  []any{msg("system", "You are helpful"), msg("user", "hi")},
			wantRoles: []string{"system", "user"},
		},
		{
			name:       "首条 user 且无 system（国内站宽容/国际站必须，统一注入 system）",
			messages:   []any{msg("user", "hi")},
			wantRoles:  []string{"system", "user"},
			wantInject: true,
		},
		{
			name:       "首条 assistant（续写）注入 system",
			messages:   []any{msg("assistant", "Sure")},
			wantRoles:  []string{"system", "assistant"},
			wantInject: true,
		},
		{
			name:       "首条 tool（仅回传工具结果）注入 system",
			messages:   []any{msg("tool", "result")},
			wantRoles:  []string{"system", "tool"},
			wantInject: true,
		},
		{
			name:      "后续 system 提升到首位",
			messages:  []any{msg("user", "hi"), msg("system", "You are helpful"), msg("user", "bye")},
			wantRoles: []string{"system", "user", "user"},
		},
		{
			name:      "首条 developer 归一化为 system",
			messages:  []any{msg("developer", "You are helpful"), msg("user", "hi")},
			wantRoles: []string{"system", "user"},
		},
		{
			name:       "空 messages 注入 system",
			messages:   []any{},
			wantRoles:  []string{"system"},
			wantInject: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := newJSONObject()
			obj.Set("messages", c.messages)
			ensureLeadingSystemMessage(obj)
			got := rolesOf(obj)
			if len(got) != len(c.wantRoles) {
				t.Fatalf("roles = %v, want %v", got, c.wantRoles)
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Fatalf("roles = %v, want %v", got, c.wantRoles)
				}
			}
			raw, _ := obj.Get("messages")
			messages, _ := raw.([]any)
			first, _ := messages[0].(*jsonObject)
			if c.wantInject {
				content, _ := first.Get("content")
				if s, _ := content.(string); s != defaultSystemPrompt {
					t.Fatalf("injected system content = %q, want %q", s, defaultSystemPrompt)
				}
			}
			t.Logf("roles -> %v", got)
		})
	}
}

// 验证缺失 / 非法 messages 字段时也能安全注入（不得 panic）
func TestEnsureLeadingSystemMessageMissingField(t *testing.T) {
	obj := newJSONObject()
	ensureLeadingSystemMessage(obj)
	raw, ok := obj.Get("messages")
	messages, ok2 := raw.([]any)
	if !ok || !ok2 || len(messages) != 1 {
		t.Fatalf("expected 1 injected message, got %#v", raw)
	}
	if roleOfMessage(messages[0]) != "system" {
		t.Fatalf("expected system first, got %v", roleOfMessage(messages[0]))
	}
	t.Log("missing messages field handled")
}

// 验证 developer 角色（GPT-5/Codex）被归一化为 system，避免上游 11128
// "Illegal API invocation from an unapproved channel"
func TestSanitizeMessagesNormalizesDeveloperRole(t *testing.T) {
	obj := newJSONObject()
	obj.Set("messages", []any{
		msg("system", "You are helpful."),
		msg("developer", "Be terse."),
		msg("user", "hi"),
	})
	sanitizeMessages(obj)
	roles := rolesOf(obj)
	for _, r := range roles {
		if r == "developer" {
			t.Fatalf("developer role should be normalized, got %v", roles)
		}
	}
	if roles[1] != "system" {
		t.Fatalf("roles = %v, want second = system", roles)
	}
	t.Logf("roles normalized: %v", roles)
}

// 验证上游 tool_calls 增量按 index 正确归并为一个完整工具调用
func TestApplyToolCallDeltaMerge(t *testing.T) {
	merged := map[int]*mergedToolCall{}
	var order []int
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": ""}},
	})
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `{"city":`}},
	})
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `"Beijing"}`}},
	})
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order = %v, want [0]", order)
	}
	st := merged[0]
	if st.ID != "call_1" || st.Name != "get_weather" {
		t.Fatalf("id/name = %q/%q", st.ID, st.Name)
	}
	if got := st.Args.String(); got != `{"city":"Beijing"}` {
		t.Fatalf("args = %q", got)
	}
	t.Logf("merged tool call: %s(%s) id=%s", st.Name, st.Args.String(), st.ID)
}

// 验证 aggregateCompletion 正确合并流式 tool_calls（修复旧的按片追加问题）
func TestAggregateCompletionToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"cmpl-1","model":"hy3-preview","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")

	out, err := aggregateCompletion(strings.NewReader(sse), "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	choices := chat["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected 1 merged tool_call, got %#v", msg["tool_calls"])
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Beijing"}` || call["id"] != "call_1" {
		t.Fatalf("bad merged call: %#v", call)
	}
	t.Logf("aggregateCompletion merged: %v", call)
}

// 验证 Responses -> Chat Completions 请求转换（instructions/input/tools/tool_choice/参数）
func TestResponsesToChatRequest(t *testing.T) {
	respReq := mustOrdered(t, `{
		"model": "hy3-preview",
		"instructions": "You are helpful.",
		"max_output_tokens": 128,
		"temperature": 0.3,
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "weather?"}]},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"BJ\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}
		],
		"tools": [{"type": "function", "name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}],
		"tool_choice": "auto",
		"reasoning": {"effort": "high"}
	}`)

	chat, err := responsesToChatRequest(respReq, "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := chat.Get("model"); v != "hy3-preview" {
		t.Fatalf("model = %v", v)
	}
	if v, _ := chat.Get("max_tokens"); v != json.Number("128") {
		t.Fatalf("max_tokens = %#v, want json.Number(128)", v)
	}
	if v, _ := chat.Get("temperature"); v != json.Number("0.3") {
		t.Fatalf("temperature = %#v, want json.Number(0.3)", v)
	}
	if v, _ := chat.Get("reasoning_effort"); v != "high" {
		t.Fatalf("reasoning_effort = %v", v)
	}
	raw, _ := chat.Get("messages")
	messages, _ := raw.([]any)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages (system+user+assistant+tool), got %d: %#v", len(messages), messages)
	}
	if rolesOf(chat)[0] != "system" {
		t.Fatalf("first message should be system")
	}
	assistant, _ := messages[2].(*jsonObject)
	if v, _ := assistant.Get("role"); v != "assistant" {
		t.Fatalf("3rd message role = %v", v)
	}
	toolMsg, _ := messages[3].(*jsonObject)
	role, _ := toolMsg.Get("role")
	callID, _ := toolMsg.Get("tool_call_id")
	content, _ := toolMsg.Get("content")
	if role != "tool" || callID != "call_1" || content != "sunny" {
		t.Fatalf("tool message = %#v", toolMsg)
	}
	toolsRaw, _ := chat.Get("tools")
	tools, _ := toolsRaw.([]any)
	firstTool, _ := tools[0].(*jsonObject)
	fnRaw, _ := firstTool.Get("function")
	fn, _ := fnRaw.(*jsonObject)
	if name, _ := fn.Get("name"); name != "get_weather" {
		t.Fatalf("tools not flattened: %#v", tools)
	}
	if v, _ := chat.Get("tool_choice"); v != "auto" {
		t.Fatalf("tool_choice = %v", v)
	}
	t.Log("responses request converted OK")
}

// 验证 responsesToChatRequest 对纯字符串 input 的处理与空 input 报错
func TestResponsesToChatRequestStringInput(t *testing.T) {
	chat, err := responsesToChatRequest(mustOrdered(t, `{"model":"x","input":"hello"}`), "x")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := chat.Get("messages")
	messages, _ := raw.([]any)
	first, _ := messages[0].(*jsonObject)
	content, _ := first.Get("content")
	if len(messages) != 1 || content != "hello" {
		t.Fatalf("bad messages: %#v", messages)
	}
	if _, err := responsesToChatRequest(mustOrdered(t, `{"model":"x"}`), "x"); err == nil {
		t.Fatal("empty input should error")
	}
}

// 验证 chat.completion -> Responses 非流式响应对象转换
func TestChatCompletionToResponses(t *testing.T) {
	chat := map[string]any{
		"id": "cmpl-123", "created": float64(1700000000), "model": "hy3-preview",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{
			"role": "assistant", "content": "hello", "reasoning_content": "thinking",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"BJ"}`},
			}},
		}}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15),
			"prompt_tokens_details":     map[string]any{"cached_tokens": float64(2)},
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(3)},
		},
	}
	raw, _ := json.Marshal(chat)
	out, err := chatCompletionToResponses(raw, "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["object"] != "response" || result["status"] != "completed" {
		t.Fatalf("bad envelope: %#v", result)
	}
	output := result["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected 3 output items (reasoning+message+function_call), got %d", len(output))
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output[0] = %#v", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output[1] = %#v", output[1])
	}
	fc := output[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call item bad: %#v", fc)
	}
	usage := result["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) && usage["input_tokens"] != int64(10) {
		t.Fatalf("usage input = %#v", usage["input_tokens"])
	}
	if usage["total_tokens"] != float64(15) && usage["total_tokens"] != int64(15) {
		t.Fatalf("usage total = %#v", usage["total_tokens"])
	}
	t.Logf("responses object output items: %d", len(output))
}

// -----------------------------------------------------------------------------
// 上游请求头指纹对齐回归
//
// 背景：网关此前发送的 35 个应用层头中仅 7 项与真实客户端一致（自造的 X-Client-ID /
// X-Client-Version / Origin / Referer，以及缺失的会话、Agent、链路传播与 SDK 指纹头），
// 构成可静态识别的机器特征。以下测试锁定与官方客户端基线的逐项一致性。
// -----------------------------------------------------------------------------

// realClientChatHeaders 是官方客户端（WorkBuddyAI desktop 5.5.2 + bundled CLI 2.137.1）
// 发往 /v2/chat/completions 的真实请求头基线，逐字取自抓包。
//
// 基线含两类不参与应用层对齐的头：
//   - 传输层：Host / Content-Length / Connection（由 net/http 自行管理）
//   - 客户端本地网关安全头：x-codebuddy-request（源码 GatewayLocalServer 模块的
//     withSecurityHeader 注入，仅存在于「客户端 → 本地网关」这一跳）
var realClientChatHeaders = [][2]string{
	{"Accept", "application/json"},
	{"Content-Type", "application/json"},
	{"x-requested-with", "XMLHttpRequest"},
	{"x-stainless-arch", "x64"},
	{"x-stainless-lang", "js"},
	{"x-stainless-os", "Windows"},
	{"x-stainless-package-version", "6.25.0"},
	{"x-stainless-retry-count", "0"},
	{"x-stainless-runtime", "node"},
	{"x-stainless-runtime-version", "v22.22.2"},
	{"X-Conversation-ID", "04bad56e-08d5-4647-9c3c-28e12897c1af"},
	{"X-Conversation-Request-ID", "a2a2a25c3094ee6e6203d9e014ba6e8c"},
	{"X-Agent-Intent", "craft"},
	{"X-Agent-Purpose", "conversation"},
	{"X-IDE-Type", "WorkBuddy"},
	{"X-IDE-Name", "WorkBuddy"},
	{"X-IDE-Version", "5.5.2"},
	{"X-Private-Data", "true"},
	{"X-Request-ID", "8e48c9ed463d48d08dce1185ed92b200"},
	{"X-Conversation-Message-ID", "8e48c9ed463d48d08dce1185ed92b200"},
	{"X-Root-Request-ID", "a2a2a25c3094ee6e6203d9e014ba6e8c"},
	{"X-Agent-Type", "main"},
	{"traceparent", "00-a2a2a25c3094ee6e6203d9e014ba6e8c-61cc1c019243bd0c-01"},
	{"b3", "a2a2a25c3094ee6e6203d9e014ba6e8c-61cc1c019243bd0c-1-298ea3b5a5796f21"},
	{"X-B3-TraceId", "a2a2a25c3094ee6e6203d9e014ba6e8c"},
	{"X-B3-ParentSpanId", "298ea3b5a5796f21"},
	{"X-B3-SpanId", "61cc1c019243bd0c"},
	{"X-B3-Sampled", "1"},
	{"X-Trace-ID", "a2a2a25c3094ee6e6203d9e014ba6e8c"},
	// 基线中为真实 Bearer JWT，此处保留前缀（仅校验存在性与方案，不校验具体令牌）
	{"Authorization", "Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJXVzhVVkZuS0l"},
	{"X-User-Id", "8efc9f5d-4289-445e-b503-c8a49eeb52c5"},
	{"X-Domain", "www.workbuddy.ai"},
	{"X-Product", "SaaS"},
	{"User-Agent", "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1"},
}

// nonApplicationHeaders 是不参与应用层对齐的头。
var nonApplicationHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Connection": true,
	"X-Codebuddy-Request": true,
}

// 上游请求头指纹必须与真实客户端逐项一致：不多、不少、关键字段形态正确。
func TestUpstreamFingerprintMatchesRealClient(t *testing.T) {
	// 逐字写入（不经 Header.Set 规范化），否则基线自身会被改写、大小写偏差无法被发现。
	real := http.Header{}
	for _, kv := range realClientChatHeaders {
		real[kv[0]] = []string{kv[1]}
	}

	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", RefreshToken: "ref", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "8efc9f5d-4289-445e-b503-c8a49eeb52c5"},
		Edition: "intl",
	}
	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, sa, &profileINTL, nil)
	h := req.Header

	// 1. 基线中的每个头都必须存在（凭据类值由账号池生成，故只校验存在性）
	for name := range real {
		if nonApplicationHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		if getHeaderExact(h, name) == "" {
			t.Errorf("MISSING header present in real client: %s = %q", name, getHeaderExact(real, name))
		}
	}
	// 2. 不得出现真实客户端没有的头（多余头同样是可识别特征）
	for name := range h {
		if nonApplicationHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		if getHeaderExact(real, name) == "" {
			t.Errorf("EXTRA header absent in real client: %s = %q", name, getHeaderExact(h, name))
		}
	}

	// 2b. 键名大小写必须逐字一致。
	//
	// HTTP/1.1 按 map 中存储的键名逐字发送（Header.Write 不做规范化），故 `X-Request-Id`
	// 与客户端的 `X-Request-ID` 是可静态区分的机器特征。http.Header.Set 会经
	// textproto.CanonicalMIMEHeaderKey 改写键名，因此这里必须绕过 Get/Set 直接比对 map 键。
	for _, kv := range realClientChatHeaders {
		name := kv[0]
		if nonApplicationHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		if _, ok := h[name]; !ok {
			// 大小写不同 → 找出实际使用的键名，便于定位
			actual := ""
			for got := range h {
				if strings.EqualFold(got, name) {
					actual = got
					break
				}
			}
			if actual == "" {
				continue // 缺失已由第 1 项报告
			}
			t.Errorf("HEADER CASING mismatch: real client sends %q, gateway sends %q", name, actual)
		}
	}

	// 3. 值必须逐字一致的头
	for _, name := range []string{
		"User-Agent", "Accept", "X-IDE-Type", "X-IDE-Name", "X-IDE-Version",
		"X-Agent-Intent", "X-Agent-Purpose", "X-Agent-Type", "X-Private-Data", "X-Product",
	} {
		if got, want := getHeaderExact(h, name), getHeaderExact(real, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// 4. 链路 ID 形态：32 位小写 hex，且 requestId 与 traceId 解耦
	for _, n := range []string{"X-Request-ID", "X-Trace-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-Conversation-Message-ID"} {
		v := getHeaderExact(h, n)
		if len(v) != 32 || strings.Trim(v, "0123456789abcdef") != "" {
			t.Errorf("%s = %q, want 32 lowercase hex chars", n, v)
		}
	}
	if getHeaderExact(h, "X-Request-ID") == getHeaderExact(h, "X-Trace-ID") {
		t.Error("X-Request-ID must be decoupled from X-Trace-ID")
	}
	if getHeaderExact(h, "X-Conversation-Message-ID") != getHeaderExact(h, "X-Request-ID") {
		t.Error("X-Conversation-Message-ID must equal X-Request-ID (observed client behavior)")
	}
	if cid := getHeaderExact(h, "X-Conversation-ID"); len(cid) != 36 || strings.Count(cid, "-") != 4 {
		t.Errorf("X-Conversation-ID = %q, want dashed UUID", cid)
	}

	// 5. 链路传播头必须互相自洽
	traceID := getHeaderExact(h, "X-Trace-ID")
	if tp := getHeaderExact(h, "traceparent"); !strings.HasPrefix(tp, "00-"+traceID+"-") || !strings.HasSuffix(tp, "-01") {
		t.Errorf("traceparent %q not linked to X-Trace-ID %q", tp, traceID)
	}
	if b3 := getHeaderExact(h, "b3"); !strings.HasPrefix(b3, traceID+"-"+getHeaderExact(h, "X-B3-SpanId")+"-1-") {
		t.Errorf("b3 %q not linked to trace/span ids (%s/%s)", b3, traceID, getHeaderExact(h, "X-B3-SpanId"))
	}
	if getHeaderExact(h, "X-B3-TraceId") != traceID {
		t.Errorf("X-B3-TraceId %q != X-Trace-ID %q", getHeaderExact(h, "X-B3-TraceId"), traceID)
	}
}

// 已鉴权请求不得携带任何 X-No-*（实测未鉴权时才成组出现）。
func TestAuthedRequestHasNoNoHeaders(t *testing.T) {
	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "u"},
	}
	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, sa, &profileINTL, nil)
	for name := range req.Header {
		if strings.HasPrefix(name, "X-No-") {
			t.Errorf("authed request must not carry %s = %q", name, getHeaderExact(req.Header, name))
		}
	}
	if got := getHeaderExact(req.Header, "Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
}

// 未鉴权请求（登录轮询链路）应携带 X-No-Authorization: true 并回退 X-Domain。
func TestUnauthedRequestDeclaresNoAuth(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, nil, &profileINTL, nil)
	if got := getHeaderExact(req.Header, "X-No-Authorization"); got != "true" {
		t.Errorf("X-No-Authorization = %q, want \"true\"", got)
	}
	if got := getHeaderExact(req.Header, "X-No-User-Id"); got != "true" {
		t.Errorf("X-No-User-Id = %q, want \"true\"", got)
	}
	if got := getHeaderExact(req.Header, "X-Domain"); got != clientDomain {
		t.Errorf("X-Domain = %q, want fallback %q", got, clientDomain)
	}
}

// x-codebuddy-request 是客户端本地网关安全头，网关不得自行合成（会构成假特征），
// 但客户端真带上时必须原样透传；同时客户端身份/会话头应优先于网关合成值。
func TestClientPassthroughAndSynthesisBoundary(t *testing.T) {
	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "u"},
	}

	// 未携带时不合成
	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, sa, &profileINTL, nil)
	if v := getHeaderExact(req.Header, "x-codebuddy-request"); v != "" {
		t.Errorf("gateway must not synthesize x-codebuddy-request, got %q", v)
	}

	// 携带时透传，且优先于合成值
	in := http.Header{}
	in.Set("x-codebuddy-request", "1")
	in.Set("X-Conversation-ID", "11111111-2222-3333-4444-555555555555")
	in.Set("X-Agent-Intent", "ask")
	in.Set("User-Agent", "custom/1.0")
	req2, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req2, sa, &profileINTL, in)
	if v := getHeaderExact(req2.Header, "x-codebuddy-request"); v != "1" {
		t.Errorf("client-provided x-codebuddy-request must pass through, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "X-Conversation-ID"); v != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("client-provided X-Conversation-ID must win, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "X-Agent-Intent"); v != "ask" {
		t.Errorf("client-provided X-Agent-Intent must win, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "User-Agent"); v != "custom/1.0" {
		t.Errorf("client-provided User-Agent must win, got %q", v)
	}
}

// 鉴权类头绝不能被下游客户端覆盖，否则会串号或泄权。
func TestClientCannotOverrideCredentialHeaders(t *testing.T) {
	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", RefreshToken: "ref", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "u", EnterpriseID: "ent"},
	}
	evil := http.Header{}
	evil.Set("Authorization", "Bearer attacker")
	evil.Set("X-User-Id", "attacker")
	evil.Set("X-Domain", "evil.example")
	evil.Set("X-Product", "evil")
	evil.Set("X-Enterprise-Id", "evil")
	evil.Set("X-Refresh-Token", "evil")

	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, sa, &profileINTL, evil)

	for name, want := range map[string]string{
		"Authorization":   "Bearer tok",
		"X-User-Id":       "u",
		"X-Domain":        "www.workbuddy.ai",
		"X-Product":       "SaaS",
		"X-Enterprise-Id": "ent",
	} {
		if got := getHeaderExact(req.Header, name); got != want {
			t.Errorf("%s = %q, want %q (must come from account pool)", name, got, want)
		}
	}
	// chat 请求不携带 X-Refresh-Token（仅 auth/token/refresh 与 account/switch 链路使用）
	if got := getHeaderExact(req.Header, "X-Refresh-Token"); got != "" {
		t.Errorf("chat request must not carry X-Refresh-Token, got %q", got)
	}
}

// /v1/models 必须返回官方客户端实际可用的模型清单（此前硬编码的 10 个模型与远端零交集）。
func TestModelsListMatchesUpstreamCatalog(t *testing.T) {
	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Data) != len(upstreamModelIDs) {
		t.Fatalf("got %d models, want %d", len(resp.Data), len(upstreamModelIDs))
	}
	ids := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	for _, want := range upstreamModelIDs {
		if !ids[want] {
			t.Errorf("missing model %q", want)
		}
	}
	// 旧硬编码模型不得回归
	for _, stale := range []string{"hy4-preview", "hy3-preview", "minimax-m3-pay", "deepseek-v4-pro"} {
		if ids[stale] {
			t.Errorf("stale hardcoded model %q must not be advertised", stale)
		}
	}
}

// 端到端冒烟：驱动真实 handleChatCompletions → upstreamChat → 生产 transport →
// 本地假上游，抓取实际上线请求头。覆盖单测无法触及的传输层（DisableCompression）
// 与透传接线。
func TestSmokeEndToEndUpstreamHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	gotPath := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		got <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	origBase := profileINTL.Base
	profileINTL.Base = up.URL
	defer func() { profileINTL.Base = origBase }()

	// 使用生产 transport（含 DisableCompression 等指纹相关设置），而非测试自建 client
	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	// 走真实凭据加载路径
	dir := t.TempDir()
	credPath := filepath.Join(dir, "workbuddy-intl.json")
	cred := map[string]any{
		"auth": map[string]any{
			"accessToken":  "smoke-token",
			"refreshToken": "smoke-refresh",
			"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
			"domain":       "www.workbuddy.ai",
		},
		"account": map[string]any{"uid": "8efc9f5d-4289-445e-b503-c8a49eeb52c5", "nickname": "smoke"},
		"edition": "intl",
	}
	raw, _ := json.Marshal(cred)
	if err := os.WriteFile(credPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	prevAccounts := accounts
	accounts = nil
	sa, err := loadAccountFile(credPath)
	if err != nil {
		accounts = prevAccounts
		accountMu.Unlock()
		t.Fatalf("loadAccountFile: %v", err)
	}
	accounts = append(accounts, &Account{Path: credPath, Auth: sa, Edition: "intl"})
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = prevAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"default-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Intent", "ask") // 模拟客户端自带身份头
	rec := httptest.NewRecorder()

	handleChatCompletions(rec, req)

	var h http.Header
	select {
	case path := <-gotPath:
		if path != "/v2/chat/completions" {
			t.Errorf("upstream path = %q, want /v2/chat/completions", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received request")
	}
	select {
	case h = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no headers captured")
	}

	// 客户端身份头透传生效（未被网关合成值覆盖）
	if v := getHeaderExact(h, "X-Agent-Intent"); v != "ask" {
		t.Errorf("client passthrough X-Agent-Intent = %q, want ask", v)
	}
	// 凭据来自账号池
	if v := getHeaderExact(h, "Authorization"); v != "Bearer smoke-token" {
		t.Errorf("Authorization = %q", v)
	}
	if v := getHeaderExact(h, "X-User-Id"); v != "8efc9f5d-4289-445e-b503-c8a49eeb52c5" {
		t.Errorf("X-User-Id = %q", v)
	}
	if v := getHeaderExact(h, "X-Domain"); v != "www.workbuddy.ai" {
		t.Errorf("X-Domain = %q", v)
	}
	// 合成指纹头存在
	if v := getHeaderExact(h, "User-Agent"); v != "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1" {
		t.Errorf("User-Agent = %q", v)
	}
	if v := getHeaderExact(h, "x-stainless-runtime"); v != "node" {
		t.Errorf("X-Stainless-Runtime = %q", v)
	}
	if v := getHeaderExact(h, "x-codebuddy-request"); v != "" {
		t.Errorf("must not synthesize x-codebuddy-request, got %q", v)
	}
	// 浏览器语义头不得出现
	if v := h.Get("Origin"); v != "" {
		t.Errorf("Origin must not be sent, got %q", v)
	}
	if v := h.Get("Referer"); v != "" {
		t.Errorf("Referer must not be sent, got %q", v)
	}
	// 真实客户端不发 Accept-Encoding；Go transport 默认会补 gzip，故须禁用自动压缩
	if v := h.Get("Accept-Encoding"); v != "" {
		t.Errorf("Accept-Encoding must not be sent, got %q", v)
	}
	for name := range h {
		if strings.HasPrefix(name, "X-No-") {
			t.Errorf("authed request must not carry %s", name)
		}
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("client body = %q", rec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 上游请求「线级」指纹回归
//
// 前两组测试断言的是 http.Header 内容；本组测试断言的是真正写到 TCP 上的字节。
// 两者不等价：HTTP/1.1 的 Header.Write 按 map 中存储的键名逐字输出，不重新规范化，
// 因此 `X-Request-Id`（Header.Set 的产物）与客户端的 `X-Request-ID` 在线上可被静态区分。
// -----------------------------------------------------------------------------

// 捕获上游真实收到的原始请求字节（请求行 + Header + 空行 + body）。
//
// 用裸 TCP 监听而非 httptest.Server：后者会用 net/http 的解析器重新规范化键名，
// 无法反映真正写到线上的字节。
func captureUpstreamWire(t *testing.T, body string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	rawCh := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// 读到 Header 结束标记后再按 Content-Length 读满 body
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
					head := string(buf[:idx])
					cl := 0
					for _, line := range strings.Split(head, "\r\n") {
						if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
							cl, _ = strconv.Atoi(strings.TrimSpace(v))
						}
					}
					if len(buf) >= idx+4+cl {
						break
					}
				}
			}
			if err != nil {
				break
			}
		}
		rawCh <- string(buf)
		resp := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: [DONE]\n\n"
		_, _ = conn.Write([]byte(resp))
	}()

	origBase := profileINTL.Base
	profileINTL.Base = "http://" + ln.Addr().String()
	defer func() { profileINTL.Base = origBase }()

	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	dir := t.TempDir()
	credPath := filepath.Join(dir, "workbuddy-intl.json")
	raw, _ := json.Marshal(map[string]any{
		"auth": map[string]any{
			"accessToken": "wire-token", "refreshToken": "r",
			"expiresAt": time.Now().Add(24 * time.Hour).Unix(), "domain": "www.workbuddy.ai",
		},
		"account": map[string]any{"uid": "u1"},
		"edition": "intl",
	})
	if err := os.WriteFile(credPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	prev := accounts
	accounts = nil
	sa, err := loadAccountFile(credPath)
	if err != nil {
		accounts = prev
		accountMu.Unlock()
		t.Fatalf("loadAccountFile: %v", err)
	}
	accounts = append(accounts, &Account{Path: credPath, Auth: sa, Edition: "intl"})
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = prev
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	select {
	case raw := <-rawCh:
		return raw
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received request")
		return ""
	}
}

// wireBody 从原始请求字节中取出 body 部分。
func wireBody(t *testing.T, raw string) string {
	t.Helper()
	idx := strings.Index(raw, "\r\n\r\n")
	if idx < 0 {
		t.Fatalf("malformed wire request: %q", raw)
	}
	return raw[idx+4:]
}

// 上游线上收到的 Header 键名大小写必须与官方客户端逐字一致。
//
// 这是 HTTP/1.1 特有的可识别特征：Go 的 http.Header.Set 会把 `X-Request-ID` 规范化成
// `X-Request-Id`、`X-IDE-Type` 成 `X-Ide-Type`、`X-B3-TraceId` 成 `X-B3-Traceid`，
// 而客户端（Node/undici）按原样发送。此类偏差仅比对线上字节可见。
func TestUpstreamWireHeaderCasingMatchesClient(t *testing.T) {
	raw := captureUpstreamWire(t, `{"model":"default-model","messages":[{"role":"user","content":"hi"}]}`)

	// 客户端实测逐字大小写（抓包 capture2.jsonl）。
	// 注意：Go 服务端会把入站头规范化，故「客户端 → 网关」这一跳的键名无法用于断言，
	// 这里断言的是网关 → 上游这一跳。
	wantExact := []string{
		"X-Request-ID",
		"X-Trace-ID",
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
		"X-Root-Request-ID",
		"X-B3-TraceId",
		"X-B3-ParentSpanId",
		"X-B3-SpanId",
		"X-B3-Sampled",
		"X-Agent-Type",
		"X-Agent-Intent",
		"X-Agent-Purpose",
		"X-IDE-Type",
		"X-IDE-Name",
		"X-IDE-Version",
		"X-Private-Data",
		"X-User-Id",
		"X-Domain",
		"X-Product",
		"x-requested-with",
		"x-stainless-arch",
		"x-stainless-lang",
		"x-stainless-os",
		"x-stainless-package-version",
		"x-stainless-retry-count",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"traceparent",
		"b3",
	}
	for _, name := range wantExact {
		if !strings.Contains(raw, "\r\n"+name+": ") {
			t.Errorf("wire is missing header with exact casing %q", name)
		}
	}

	// 规范化形态绝不得出现在线上（否则即被 Header.Set 改写）
	for _, bad := range []string{
		"X-Request-Id", "X-Trace-Id", "X-Ide-Type", "X-Ide-Name", "X-Ide-Version",
		"X-B3-Traceid", "X-B3-Parentspanid", "X-B3-Spanid", "X-Conversation-Id",
		"X-Requested-With", "X-Stainless-Runtime", "X-Stainless-Arch",
	} {
		if strings.Contains(raw, "\r\n"+bad+": ") {
			t.Errorf("wire carries canonicalized header %q; client sends a different casing", bad)
		}
	}
	t.Logf("wire request line: %s", strings.SplitN(raw, "\r\n", 2)[0])
}

// 上游收到的请求体必须复刻客户端 JSON.stringify 的输出口径：
// 键序保持（model 在首位）且 < > & 不被转义。
//
// 网关此前用 map[string]any 中转再 json.Marshal，导致：键序变字典序（messages 跑到
// model 之前）、`<` 被转义成 `\u003c`。两者都是无需解析语义、比对字节即可判定的机器特征。
func TestUpstreamWireBodyMatchesClientEncoding(t *testing.T) {
	// 含 HTML 敏感字符的 system prompt，贴近客户端真实 harness 正文
	body := `{"model":"deepseek-v3-2-volc","messages":[{"role":"system","content":"a <user_query> b & c > d"},{"role":"user","content":"hi"}]}`
	raw := captureUpstreamWire(t, body)
	got := wireBody(t, raw)

	// 1. model 必须在首位（客户端 JSON.stringify 的插入顺序）
	if !strings.HasPrefix(got, `{"model":"deepseek-v3-2-volc"`) {
		t.Errorf("body must start with model key (client key order), got: %s", truncateForLog(got))
	}
	// 2. messages 必须排在 model 之后
	mi, si := strings.Index(got, `"messages"`), strings.Index(got, `"model"`)
	if mi < si {
		t.Errorf("messages key precedes model; client emits model first: %s", truncateForLog(got))
	}
	// 3. HTML 敏感字符原样输出，不得转义
	for _, bad := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, bad) {
			t.Errorf("body contains escaped %s; JSON.stringify emits the raw character: %s", bad, truncateForLog(got))
		}
	}
	if !strings.Contains(got, "a <user_query> b & c > d") {
		t.Errorf("body lost raw HTML-sensitive characters: %s", truncateForLog(got))
	}
	// 4. 网关注入的 stream=true 也必须存在（上游强制流式）
	if !strings.Contains(got, `"stream":true`) {
		t.Errorf("body must force stream=true: %s", truncateForLog(got))
	}
	// 5. 不得有尾随换行（json.Encoder.Encode 会追加）
	if strings.HasSuffix(got, "\n") {
		t.Errorf("body must not carry a trailing newline")
	}
	t.Logf("wire body: %s", truncateForLog(got))
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
