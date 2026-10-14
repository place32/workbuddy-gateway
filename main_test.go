package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

// TestMain 将整个测试进程切到临时工作目录，确保任何写状态/缓存/日志的测试
// 都不会污染仓库目录（这些文件按设计写在进程 cwd）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "workbuddy-gateway-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// chdirTemp 将测试工作目录切到临时目录，避免测试写入仓库内的状态/缓存文件。
func chdirTemp(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// 不使用 t.TempDir()：它的清理时机与「切回原工作目录」的相对顺序无法保证，
	// 而 Windows 不允许删除仍是当前工作目录的目录，顺序错了会留下无效 cwd，
	// 进而让后续测试的 os.Getwd/os.Chdir 全部失效。
	dir, err := os.MkdirTemp("", "workbuddy-test-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 必须在同一个清理里先恢复工作目录、再删除，顺序不能颠倒。
		_ = os.Chdir(old)
		_ = os.RemoveAll(dir)
	})
}

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
	if p := profileForEdition("cn"); p.Base != "https://copilot.tencent.com" || p.Platform != "workbuddy" {
		t.Errorf("cn profile base/platform unexpected: %+v", p)
	}
	// 两站都跑同一个桌面宿主（`@genie/workbuddy-desktop`），身份由宿主注入的
	// CLIENT_INFO_* 决定：X-IDE-* 与 UA 首段是宿主产品标识 WorkBuddy，宿主版本取
	// app.getVersion()，UA 中段/尾段取 bundled product.json 的 productName 与 bundled
	// CLI 包版本。国内站 5.5.3 + CLI 2.137.1（productName 也是 WorkBuddy），
	// 国际站 5.7.6 + CLI 2.156.0（productName 是 WorkBuddy AI）。
	if p := &profileCN; p.PlatformVersion != "5.5.3" || p.CliVersion != "2.137.1" || p.PlatformName != "WorkBuddy" || p.AppName != "WorkBuddy" || p.ProductName != "WorkBuddy" || !p.GzipBody {
		t.Errorf("cn client version baseline unexpected: %+v", p)
	}
	if ua := buildUserAgent(&profileCN); ua != "WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1" {
		t.Errorf("cn user agent = %q", ua)
	}
	if p := &profileINTL; p.PlatformVersion != "5.7.6" || p.CliVersion != "2.156.0" || p.PlatformName != "WorkBuddy" || p.ProductName != "WorkBuddy AI" || p.AppName != "WorkBuddy" || !p.GzipBody {
		t.Errorf("intl client version baseline unexpected: %+v", p)
	}
	if ua := buildUserAgent(&profileINTL); ua != "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0" {
		t.Errorf("intl user agent = %q", ua)
	}
	// 压缩协商按站点分叉：国际站 CLI 2.156.0 发 Accept-Encoding，国内站 CLI 2.137.1 不发。
	// 用同一个值覆盖两站，必然使其中一站多出（或少掉）一个客户端实测存在的头。
	if profileCN.AcceptEncoding != "" {
		t.Errorf("cn profile must not send Accept-Encoding (client 2.137.1 does not), got %q", profileCN.AcceptEncoding)
	}
	if profileINTL.AcceptEncoding != "gzip, compress, deflate, br" {
		t.Errorf("intl AcceptEncoding = %q", profileINTL.AcceptEncoding)
	}
}

// x-stainless-runtime-version 是「运行 bundled OpenAI SDK 的 Node 进程」版本，随宿主客户端
// 自带的 Electron 走。两站当前安装的 Electron 都是 37.10.3 → v22.21.1（各自
// ELECTRON_RUN_AS_NODE=1 实测），但该值仍按 Profile 取，不写死成常数：宿主升级会同时
// 改变两站的 Node 版本，而下一次实测可能只覆盖其中一站。
func TestStainlessNodeVersionFollowsProfile(t *testing.T) {
	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "u"},
	}
	for _, c := range []struct {
		prof *upstreamProfile
		want string
	}{
		{&profileCN, "v22.21.1"},
		{&profileINTL, "v22.21.1"},
	} {
		req, err := http.NewRequest(http.MethodPost, c.prof.chatURL(), nil)
		if err != nil {
			t.Fatal(err)
		}
		backendHeaders(req, sa, c.prof, nil, sessionScope{})
		if got := getHeaderExact(req.Header, "x-stainless-runtime-version"); got != c.want {
			t.Errorf("%s 站 x-stainless-runtime-version = %q, want %q", c.prof.Key, got, c.want)
		}
	}
}

// 验证各站点上游 URL 构建与旧版常量完全一致（国内站回归）+ 国际站正确
func TestUpstreamProfileURLs(t *testing.T) {
	cn := profileForEdition("cn")
	if got := cn.authStateURL(); got != "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy" {
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
	if got := cn.dailyCheckinURL(); got != "https://www.codebuddy.cn/v2/billing/meter/daily-checkin" {
		t.Errorf("cn dailyCheckinURL = %s", got)
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

func TestNextAccountForModelPrefersKnownFreeExhausted(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "empty-free.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
			"free-model": {CostClass: modelCostFree},
		}},
		{Path: "available.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 10},
	}
	rrIndex = 0
	accountMu.Unlock()

	acc, kind, err := nextAccountForModel("free-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "empty-free.json" || kind != selectionFreeExhausted {
		t.Fatalf("expected free exhausted account, got %s kind=%s", acc.Path, kind)
	}
}

func TestNextAccountForModelAllowsOneUnknownProbe(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true},
	}
	rrIndex = 0
	accountMu.Unlock()

	acc, kind, err := nextAccountForModel("unknown-model", nil)
	if err != nil || acc.Path != "a.json" || kind != selectionProbeExhausted {
		t.Fatalf("expected one controlled probe, acc=%v kind=%s err=%v", acc, kind, err)
	}
	if _, _, err = nextAccountForModel("unknown-model", nil); err == nil || !strings.Contains(err.Error(), "等待探测=1") {
		t.Fatalf("expected probe cooldown error, got %v", err)
	}
}

func TestNextAccountForModelSkipsPaidAndQuotaBlocked(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "paid.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
			"paid-model": {CostClass: modelCostPaid, QuotaBlocked: true},
		}},
	}
	rrIndex = 0
	accountMu.Unlock()
	if _, _, err := nextAccountForModel("paid-model", nil); err == nil || !strings.Contains(err.Error(), "额度阻断=1") {
		t.Fatalf("expected model quota block, got %v", err)
	}
}

func TestModelCooldownOnlyBlocksTriggeringModel(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{}, ModelStates: map[string]*modelRuntimeState{
		"limited-model": {CostClass: modelCostFree, CooldownUntil: time.Now().Add(time.Hour)},
	}}}
	rrIndex = 0
	accountMu.Unlock()
	if _, _, err := nextAccountForModel("limited-model", nil); err == nil || !strings.Contains(err.Error(), "模型冷却=1") {
		t.Fatalf("expected model cooldown, got %v", err)
	}
	acc, _, err := nextAccountForModel("other-model", nil)
	if err != nil || acc.Path != "a.json" {
		t.Fatalf("other model should remain usable, acc=%v err=%v", acc, err)
	}
}

func TestMarkModelQuotaBlockedDoesNotOverwriteAccountBalance(t *testing.T) {
	oldDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	acc := &Account{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 123}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	markModelQuotaBlocked(acc, "paid-model", "14018")
	accountMu.Lock()
	state := acc.ModelStates["paid-model"]
	remaining := acc.QuotaRemaining
	exhausted := acc.QuotaExhausted
	accountMu.Unlock()
	if state == nil || !state.QuotaBlocked {
		t.Fatalf("model block missing: %+v", state)
	}
	if remaining != 123 || exhausted {
		t.Fatalf("model 14018 must not overwrite account balance: remaining=%v exhausted=%v", remaining, exhausted)
	}
}

// 14018 只表示账号额度耗尽，与模型收费属性无关。
// 已知免费模型遇到 14018 时，必须保持 free 分类，不能被错误改写成 paid。
func TestQuotaBlockedMustNotRewriteFreeModelAsPaid(t *testing.T) {
	chdirTemp(t)
	acc := &Account{Path: "free.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"deepseek-v4.1-flash": {CostClass: modelCostFree},
		}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	markModelQuotaBlocked(acc, "deepseek-v4.1-flash", "14018 Credits exhausted")

	accountMu.Lock()
	state := acc.ModelStates["deepseek-v4.1-flash"]
	accountMu.Unlock()
	if state.CostClass != modelCostFree {
		t.Fatalf("14018 must not overwrite a free model's cost class, got %q", state.CostClass)
	}
	if !state.QuotaBlocked {
		t.Fatal("14018 should still block this account for the model until quota recovers")
	}
	// 未知模型遇到 14018 也应保持 unknown，不得被当成 paid。
	acc2 := &Account{Path: "unknown.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"mystery": {CostClass: modelCostUnknown}}}
	markModelQuotaBlocked(acc2, "mystery", "14018")
	accountMu.Lock()
	got := acc2.ModelStates["mystery"].CostClass
	accountMu.Unlock()
	if got != modelCostUnknown {
		t.Fatalf("14018 must not turn an unknown model into paid, got %q", got)
	}
}

// 旧版本快照里可能残留「14018 被误写成 paid」的脏数据；恢复时必须降回 unknown。
func TestSanitizeRestoredModelStateFixesLegacyPaidFromQuotaBlock(t *testing.T) {
	legacy := &modelRuntimeState{
		CostClass:    modelCostPaid,
		QuotaBlocked: true,
		LastReason:   `{"error":{"data":{"code":14018,"msg":"Credits exhausted."}}}`,
	}
	sanitizeRestoredModelState(legacy)
	if legacy.CostClass != modelCostUnknown {
		t.Fatalf("legacy 14018-derived paid should be reset to unknown, got %q", legacy.CostClass)
	}
	if !legacy.QuotaBlocked {
		t.Fatal("quota block should be preserved")
	}

	// 真正由 credit 证据学到的收费模型（非额度耗尽导致）必须保持 paid。
	genuinePaid := &modelRuntimeState{CostClass: modelCostPaid}
	sanitizeRestoredModelState(genuinePaid)
	if genuinePaid.CostClass != modelCostPaid {
		t.Fatalf("genuine paid model must stay paid, got %q", genuinePaid.CostClass)
	}

	// 免费模型不受影响。
	free := &modelRuntimeState{CostClass: modelCostFree, QuotaBlocked: true}
	sanitizeRestoredModelState(free)
	if free.CostClass != modelCostFree {
		t.Fatalf("free model must stay free, got %q", free.CostClass)
	}
}

// QuotaBlocked 必须短路调度，避免额度耗尽账号被反复选中探测。
func TestQuotaBlockedShortCircuitsScheduling(t *testing.T) {
	acc := &Account{Path: "exhausted.json", Auth: &StoredAuth{}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"m": {CostClass: modelCostUnknown, QuotaBlocked: true},
		}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	if _, _, err := nextAccountForModel("m", nil); err == nil {
		t.Fatal("quota-blocked account must not be selected for the model")
	}
}

func TestQuotaRecoveryClearsBlocksKeepsModelCooldown(t *testing.T) {
	until := time.Now().Add(time.Hour)
	acc := &Account{QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
		"m": {CostClass: modelCostPaid, QuotaBlocked: true, CooldownUntil: until},
	}}
	accountMu.Lock()
	acc.QuotaExhausted = false
	for _, state := range acc.ModelStates {
		state.QuotaBlocked = false
		state.NextProbeAt = time.Time{}
	}
	state := acc.ModelStates["m"]
	accountMu.Unlock()
	if state.QuotaBlocked || !state.CooldownUntil.Equal(until) {
		t.Fatalf("quota recovery should clear only quota block: %+v", state)
	}
}

func TestIsModelRateLimited(t *testing.T) {
	if !isModelRateLimited(`{"code":6004,"msg":"您也可以切换其他模型继续使用"}`) {
		t.Fatal("6004 should be model-level rate limit")
	}
	if isModelRateLimited(`{"code":14018,"msg":"额度已用尽"}`) {
		t.Fatal("14018 should not be model rate limit")
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

func TestIsAlreadyCheckedIn(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{409, `{"code":10001,"msg":"今天已签到"}`, true},
		{400, `{"code":14001,"msg":"今日已签到"}`, true},
		{409, `{"msg":"Already checked in today"}`, true},
		{500, "connection reset", false},
		{400, `{"code":10002,"msg":"积分不足"}`, false},
	}
	for _, tc := range cases {
		if got := isAlreadyCheckedIn(tc.status, tc.body); got != tc.want {
			t.Errorf("isAlreadyCheckedIn(%d, %q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestNextDailyCheckinUTC8(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	before := time.Date(2026, 9, 15, 8, 59, 0, 0, loc)
	if got := nextDailyCheckin(before); !got.Equal(time.Date(2026, 9, 15, 9, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))) {
		t.Fatalf("before 09:00 got %v", got)
	}
	after := time.Date(2026, 9, 15, 9, 1, 0, 0, loc)
	if got := nextDailyCheckin(after); !got.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))) {
		t.Fatalf("after 09:00 got %v", got)
	}
}

func TestCheckinAccountCNAndSkipIntl(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v2/billing/meter/daily-checkin" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("X-User-Id") != "user-1" {
			t.Errorf("missing checkin auth headers")
		}
		// 签到是控制面接口（客户端 `checkin()` 经 axios `RestOperations`），
		// 不得带模型请求专有头或已移除的自造头 X-Client-Platform。
		for name, want := range map[string]string{
			"X-Requested-With": "XMLHttpRequest",
			"Accept":           "application/json, text/plain, */*",
			"X-Product":        "SaaS",
			"User-Agent":       "WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if r.Header.Get("X-Request-ID") != r.Header.Get("X-Trace-ID") {
			t.Error("控制面 X-Request-ID 必须等于 X-Trace-ID")
		}
		// 凭据未记录 domain 时回退站点主机（客户端为 auth.domain 或 endpoint authority）
		if r.Header.Get("X-Domain") == "" {
			t.Error("签到请求缺少 X-Domain")
		}
		for _, name := range []string{"X-Client-Platform", "X-IDE-Type", "x-stainless-runtime",
			"x-codebuddy-request", "traceparent", "X-Conversation-ID", "X-Refresh-Token"} {
			if v := r.Header.Get(name); v != "" {
				t.Errorf("签到请求不得携带 %s=%q", name, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer server.Close()

	oldOrigin := profileCN.PortalOrigin
	oldClient := cfg.HttpClient
	profileCN.PortalOrigin = server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.PortalOrigin = oldOrigin
		cfg.HttpClient = oldClient
	}()

	cn := &Account{Path: "cn.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "test-access"},
		Account: StoredAccount{UID: "user-1"},
	}}
	result, err := checkinAccount(context.Background(), cn)
	if err != nil || result != "ok" || calls != 1 {
		t.Fatalf("cn checkin result=%q calls=%d err=%v", result, calls, err)
	}

	intl := &Account{Path: "intl.json", Auth: &StoredAuth{
		Edition: "intl",
		Auth:    StoredTokens{AccessToken: "test-access"},
	}}
	result, err = checkinAccount(context.Background(), intl)
	if err != nil || result != "global_skipped" || calls != 1 {
		t.Fatalf("intl should be skipped: result=%q calls=%d err=%v", result, calls, err)
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

// 验证 disableAccount：停止调度并写入失效标记。
// 删除凭据需要「只读校验明确确认不可用」，测试环境没有 HTTP 客户端时应保守保留文件。
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
	// 无法确认凭据失效时必须保留文件：删除不可逆，误删只能重新登录。
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("无法确认凭据失效时应保留凭据文件, stat err=%v", err)
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
		State:          "paid_exhausted",
		TokenExpiresAt: expires.Unix(),
		QuotaTotal:     1100,
		QuotaUsed:      1100,
		QuotaRemaining: 0,
		IsPaidUser:     false,
		QuotaKnown:     true,
		QuotaExhausted: true,
		FreeModels:     1,
		ModelCooldowns: 2,
	}})
	for _, want := range []string{"凭据文件", "workbuddy4.json", "国际站", "付费耗尽", "2027-09-05 01:36:55", "总额度", "已用", "剩余", "套餐", "免费模型", "模型冷却", "1100", "免费"} {
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

func TestUsageCreditAndObserveModelCost(t *testing.T) {
	for _, tc := range []struct {
		usage map[string]any
		want  float64
		ok    bool
	}{
		{map[string]any{"credit": float64(0)}, 0, true},
		{map[string]any{"credit": "1.25"}, 1.25, true},
		{map[string]any{"total_tokens": 1}, 0, false},
		{nil, 0, false},
	} {
		got, ok := usageCredit(tc.usage)
		if got != tc.want || ok != tc.ok {
			t.Errorf("usageCredit(%v)=%v/%v want %v/%v", tc.usage, got, ok, tc.want, tc.ok)
		}
	}

	oldDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	acc := &Account{Path: "empty.json", Auth: &StoredAuth{}, QuotaExhausted: true}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	var logBuf bytes.Buffer
	oldLogWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldLogWriter)
	// 样本过小：credit=0 但 total_tokens 太低，不得判定为免费
	observeModelCredit(acc, "tiny-model", map[string]any{"credit": float64(0), "total_tokens": float64(2)}, 10)
	accountMu.Lock()
	tiny := acc.ModelStates["tiny-model"]
	accountMu.Unlock()
	if tiny != nil && tiny.CostClass == modelCostFree {
		t.Fatalf("tiny sample must not be learned as free: %+v", tiny)
	}

	// 样本充足：credit=0 且 total_tokens 达标，判定为免费
	observeModelCredit(acc, "free-model", map[string]any{"credit": float64(0), "total_tokens": float64(500)}, 1)
	accountMu.Lock()
	state := acc.ModelStates["free-model"]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree || state.QuotaBlocked {
		t.Fatalf("free model not learned: %+v", state)
	}
	if text := logBuf.String(); !strings.Contains(text, "[FreeModel]") || !strings.Contains(text, "付费余额耗尽账号 empty.json") {
		t.Fatalf("missing explicit free model exhausted-account log: %s", text)
	}
	observeModelCredit(acc, "paid-model", map[string]any{"credit": 2.5}, 2)
	accountMu.Lock()
	paid := acc.ModelStates["paid-model"]
	accountMu.Unlock()
	if paid == nil || paid.CostClass != modelCostPaid {
		t.Fatalf("paid model not learned: %+v", paid)
	}
}

func TestKnownFreeModelUsesExhaustedAccountAndLogs(t *testing.T) {
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":500}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.PortalOrigin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "empty-free.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"free-model": {CostClass: modelCostFree}}}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.PortalOrigin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.PortalOrigin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	text := logs.String()
	for _, want := range []string{
		"请求的是已知免费模型 free-model，选择付费余额耗尽账号 empty-free.json 发起请求",
		"请求的是免费模型 free-model，已明确使用付费余额耗尽账号 empty-free.json 完成请求，usage.credit=0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing log %q:\n%s", want, text)
		}
	}
}

func TestUnknownModelQuotaProbeStopsAfterOneExhaustedAccount(t *testing.T) {
	chdirTemp(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`)
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.PortalOrigin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	a := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true}
	b := &Account{Path: "b.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "b", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true}
	accounts, rrIndex = []*Account{a, b}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.PortalOrigin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.PortalOrigin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"unknown-paid","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusServiceUnavailable || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	accountMu.Lock()
	blockedA := a.ModelStates["unknown-paid"] != nil && a.ModelStates["unknown-paid"].QuotaBlocked
	_, touchedB := b.ModelStates["unknown-paid"]
	accountMu.Unlock()
	if !blockedA || touchedB {
		t.Fatalf("expected only first exhausted account probed: blockedA=%v touchedB=%v", blockedA, touchedB)
	}
}

func TestParseQuotaSummary(t *testing.T) {
	data := []byte(`{"Packages":[{"CycleTotalCapacity":"1500","CycleUsedCapacity":"69.98999993","CycleRemainCapacity":"1430.01000007"},{"CycleTotalCapacity":"500","CycleUsedCapacity":"500","CycleRemainCapacity":"0"}],"IsPaidUser":true}`)
	total, used, remaining, paid, plan, err := parseQuotaSummary(data)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2000 || used != 569.98999993 || remaining != 1430.01000007 || !paid {
		t.Fatalf("unexpected quota summary: total=%v used=%v remaining=%v paid=%v", total, used, remaining, paid)
	}
	if plan != planUnknown {
		t.Fatalf("summary alone cannot establish the current plan, got %q", plan)
	}
}

// 摘要标记不再直接当成当前有效权益。
func TestPlanLabelFromSummary(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"历史试用标记", `{"ProTrialStatus":1,"IsPaidUser":false}`, planUnknown},
		{"字符串试用标记", `{"ProTrialStatus":"1","IsPaidUser":false}`, planUnknown},
		{"试用与付费标记同时存在", `{"ProTrialStatus":1,"IsPaidUser":true}`, planUnknown},
		{"付费标记不能决定等级", `{"ProTrialStatus":0,"IsPaidUser":true}`, planUnknown},
		{"空标记也要查有效权益", `{"ProTrialStatus":0,"IsPaidUser":false}`, planUnknown},
		{"字段缺失", `{"Packages":[]}`, planUnknown},
		{"类型异常", `{"ProTrialStatus":{"unexpected":true},"IsPaidUser":false}`, planUnknown},
		{"国内站无试用标记", `{"IsPaidUser":false,"SubscriptionPackageCode":""}`, planUnknown},
		{"未知订阅包", `{"IsPaidUser":false,"SubscriptionPackageCode":"pkg-code-000"}`, planUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var summary quotaSummaryData
			if err := json.Unmarshal([]byte(tc.json), &summary); err != nil {
				t.Fatal(err)
			}
			if got := planLabelFromSummary(summary); got != tc.want {
				t.Fatalf("plan=%q want=%q", got, tc.want)
			}
		})
	}
}

// 套餐列必须展示 Pro试用/pro/免费，且不再出现 1 和 0。
func TestRenderAccountTableShowsPlanLabel(t *testing.T) {
	table := renderAccountTable([]accountSnapshot{
		{Path: "intl-trial.json", Edition: "intl", Nickname: "trial@example.com", State: "active", QuotaTotal: 500, QuotaRemaining: 500, QuotaKnown: true, PlanLabel: "Pro试用"},
		{Path: "cn-paid.json", Edition: "cn", Nickname: "paid@example.com", State: "active", QuotaTotal: 2200, QuotaRemaining: 400, QuotaKnown: true, PlanLabel: "pro"},
		{Path: "cn-free.json", Edition: "cn", Nickname: "free@example.com", State: "active", QuotaTotal: 2200, QuotaRemaining: 400, QuotaKnown: true, PlanLabel: "免费"},
	})
	for _, want := range []string{"套餐", "Pro试用", "pro", "免费"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "付费用户") {
		t.Fatalf("table should no longer contain 付费用户 column:\n%s", table)
	}
}

func TestLockAccountWithContextTimeout(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if lockAccountWithContext(ctx, &mu) {
		t.Fatal("lock should time out while mutex is held")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("unexpected timeout duration: %v", elapsed)
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
		{Path: "workbuddy1.json", Nickname: "user-a", Edition: "cn", State: "quota_exhausted", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 2000},
		{Path: "workbuddy2.json", Nickname: "user-b", Edition: "cn", State: "active", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 69.98999993, QuotaRemaining: 1930.01000007},
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

// 提取 messages 各条 role，便于断言。保序对象与 map 都支持。
func rolesOf(obj any) []string {
	messages := messagesOf(obj)
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
			ensureLeadingSystemMessageOrdered(obj)
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
	ensureLeadingSystemMessageOrdered(obj)
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

// 验证思考等级归一化：OpenAI 兼容写法（顶层 / 嵌套）落到上游扁平字段，
// 关闭语义与大小写归一，且未显式请求时绝不注入。
//
// 上游实测：取值大小写/空白敏感（HIGH、" high " 均 400 code 11150）；
// none 语义等价于关闭；auto/default 为客户端习惯值但上游 400，按缺省处理。
func TestApplyThinkingRulesNormalizesEffort(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 期望上游收到的 reasoning_effort；"" 表示该字段必须不存在
	}{
		{"未请求时不注入", `{"model":"m","messages":[]}`, ""},
		{"null 按缺省处理", `{"model":"m","reasoning_effort":null}`, ""},
		{"非字符串按缺省处理", `{"model":"m","reasoning_effort":3}`, ""},
		{"空串关闭", `{"model":"m","reasoning_effort":""}`, ""},
		{"none 关闭", `{"model":"m","reasoning_effort":"none"}`, ""},
		{"off 关闭", `{"model":"m","reasoning_effort":"off"}`, ""},
		{"disabled 关闭", `{"model":"m","reasoning_effort":"disabled"}`, ""},
		{"auto 交由上游默认", `{"model":"m","reasoning_effort":"auto"}`, ""},
		{"default 交由上游默认", `{"model":"m","reasoning_effort":"default"}`, ""},
		{"high 原样", `{"model":"m","reasoning_effort":"high"}`, "high"},
		{"low 原样", `{"model":"m","reasoning_effort":"low"}`, "low"},
		{"minimal 原样", `{"model":"m","reasoning_effort":"minimal"}`, "minimal"},
		{"xhigh 原样", `{"model":"m","reasoning_effort":"xhigh"}`, "xhigh"},
		{"大写归一", `{"model":"m","reasoning_effort":"HIGH"}`, "high"},
		{"混合大小写归一", `{"model":"m","reasoning_effort":"Medium"}`, "medium"},
		{"空白归一", `{"model":"m","reasoning_effort":" high "}`, "high"},
		{"嵌套 reasoning.effort 提升", `{"model":"m","reasoning":{"effort":"low"}}`, "low"},
		{"顶层优先于嵌套", `{"model":"m","reasoning_effort":"high","reasoning":{"effort":"low"}}`, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := mustOrdered(t, tc.body)
			applyThinkingRules(obj)
			got, exists := obj.Get("reasoning_effort")
			if tc.want == "" {
				if exists {
					t.Fatalf("reasoning_effort should be absent, got %#v", got)
				}
				return
			}
			if !exists || got != tc.want {
				t.Fatalf("reasoning_effort = %#v (exists=%v), want %q", got, exists, tc.want)
			}
		})
	}
}

// 验证网关不凭空注入 reasoning_summary：客户端未传时上游收到的报文里不得出现该字段。
//
// 官方客户端发往同一上游的报文不含 reasoning_summary（命中 externalDomain 时
// SdkFieldCleanupRule 会删除该字段）；网关自行追加会构成可识别的报文差异。
func TestApplyThinkingRulesNeverInjectsReasoningSummary(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[]}`,
		`{"model":"m","reasoning_effort":"high"}`,
		`{"model":"m","reasoning":{"effort":"high"}}`,
		`{"model":"m","reasoning_effort":"none"}`,
	} {
		obj := mustOrdered(t, body)
		applyThinkingRules(obj)
		if v, exists := obj.Get("reasoning_summary"); exists {
			t.Fatalf("body %s: reasoning_summary injected as %#v", body, v)
		}
	}
}

// 验证关闭推理时清掉客户端自带的 reasoning_summary，避免「已关闭却索要摘要」的矛盾组合。
func TestApplyThinkingRulesDropsSummaryWhenReasoningDisabled(t *testing.T) {
	obj := mustOrdered(t, `{"model":"m","reasoning_effort":"none","reasoning_summary":"auto"}`)
	applyThinkingRules(obj)
	if v, exists := obj.Get("reasoning_summary"); exists {
		t.Fatalf("reasoning_summary should be dropped, got %#v", v)
	}
	// 显式请求思考时，客户端自带的 summary 必须原样保留（网关不改写其取值）。
	obj = mustOrdered(t, `{"model":"m","reasoning_effort":"high","reasoning_summary":"concise"}`)
	applyThinkingRules(obj)
	if v, _ := obj.Get("reasoning_summary"); v != "concise" {
		t.Fatalf("reasoning_summary = %#v, want concise", v)
	}
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
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
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

// 验证 Responses 的 reasoning.effort / reasoning.summary / text.verbosity
// 全部摊平为上游认识的扁平字段。
//
// 上游实测只解析扁平字段：嵌套 reasoning.effort 被完全忽略（推理根本不开启）。
// summary 取值上游不校验（bogus/none/null 均 200），故原样透传、不改写。
func TestResponsesToChatRequestFlattensReasoningFields(t *testing.T) {
	chat, err := responsesToChatRequest(mustOrdered(t, `{
		"model": "x",
		"input": "hi",
		"reasoning": {"effort": "medium", "summary": "concise"},
		"text": {"verbosity": "low"}
	}`), "x")
	if err != nil {
		t.Fatal(err)
	}
	applyThinkingRules(chat)

	if v, _ := chat.Get("reasoning_effort"); v != "medium" {
		t.Fatalf("reasoning_effort = %#v, want medium", v)
	}
	if v, _ := chat.Get("reasoning_summary"); v != "concise" {
		t.Fatalf("reasoning_summary = %#v, want concise", v)
	}
	if v, _ := chat.Get("verbosity"); v != "low" {
		t.Fatalf("verbosity = %#v, want low", v)
	}

	// 非字符串 / null 的 summary 不得透传成上游无法解析的值
	chat, err = responsesToChatRequest(mustOrdered(t, `{
		"model": "x", "input": "hi",
		"reasoning": {"effort": "high", "summary": null}
	}`), "x")
	if err != nil {
		t.Fatal(err)
	}
	applyThinkingRules(chat)
	if v, exists := chat.Get("reasoning_summary"); exists {
		t.Fatalf("null summary should not be forwarded, got %#v", v)
	}
	t.Log("responses reasoning fields flattened OK")
}

// 验证 Responses 请求关闭推理时不向上游发送 reasoning_effort（none 语义等价于关闭）。
func TestResponsesToChatRequestDisablesReasoning(t *testing.T) {
	chat, err := responsesToChatRequest(mustOrdered(t, `{
		"model": "x", "input": "hi", "reasoning": {"effort": "none"}
	}`), "x")
	if err != nil {
		t.Fatal(err)
	}
	applyThinkingRules(chat)
	if v, exists := chat.Get("reasoning_effort"); exists {
		t.Fatalf("reasoning_effort should be absent for none, got %#v", v)
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

// realClientChatHeaders 是官方客户端发往国际站 /v2/chat/completions 的真实请求头基线，
// 逐字取自 WorkBuddyAI desktop 5.7.6 驱动内置 CLI（包版本 2.156.0）直连非回环地址的抓包。
//
// 基线含两类不参与应用层对齐的头：
//   - 传输层：Host / Content-Length / Connection（由 net/http 自行管理）
//   - Content-Encoding：由 upstreamBody 在 doUpstreamRequest 里按站点特性写入
//     （gateway 只在「拼头」阶段比对，压缩在发送阶段，见 TestIntlModelRequestBodyIsGzipped）
//
// Accept-Encoding 属应用层基线：值来自 axios 默认头，由 Node http 适配器在传输层补写，
// 线上字节位于 Content-Length 之后、Host 之前；网关在 backendHeaders 里显式声明同一值
// （响应侧自行解压，见 TestCompressedUpstreamResponseIsDecoded）。
//
// x-codebuddy-request 属应用层基线：客户端对模型请求恒发送该头（withSecurityHeader 注入，
// 桌面宿主 daemon-bootstrap 的模型请求构造器亦显式 set），且目标为非回环地址时同样携带。
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
	{"x-stainless-runtime-version", "v22.21.1"},
	{"X-Conversation-ID", "01a1382f-91d3-714c-a1ef-0487655ec2b0"},
	{"X-Conversation-Request-ID", "01a1382f925a7f33b5adbd0105db2289"},
	{"X-Agent-Intent", "craft"},
	{"X-Agent-Purpose", "conversation"},
	{"X-IDE-Type", "WorkBuddy"},
	{"X-IDE-Name", "WorkBuddy"},
	{"X-IDE-Version", "5.7.6"},
	{"X-Private-Data", "true"},
	{"x-codebuddy-request", "1"},
	{"X-Request-ID", "01a1382f93107f5b9a4fff258e4f34f4"},
	{"X-Conversation-Message-ID", "01a1382f93107f5b9a4fff258e4f34f4"},
	{"X-Root-Request-ID", "01a1382f925a7f33b5adbd0105db2289"},
	{"X-Agent-Type", "main"},
	{"traceparent", "00-db2fa486083cae43e282286363b59112-12bbcfc116d7a52b-01"},
	{"b3", "db2fa486083cae43e282286363b59112-12bbcfc116d7a52b-1-6afdfbcaf2b0a3a7"},
	{"X-B3-TraceId", "db2fa486083cae43e282286363b59112"},
	{"X-B3-ParentSpanId", "6afdfbcaf2b0a3a7"},
	{"X-B3-SpanId", "12bbcfc116d7a52b"},
	{"X-B3-Sampled", "1"},
	{"X-Trace-ID", "db2fa486083cae43e282286363b59112"},
	// 基线中为真实 Bearer JWT，此处保留前缀（仅校验存在性与方案，不校验具体令牌）
	{"Authorization", "Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJXVzhVVkZuS0l"},
	{"X-User-Id", "8efc9f5d-4289-445e-b503-c8a49eeb52c5"},
	{"X-Domain", "www.workbuddy.ai"},
	{"X-Product", "SaaS"},
	{"User-Agent", "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0"},
	{"Accept-Encoding", "gzip, compress, deflate, br"},
}

// nonApplicationHeaders 是不参与应用层对齐的头。
var nonApplicationHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Connection": true,
	"Content-Encoding": true,
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
	backendHeaders(req, sa, &profileINTL, nil, sessionScope{})
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
	// 会话级三者同值：客户端源码 X-Trace-ID 取 OTel traceId，
	// X-Conversation-Request-ID / X-Root-Request-ID 取 session.conversationRequestId，
	// 9 条会话抓包实测完全相等。
	if a, b, c := getHeaderExact(h, "X-Trace-ID"), getHeaderExact(h, "X-Conversation-Request-ID"), getHeaderExact(h, "X-Root-Request-ID"); a != b || b != c {
		t.Errorf("session-scoped link ids must be equal: X-Trace-ID=%q X-Conversation-Request-ID=%q X-Root-Request-ID=%q", a, b, c)
	}
	if getHeaderExact(h, "X-Conversation-Message-ID") != getHeaderExact(h, "X-Request-ID") {
		t.Error("X-Conversation-Message-ID must equal X-Request-ID (observed client behavior)")
	}
	// 每请求级与会话级必须解耦：客户端 messageId 与 conversationRequestId 是两个字段。
	if getHeaderExact(h, "X-Request-ID") == getHeaderExact(h, "X-Conversation-Request-ID") {
		t.Error("X-Request-ID (per-request) must be decoupled from X-Conversation-Request-ID (per-session)")
	}
	if cid := getHeaderExact(h, "X-Conversation-ID"); len(cid) != 36 || strings.Count(cid, "-") != 4 {
		t.Errorf("X-Conversation-ID = %q, want dashed UUID", cid)
	}
	// 4b. ID 必须为 UUID v7 形态：客户端用 uuidv7 生成会话/消息 ID（版本位恒为 7，
	// 前 48 位为毫秒时间戳），随机 v4 属可静态识别的偏差。
	for _, n := range []string{"X-Request-ID", "X-Conversation-Message-ID"} {
		if v := getHeaderExact(h, n); v[12] != '7' {
			t.Errorf("%s = %q, want UUIDv7 version nibble '7'", n, v)
		}
	}
	if cid := getHeaderExact(h, "X-Conversation-ID"); strings.ReplaceAll(cid, "-", "")[12] != '7' {
		t.Errorf("X-Conversation-ID = %q, want UUIDv7 version nibble '7'", cid)
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
	backendHeaders(req, sa, &profileINTL, nil, sessionScope{})
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
	backendHeaders(req, nil, &profileINTL, nil, sessionScope{})
	if got := getHeaderExact(req.Header, "X-No-Authorization"); got != "true" {
		t.Errorf("X-No-Authorization = %q, want \"true\"", got)
	}
	if got := getHeaderExact(req.Header, "X-No-User-Id"); got != "true" {
		t.Errorf("X-No-User-Id = %q, want \"true\"", got)
	}
	if got := getHeaderExact(req.Header, "X-Domain"); got != profileINTL.domain() {
		t.Errorf("X-Domain = %q, want fallback %q", got, profileINTL.domain())
	}
}

// 登录链路三步（auth/state → auth/token 轮询 → login/account）的头集合必须与客户端
// 源码逐项一致：控制面走 axios 族（与模型请求不同），且三步的 X-No-* / X-Domain /
// Authorization 组合各自不同。上游只看这三个请求就能分辨「官方客户端登录」与「自造客户端」。
func TestLoginControlPlaneHeadersMatchClient(t *testing.T) {
	prof := &profileINTL
	controlOnly := map[string]string{
		"Accept":           "application/json, text/plain, */*",
		"X-Requested-With": "XMLHttpRequest",
		"User-Agent":       "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0",
		"X-Product":        "SaaS",
		"Accept-Encoding":  "gzip, compress, deflate, br",
	}
	// 控制面请求不得出现模型请求专有的头，也不得出现任何 x-stainless-*。
	mustBeAbsent := []string{
		"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "X-Agent-Type", "X-Agent-Intent",
		"X-Agent-Purpose", "X-Private-Data", "x-codebuddy-request",
		"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Root-Request-ID", "X-Parent-Conversation-ID", "X-Session-ID",
		// traceparent 只出现在模型请求链路；控制面的 traceSpan.requestHeaders 不含它
		"traceparent",
		"x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version",
		"x-stainless-retry-count", "x-stainless-runtime", "x-stainless-runtime-version",
	}

	steps := []struct {
		name    string
		method  string
		body    io.Reader
		headers func(*http.Request)
		want    map[string]string
		absent  []string
	}{
		{
			// fetchAuthState: post(`/v2${prefixPath}/auth/state?platform=...`, {})
			name:    "auth/state",
			method:  http.MethodPost,
			body:    bytes.NewReader([]byte("{}")),
			headers: loginStateHeaders(prof),
			want: map[string]string{
				"Content-Type":         "application/json",
				"X-Domain":             "www.workbuddy.ai",
				"X-No-Authorization":   "true",
				"X-No-User-Id":         "true",
				"X-No-Enterprise-Id":   "true",
				"X-No-Department-Info": "true",
			},
		},
		{
			// loopGetToken: get(`/v2${prefixPath}/auth/token?state=${state}`)
			name:    "auth/token",
			method:  http.MethodGet,
			headers: loginPollHeaders(prof),
			want: map[string]string{
				"X-No-Authorization":   "true",
				"X-No-User-Id":         "true",
				"X-No-Enterprise-Id":   "true",
				"X-No-Department-Info": "true",
			},
			// loopGetToken 不调 enterpriseHeaders，故不带 X-Domain；GET 无 body，故无 Content-Type
			absent: []string{"X-Domain", "Content-Type"},
		},
		{
			// loopGetAccount: get(`/v2${prefixPath}/login/account?state=${state}`)
			name:    "login/account",
			method:  http.MethodGet,
			headers: loginAccountHeaders(prof, "tok-123", "www.workbuddy.ai"),
			want: map[string]string{
				"Authorization":        "Bearer tok-123",
				"X-Domain":             "www.workbuddy.ai",
				"X-No-User-Id":         "true",
				"X-No-Enterprise-Id":   "true",
				"X-No-Department-Info": "true",
			},
			// 该步必带 Authorization，故不声明 X-No-Authorization；GET 无 body，故无 Content-Type
			absent: []string{"X-No-Authorization", "Content-Type"},
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			req, _ := http.NewRequest(step.method, prof.Base+"/v2/plugin/x", step.body)
			step.headers(req)
			for k, v := range controlOnly {
				if got := getHeaderExact(req.Header, k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for k, v := range step.want {
				if got := getHeaderExact(req.Header, k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for _, k := range append(mustBeAbsent, step.absent...) {
				if v := getHeaderExact(req.Header, k); v != "" {
					t.Errorf("%s must be absent, got %q", k, v)
				}
			}
			// 链路传播头：traceSpan.requestHeaders 的六项，且组内自洽。
			// X-Request-ID 由 injectRequestIdHeader 取 X-Trace-ID，故两者恒等。
			trace := getHeaderExact(req.Header, "X-Trace-ID")
			if len(trace) != 32 || !isHexLen(trace, 32) {
				t.Errorf("X-Trace-ID = %q, want 32-hex trace id", trace)
			}
			if got := getHeaderExact(req.Header, "X-Request-ID"); got != trace {
				t.Errorf("X-Request-ID = %q, want X-Trace-ID %q (injectRequestIdHeader 取 trace id)", got, trace)
			}
			span := getHeaderExact(req.Header, "X-B3-SpanId")
			if len(span) != 16 || !isHexLen(span, 16) {
				t.Errorf("X-B3-SpanId = %q, want 16-hex span id", span)
			}
			if got := getHeaderExact(req.Header, "X-B3-TraceId"); got != trace {
				t.Errorf("X-B3-TraceId = %q, want %q", got, trace)
			}
			// 无父 span 的控制面请求：b3 末段为空、X-B3-ParentSpanId 为空值
			if got, want := getHeaderExact(req.Header, "b3"), trace+"-"+span+"-1-"; got != want {
				t.Errorf("b3 = %q, want %q", got, want)
			}
			if v, ok := req.Header["X-B3-ParentSpanId"]; !ok || len(v) != 1 || v[0] != "" {
				t.Errorf("X-B3-ParentSpanId must be present and empty, got %v", req.Header["X-B3-ParentSpanId"])
			}
			if got := getHeaderExact(req.Header, "X-B3-Sampled"); got != "1" {
				t.Errorf("X-B3-Sampled = %q, want \"1\"", got)
			}
		})
	}
}

// 刷新链路在客户端是 `refreshSession`：`post('/v2${prefixPath}/auth/token/refresh', {})`。
// axios 因 body 存在而补 Content-Type，线上 Content-Length 为 2。网关此前发空 body，
// Content-Length(0) 与 Content-Type(缺失) 都与客户端不同 —— 该用例锁住这两项。
func TestRefreshRequestMatchesClientBody(t *testing.T) {
	type wire struct {
		method  string
		path    string
		body    string
		length  int64
		ctype   string
		headers http.Header
	}
	got := make(chan wire, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got <- wire{
			method: r.Method, path: r.URL.Path, body: string(raw),
			length: r.ContentLength, ctype: r.Header.Get("Content-Type"),
			headers: r.Header.Clone(),
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`))
	}))
	t.Cleanup(server.Close)

	oldBase, oldClient := profileINTL.Base, cfg.HttpClient
	profileINTL.Base, cfg.HttpClient = server.URL, server.Client()
	t.Cleanup(func() { profileINTL.Base, cfg.HttpClient = oldBase, oldClient })

	sa := &StoredAuth{
		Edition: "intl",
		Auth: StoredTokens{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			Domain:       "www.workbuddy.ai",
		},
		Account: StoredAccount{UID: "uid-1"},
	}
	if _, err := refreshTokenPayload(sa); err != nil {
		t.Fatalf("refreshTokenPayload: %v", err)
	}

	var w wire
	select {
	case w = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("上游未收到刷新请求")
	}
	if w.method != http.MethodPost || w.path != "/v2/plugin/auth/token/refresh" {
		t.Fatalf("刷新请求 = %s %s, want POST /v2/plugin/auth/token/refresh", w.method, w.path)
	}
	if w.body != "{}" || w.length != 2 {
		t.Errorf("body=%q Content-Length=%d, want {} / 2（客户端 post(url, {}) 的线形态）", w.body, w.length)
	}
	if w.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", w.ctype)
	}
	for name, want := range map[string]string{
		"X-Refresh-Token":       "refresh-1",
		"X-Auth-Refresh-Source": "plugin",
		"X-Domain":              "www.workbuddy.ai",
		"Authorization":         "Bearer access-1",
		"X-User-Id":             "uid-1",
		"X-Product":             "SaaS",
		"Accept":                "application/json, text/plain, */*",
	} {
		if v := w.headers.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	// 控制面族：模型请求专有头一律不得出现
	for _, name := range []string{"X-IDE-Type", "X-IDE-Version", "x-stainless-lang", "traceparent", "X-Conversation-ID", "X-Agent-Intent", "x-codebuddy-request"} {
		if v := w.headers.Get(name); v != "" {
			t.Errorf("%s 不应出现在刷新请求里，got %q", name, v)
		}
	}
}

// x-codebuddy-request 是客户端对模型请求恒发送的头（抓包实测，含非回环目标），故网关必须
// 合成；客户端真带上时以客户端值为准。同时下游客户端的 User-Agent / x-stainless-* 不得透传
// —— 那是第三方 SDK 的身份（如 claude-cli），会与 WorkBuddy IDE 头形成矛盾组合。
func TestClientPassthroughAndSynthesisBoundary(t *testing.T) {
	sa := &StoredAuth{
		Auth:    StoredTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
		Account: StoredAccount{UID: "u"},
	}

	// 未携带时也必须合成（客户端恒发该头，缺失即偏差）
	req, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req, sa, &profileINTL, nil, sessionScope{})
	if v := getHeaderExact(req.Header, "x-codebuddy-request"); v != "1" {
		t.Errorf("gateway must synthesize x-codebuddy-request=1, got %q", v)
	}

	// 客户端携带时透传，且身份头优先于合成值
	in := http.Header{}
	in.Set("x-codebuddy-request", "1")
	in.Set("X-Conversation-ID", "11111111-2222-3333-4444-555555555555")
	in.Set("X-Agent-Intent", "ask")
	in.Set("X-IDE-Version", "9.9.9")
	in.Set("User-Agent", "claude-cli/2.0.0 (external, cli)")
	in.Set("x-stainless-package-version", "0.60.0")
	in.Set("x-stainless-runtime-version", "v24.11.1")
	req2, _ := http.NewRequest(http.MethodPost, profileINTL.chatURL(), nil)
	backendHeaders(req2, sa, &profileINTL, in, sessionScope{})
	if v := getHeaderExact(req2.Header, "x-codebuddy-request"); v != "1" {
		t.Errorf("client-provided x-codebuddy-request must pass through, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "X-Conversation-ID"); v != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("client-provided X-Conversation-ID must win, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "X-Agent-Intent"); v != "ask" {
		t.Errorf("client-provided X-Agent-Intent must win, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "X-IDE-Version"); v != "9.9.9" {
		t.Errorf("client-provided X-IDE-Version must win, got %q", v)
	}
	// 第三方下游的 SDK 身份必须被合成值覆盖，不得出现在上游
	if v := getHeaderExact(req2.Header, "User-Agent"); v != "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0" {
		t.Errorf("downstream User-Agent must not reach upstream, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "x-stainless-package-version"); v != "6.25.0" {
		t.Errorf("downstream x-stainless-package-version must not reach upstream, got %q", v)
	}
	if v := getHeaderExact(req2.Header, "x-stainless-runtime-version"); v != "v22.21.1" {
		t.Errorf("downstream x-stainless-runtime-version must not reach upstream, got %q", v)
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
	backendHeaders(req, sa, &profileINTL, evil, sessionScope{})

	for name, want := range map[string]string{
		"Authorization":   "Bearer tok",
		"X-User-Id":       "u",
		"X-Domain":        "www.workbuddy.ai",
		"X-Product":       "SaaS",
		"X-Enterprise-Id": "ent",
		// 客户端 AuthenticationHttpInterceptor 在账号有 enterpriseId 时无条件补 X-Tenant-Id，
		// 取值与 X-Enterprise-Id 相同；下游不得覆盖。
		"X-Tenant-Id": "ent",
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

// /v1/models 必须反映「实时接口 + npm 静态目录」按站点合并后的真实清单，
// 并暴露目录来源；跨站点同名模型只出现一次。
func TestModelsListReflectsMergedCatalog(t *testing.T) {
	resetModelsStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "hy3", FromLive: true}, {ID: "glm-5.2", FromLive: true}},
		"intl": {{ID: "hy3", FromLive: true}, {ID: "official-dynamic-1", FromLive: true}},
	}
	dynamicSource = "2026-09-18 10:00"
	modelsMu.Unlock()

	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if got := rec.Header().Get("X-Model-Source"); got != "live-api@2026-09-18 10:00" {
		t.Errorf("X-Model-Source = %q, want live-api@2026-09-18 10:00", got)
	}

	// 契约：与 mergedModelIDs() 的顺序/长度一致（catalogSites 顺序 + 站点内目录顺序），且无重复。
	want, _ := mergedModelIDs()
	if len(resp.Data) != len(want) {
		t.Fatalf("got %d models, want %d (merged catalog): %v", len(resp.Data), len(want), resp.Data)
	}
	seen := make(map[string]bool, len(resp.Data))
	for i, m := range resp.Data {
		if m.ID != want[i] {
			t.Fatalf("data[%d].id = %q, want %q (merged order)", i, m.ID, want[i])
		}
		if m.Object != "model" || m.OwnedBy != "workbuddy" {
			t.Errorf("model %q malformed: object=%q owned_by=%q", m.ID, m.Object, m.OwnedBy)
		}
		if seen[m.ID] {
			t.Errorf("duplicate model %q in response", m.ID)
		}
		seen[m.ID] = true
	}
	// 两个站点各自的目录都必须出现在响应里（证明按站点合并而非只取其一）
	for _, id := range []string{"glm-5.2", "official-dynamic-1"} {
		if !seen[id] {
			t.Errorf("model %q missing from response: %v", id, want)
		}
	}
}

// gunzipUpstreamBody 解压出站请求体：站点 Profile 的 GzipBody 开启时（两站当前都开启），
// 假上游收到的是压缩字节。断言对象是「上游实际收到什么」，故按 gzip 魔数判定后解压，
// 未压缩的请求体原样返回。可从任意 goroutine 调用（只使用 t.Errorf）。
func gunzipUpstreamBody(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Errorf("解压出站请求体失败: %v", err)
		return body
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Errorf("读取解压后的出站请求体失败: %v", err)
		return body
	}
	return plain
}

// accountForTest 写入一个指定站点的凭据文件并装载进账号池（走真实凭据加载路径
// loadAccountFile，而非手工构造 Account）；结束测试时还原账号池。
func accountForTest(t *testing.T, edition, domain, token, uid string) {
	t.Helper()

	credPath := filepath.Join(t.TempDir(), "workbuddy-"+edition+".json")
	raw, _ := json.Marshal(map[string]any{
		"auth": map[string]any{
			"accessToken":  token,
			"refreshToken": token + "-refresh",
			"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
			"domain":       domain,
		},
		"account": map[string]any{"uid": uid},
		"edition": edition,
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
	accounts = append(accounts, &Account{Path: credPath, Auth: sa, Edition: edition})
	accountMu.Unlock()

	t.Cleanup(func() {
		accountMu.Lock()
		accounts = prev
		accountMu.Unlock()
	})
}

func intlAccountForTest(t *testing.T, token, uid string) {
	t.Helper()
	accountForTest(t, "intl", "www.workbuddy.ai", token, uid)
}

// 端到端冒烟：驱动真实 handleChatCompletions → upstreamChat → 生产 transport →
// 本地假上游，抓取实际上线请求头。覆盖单测无法触及的传输层（DisableCompression）
// 与透传接线。
func TestSmokeEndToEndUpstreamHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	gotPath := make(chan string, 1)
	gotBody := make(chan []byte, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		got <- r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody <- raw
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
	intlAccountForTest(t, "smoke-token", "8efc9f5d-4289-445e-b503-c8a49eeb52c5")

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
	// 合成指纹头存在（国际站 = 桌面宿主 5.7.6 + bundled CLI 2.156.0）
	if v := getHeaderExact(h, "User-Agent"); v != "WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0" {
		t.Errorf("User-Agent = %q", v)
	}
	if v := getHeaderExact(h, "x-stainless-runtime"); v != "node" {
		t.Errorf("X-Stainless-Runtime = %q", v)
	}
	if v := getHeaderExact(h, "x-codebuddy-request"); v != "1" {
		t.Errorf("x-codebuddy-request = %q, want \"1\"", v)
	}
	// 模型请求体按国际站 product.json 的 RequestBodyGzip 压缩
	if v := getHeaderExact(h, "Content-Encoding"); v != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", v)
	}
	select {
	case raw := <-gotBody:
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("upstream body is not gzip: %v", err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if !bytes.Contains(plain, []byte(`"model":"default-model"`)) {
			t.Errorf("decompressed upstream body = %s", plain)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream body captured")
	}
	// 浏览器语义头不得出现
	if v := h.Get("Origin"); v != "" {
		t.Errorf("Origin must not be sent, got %q", v)
	}
	if v := h.Get("Referer"); v != "" {
		t.Errorf("Referer must not be sent, got %q", v)
	}
	// 压缩协商声明必须与客户端一致（5.7.6 线上字节：Content-Length 之后、Host 之前）。
	// 值来自 axios 默认头，由 Node http 适配器在传输层补写；网关同样关闭了
	// Transport 自动压缩，响应解压由 wrapUpstreamDecoding 自行完成。
	if v := h.Get("Accept-Encoding"); v != "gzip, compress, deflate, br" {
		t.Errorf("Accept-Encoding = %q, want \"gzip, compress, deflate, br\"", v)
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

// 国内站模型请求的端到端指纹：驱动真实 handleChatCompletions → upstreamChat →
// 生产 transport → 本地假上游。国内站身份与国际站**不同**（客户端 5.5.3 + bundled
// CLI 2.137.1 的抓包实测）：
//   - UA `WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1`（中段是 productName，国内站也是 WorkBuddy）
//   - X-IDE-Type / X-IDE-Name `WorkBuddy`、X-IDE-Version `5.5.3`
//   - x-stainless-runtime-version `v22.21.1`（宿主 Electron 37.10.3 内置 Node）
//   - 请求体 gzip，且**不发** Accept-Encoding（CLI 2.137.1 模型与控制面两族都不发）
func TestCNModelRequestMatchesClient(t *testing.T) {
	got := make(chan http.Header, 1)
	gotBody := make(chan []byte, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	origBase := profileCN.Base
	profileCN.Base = up.URL
	defer func() { profileCN.Base = origBase }()

	// 生产 transport（含 DisableCompression 等指纹相关设置）
	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	accountForTest(t, "cn", "copilot.tencent.com", "cn-smoke-token", "u-cn")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"default-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	var h http.Header
	select {
	case h = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received request")
	}

	for name, want := range map[string]string{
		"User-Agent":                  "WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1",
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               "5.5.3",
		"x-stainless-runtime-version": "v22.21.1",
		"x-stainless-package-version": "6.25.0",
		"X-Product":                   "SaaS",
		"X-Domain":                    "copilot.tencent.com",
		"Authorization":               "Bearer cn-smoke-token",
		"Content-Encoding":            "gzip",
	} {
		if v := getHeaderExact(h, name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	// 国内站客户端不声明压缩协商：多出该头即为可静态识别的偏差。
	if v := h.Get("Accept-Encoding"); v != "" {
		t.Errorf("cn request must not carry Accept-Encoding, got %q", v)
	}
	select {
	case raw := <-gotBody:
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("cn upstream body is not gzip: %v", err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if !bytes.Contains(plain, []byte(`"model":"default-model"`)) {
			t.Errorf("decompressed cn body = %s", plain)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no cn upstream body captured")
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("client body = %q", rec.Body.String())
	}
}

// 国内站控制面（签到/额度/刷新）同样不发 Accept-Encoding —— 该头随站点分叉，
// 用国际站的值覆盖国内站会多出一个客户端实测不存在的头。
func TestCNControlPlaneOmitsAcceptEncoding(t *testing.T) {
	got := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
	}))
	defer server.Close()

	origBase, origPortal := profileCN.Base, profileCN.PortalOrigin
	profileCN.Base, profileCN.PortalOrigin = server.URL, server.URL
	defer func() { profileCN.Base, profileCN.PortalOrigin = origBase, origPortal }()

	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	if _, _, err := doJSON(cfg.HttpClient, http.MethodGet, profileCN.quotaSummaryURL(),
		func(r *http.Request) { controlHeaders(r, &profileCN) }, nil); err != nil {
		t.Fatalf("cn control-plane request: %v", err)
	}

	select {
	case h := <-got:
		if v := h.Get("Accept-Encoding"); v != "" {
			t.Errorf("cn control-plane must not carry Accept-Encoding, got %q", v)
		}
		if v := getHeaderExact(h, "User-Agent"); v != "WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1" {
			t.Errorf("cn control-plane User-Agent = %q", v)
		}
		if v := getHeaderExact(h, "Accept"); v != "application/json, text/plain, */*" {
			t.Errorf("cn control-plane Accept = %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cn control-plane upstream never received request")
	}

	// 国际站仍带该头（避免把站点分叉写成「两边都不发」）。
	ireq, err := http.NewRequest(http.MethodGet, profileINTL.quotaSummaryURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	controlHeaders(ireq, &profileINTL)
	if v := ireq.Header.Get("Accept-Encoding"); v != "gzip, compress, deflate, br" {
		t.Errorf("intl control-plane Accept-Encoding = %q", v)
	}
}

// 上游按我们声明的 `Accept-Encoding: gzip, compress, deflate, br` 压缩响应时，
// 解压必须由网关完成：production transport 关掉了自动解压（指纹需要），
// 若不解压则压缩字节会被当成 SSE 明文喂给下游。
//
// 断言的是可观察结果：br / gzip / deflate / 不压缩四种情况下，下游都拿到明文 SSE 事件；
// 未支持的编码必须显式失败，绝不把压缩字节伪装成正常响应。
func TestCompressedUpstreamResponseIsDecoded(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	plain := func(in []byte) ([]byte, error) { return in, nil }
	encoders := map[string]func([]byte) ([]byte, error){
		"":        plain,
		"gzip":    compressor(func(w io.Writer) io.WriteCloser { return gzip.NewWriter(w) }),
		"deflate": compressor(func(w io.Writer) io.WriteCloser { return zlib.NewWriter(w) }),
		"br":      compressor(func(w io.Writer) io.WriteCloser { return brotli.NewWriter(w) }),
		// 未支持的编码：载荷无所谓，网关必须在读之前就拒绝
		"zstd": plain,
	}

	for encoding, encode := range encoders {
		name := encoding
		if name == "" {
			name = "identity"
		}
		t.Run(name, func(t *testing.T) {
			payload, err := encode([]byte(sse))
			if err != nil {
				t.Fatal(err)
			}
			seenEncoding := make(chan string, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenEncoding <- r.Header.Get("Accept-Encoding")
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(payload)
			}))
			defer up.Close()

			origBase := profileINTL.Base
			profileINTL.Base = up.URL
			defer func() { profileINTL.Base = origBase }()

			// 生产 transport（DisableCompression）：解压只能由网关自己做
			origClient := cfg.HttpClient
			initHTTPClient()
			defer func() { cfg.HttpClient = origClient }()

			intlAccountForTest(t, "zip-token", "u-zip")

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"default-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handleChatCompletions(rec, req)

			select {
			case got := <-seenEncoding:
				if got != "gzip, compress, deflate, br" {
					t.Errorf("upstream saw Accept-Encoding = %q, want 客户端值", got)
				}
			default:
				t.Error("upstream never received the request")
			}

			body := rec.Body.String()
			if encoding == "zstd" {
				if rec.Code != http.StatusBadGateway || !strings.Contains(body, "upstream_encoding_unsupported") {
					t.Errorf("unsupported encoding must fail loudly: status=%d body=%q", rec.Code, body)
				}
				return
			}
			if !strings.Contains(body, `"ok"`) || !strings.Contains(body, "data: [DONE]") {
				t.Errorf("downstream did not receive decoded SSE (Content-Encoding %q): status=%d body=%q", encoding, rec.Code, body)
			}
		})
	}
}

// 上游的**错误**响应同样可能被压缩（我们声明了 br）。限流 / 额度 / 鉴权的判定与日志
// 都依赖错误正文里的关键字，拿压缩字节去判定会把可识别的上游错误降级成「未知错误」
// （账号不再被冷却/禁用，下游也拿不到原文）。
func TestCompressedUpstreamErrorResponseIsDecoded(t *testing.T) {
	const upstreamBody = `{"code":11140,"msg":"request illegal"}`
	payload, err := compressor(func(w io.Writer) io.WriteCloser { return gzip.NewWriter(w) })([]byte(upstreamBody))
	if err != nil {
		t.Fatal(err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(payload)
	}))
	defer up.Close()

	origBase := profileINTL.Base
	profileINTL.Base = up.URL
	defer func() { profileINTL.Base = origBase }()

	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	intlAccountForTest(t, "err-token", "u-err")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"default-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want upstream status 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request illegal") {
		t.Errorf("downstream must see the decoded upstream error body, got %q", rec.Body.String())
	}
}

// compressor 返回一个「压缩入参字节」的编码器。
// 必须先 Close 再取字节：gzip 的 CRC/长度、zlib 的校验和、brotli 的收尾块都在 Close
// 时写出，先取字节会得到截断流（解码侧报 unexpected EOF）。
func compressor(newWriter func(io.Writer) io.WriteCloser) func([]byte) ([]byte, error) {
	return func(in []byte) ([]byte, error) {
		var buf bytes.Buffer
		w := newWriter(&buf)
		if _, err := w.Write(in); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// -----------------------------------------------------------------------------
// 会话作用域回归
//
// 官方客户端把链路 ID 分两个作用域（源码 + 9 条会话 / 19 次请求抓包实测）：
//   - 会话级：X-Conversation-ID、X-Conversation-Request-ID、X-Root-Request-ID、X-Trace-ID
//   - 每请求级：X-Request-ID、X-Conversation-Message-ID、span 部分（traceparent/b3/X-B3-*）
//
// 网关此前把会话级四个头也按请求新生成，导致「同一会话的连续请求携带互不相同的
// 会话链路 ID」—— 比对同一会话的两次请求即可判定，是稳定的机器特征。
// -----------------------------------------------------------------------------

// upstreamHeadersForSession 驱动真实 handleChatCompletions 链路，返回上游收到的 Header。
// hdrs 模拟客户端携带的会话头（如 X-Conversation-ID / X-Conversation-Request-ID）。
// audit 为真时包一层 requestAuditMiddleware，复现调试开关下的出站头覆盖路径。
func upstreamHeadersForSession(t *testing.T, hdrs map[string]string, audit bool) http.Header {
	t.Helper()

	got := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	origClient := cfg.HttpClient
	initHTTPClient()
	defer func() { cfg.HttpClient = origClient }()

	intlAccountForTest(t, "sess-token", "u1")

	body := `{"model":"default-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	var handler http.Handler = http.HandlerFunc(handleChatCompletions)
	if audit {
		handler = requestAuditMiddleware(handler)
	}
	handler.ServeHTTP(rec, req)

	select {
	case h := <-got:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received request")
		return nil
	}
}

// 同一会话的连续请求必须复用会话级链路 ID，且每请求级 ID 必须各不相同。
func TestSessionScopedLinkIDsAreStableWithinConversation(t *testing.T) {
	const conv = "04bad56e-08d5-4647-9c3c-28e12897c1af"
	hdrs := map[string]string{"X-Conversation-ID": conv}
	a := upstreamHeadersForSession(t, hdrs, false)
	b := upstreamHeadersForSession(t, hdrs, false)

	// 会话级：两次请求必须逐字相同
	for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-Trace-ID"} {
		va, vb := getHeaderExact(a, name), getHeaderExact(b, name)
		if va != vb {
			t.Errorf("%s must be stable within a conversation: %q vs %q", name, va, vb)
		}
	}
	// 会话级三者同值（客户端源码语义 + 抓包实测）
	if x, y, z := getHeaderExact(a, "X-Conversation-Request-ID"), getHeaderExact(a, "X-Root-Request-ID"), getHeaderExact(a, "X-Trace-ID"); x != y || y != z {
		t.Errorf("session-scoped ids must be equal: %q %q %q", x, y, z)
	}
	// X-Conversation-ID 原样透传客户端会话键
	if v := getHeaderExact(a, "X-Conversation-ID"); v != conv {
		t.Errorf("X-Conversation-ID = %q, want client-provided %q", v, conv)
	}
	// 每请求级：两次请求必须不同
	for _, name := range []string{"X-Request-ID", "X-Conversation-Message-ID", "X-B3-SpanId", "X-B3-ParentSpanId"} {
		if va, vb := getHeaderExact(a, name), getHeaderExact(b, name); va == vb {
			t.Errorf("%s must vary per request, got %q twice", name, va)
		}
	}
	// 每请求级与会话级解耦
	if getHeaderExact(a, "X-Request-ID") == getHeaderExact(a, "X-Trace-ID") {
		t.Error("X-Request-ID must be decoupled from session-scoped X-Trace-ID")
	}
	// 形态：会话请求 ID 为 32 位小写 hex
	if v := getHeaderExact(a, "X-Conversation-Request-ID"); len(v) != 32 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("X-Conversation-Request-ID = %q, want 32 lowercase hex", v)
	}
}

// 不同会话必须得到不同的会话级链路 ID（不得跨会话复用）。
func TestSessionScopedLinkIDsDifferAcrossConversations(t *testing.T) {
	a := upstreamHeadersForSession(t, map[string]string{"X-Conversation-ID": "04bad56e-08d5-4647-9c3c-28e12897c1af"}, false)
	b := upstreamHeadersForSession(t, map[string]string{"X-Conversation-ID": "633ef566-21df-41b5-a796-c57fda9e29a6"}, false)

	if x, y := getHeaderExact(a, "X-Conversation-Request-ID"), getHeaderExact(b, "X-Conversation-Request-ID"); x == y {
		t.Errorf("distinct conversations must not share X-Conversation-Request-ID: %q", x)
	}
	if x, y := getHeaderExact(a, "X-Conversation-ID"), getHeaderExact(b, "X-Conversation-ID"); x == y {
		t.Errorf("distinct conversations must not share X-Conversation-ID: %q", x)
	}
	if x, y := getHeaderExact(a, "X-Trace-ID"), getHeaderExact(b, "X-Trace-ID"); x == y {
		t.Errorf("distinct conversations must not share X-Trace-ID: %q", x)
	}
}

// 下游未提供会话标识时不得臆造会话关联：会话级 ID 退回每请求新值。
func TestNoConversationKeyFallsBackToPerRequestIDs(t *testing.T) {
	a := upstreamHeadersForSession(t, nil, false)
	b := upstreamHeadersForSession(t, nil, false)

	for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-Trace-ID"} {
		if va, vb := getHeaderExact(a, name), getHeaderExact(b, name); va == vb {
			t.Errorf("%s must vary per request when no conversation key is present, got %q twice", name, va)
		}
	}
	// 形态仍须自洽：X-Conversation-ID 为带连字符 UUID，其余为 32 位 hex
	if cid := getHeaderExact(a, "X-Conversation-ID"); len(cid) != 36 || strings.Count(cid, "-") != 4 {
		t.Errorf("X-Conversation-ID = %q, want dashed UUID", cid)
	}
	if x, y, z := getHeaderExact(a, "X-Conversation-Request-ID"), getHeaderExact(a, "X-Root-Request-ID"), getHeaderExact(a, "X-Trace-ID"); x != y || y != z {
		t.Errorf("session-scoped ids must still be mutually equal: %q %q %q", x, y, z)
	}
}

// 下游只带 X-Conversation-Request-ID（无 X-Conversation-ID）时：
// 会话级请求 ID 原样采用（保证三者同值），并补一个形态自洽的会话 ID。
func TestConversationRequestIDOnlyIsAdoptedVerbatim(t *testing.T) {
	const crid = "96fa0aae82d9df0aa4238fc12e42afc0"
	hdrs := map[string]string{"X-Conversation-Request-ID": crid}
	a := upstreamHeadersForSession(t, hdrs, false)
	b := upstreamHeadersForSession(t, hdrs, false)

	if v := getHeaderExact(a, "X-Conversation-Request-ID"); v != crid {
		t.Errorf("X-Conversation-Request-ID = %q, want client-provided %q", v, crid)
	}
	// 采用客户端值后三者仍须同值
	if x, y, z := getHeaderExact(a, "X-Conversation-Request-ID"), getHeaderExact(a, "X-Root-Request-ID"), getHeaderExact(a, "X-Trace-ID"); x != y || y != z {
		t.Errorf("session-scoped ids must be equal: %q %q %q", x, y, z)
	}
	// 会话 ID 缺失时补齐，且形态为带连字符 UUID、会话内稳定、非客户端未提供的空值
	cid := getHeaderExact(a, "X-Conversation-ID")
	if len(cid) != 36 || strings.Count(cid, "-") != 4 {
		t.Errorf("X-Conversation-ID = %q, want dashed UUID", cid)
	}
	if cid != getHeaderExact(b, "X-Conversation-ID") {
		t.Errorf("derived X-Conversation-ID must be stable: %q vs %q", cid, getHeaderExact(b, "X-Conversation-ID"))
	}
	// 每请求级仍须变化
	if getHeaderExact(a, "X-Request-ID") == getHeaderExact(b, "X-Request-ID") {
		t.Error("X-Request-ID must vary per request")
	}
}

// 调试开关打开且请求经过审计中间件时，出站链路头的值仍只由会话作用域对齐写入。
// 中间件在没有客户端 X-Trace-ID 时会生成带连字符的 UUID，并把它记进调试上下文；
// 旧逻辑再用这个值覆盖上游头，于是 X-Trace-ID 不再等于由会话键派生的 32 位 hex。
// X-Parent-Request-ID 不是客户端指纹，同样不得因为调试打开而出现在上游请求上。
// 键名大小写不在此断言：httptest 的入站解析会把所有头规范化，线上字节由
// captureUpstreamWire 的指纹回归单独核对。
func TestDebugLoggingDoesNotRewriteUpstreamTraceHeaders(t *testing.T) {
	old := cfg.DebugEnabled
	cfg.DebugEnabled = true
	sink := &debugJSONSink{writer: io.Discard}
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = sink
	debugSinkMu.Unlock()
	t.Cleanup(func() {
		cfg.DebugEnabled = old
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	const conv = "04bad56e-08d5-4647-9c3c-28e12897c1af"
	h := upstreamHeadersForSession(t, map[string]string{
		"X-Conversation-ID": conv,
	}, true)

	traceID := getHeaderExact(h, "X-Trace-ID")
	if len(traceID) != 32 || strings.Trim(traceID, "0123456789abcdef") != "" {
		t.Errorf("X-Trace-ID = %q, want 32 lowercase hex derived from the conversation", traceID)
	}
	if strings.Contains(traceID, "-") {
		t.Errorf("X-Trace-ID = %q, debug middleware UUID leaked onto the upstream request", traceID)
	}

	if x, y := getHeaderExact(h, "X-Conversation-Request-ID"), getHeaderExact(h, "X-Root-Request-ID"); x != traceID || y != traceID {
		t.Errorf("session-scoped ids diverged under debug: request=%q root=%q trace=%q", x, y, traceID)
	}
	if got := getHeaderExact(h, "X-Parent-Request-ID"); got != "" {
		t.Errorf("X-Parent-Request-ID = %q, debug must not add a non-client header", got)
	}
	if tp := getHeaderExact(h, "traceparent"); !strings.HasPrefix(tp, "00-"+traceID+"-") {
		t.Errorf("traceparent %q not linked to X-Trace-ID %q", tp, traceID)
	}
}

// 下游只带链路 ID 的一部分时，网关必须按客户端不变量补齐同组伙伴，
// 而不能让其余头另生成值 —— 后者会产出客户端不可能产生的组合（比缺失更显眼）。
func TestPartialLinkHeadersAreCompletedCoherently(t *testing.T) {
	const (
		traceID = "a2a2a25c3094ee6e6203d9e014ba6e8c"
		spanID  = "61cc1c019243bd0c"
		reqID   = "8e48c9ed463d48d08dce1185ed92b200"
	)

	t.Run("only traceparent", func(t *testing.T) {
		h := upstreamHeadersForSession(t, map[string]string{
			"traceparent": "00-" + traceID + "-" + spanID + "-01",
		}, false)
		// trace 组：下游 traceparent 的 trace 段必须传播到全部同义头
		for _, n := range []string{"X-Trace-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-B3-TraceId"} {
			if got := getHeaderExact(h, n); got != traceID {
				t.Errorf("%s = %q, want %q (from traceparent)", n, got, traceID)
			}
		}
		// span 组：traceparent 的 span 段必须与 b3 / X-B3-SpanId 一致
		if got := getHeaderExact(h, "X-B3-SpanId"); got != spanID {
			t.Errorf("X-B3-SpanId = %q, want %q (from traceparent)", got, spanID)
		}
		if b3 := getHeaderExact(h, "b3"); !strings.HasPrefix(b3, traceID+"-"+spanID+"-") {
			t.Errorf("b3 = %q, want trace/span from traceparent (%s/%s)", b3, traceID, spanID)
		}
	})

	t.Run("only X-Request-ID", func(t *testing.T) {
		h := upstreamHeadersForSession(t, map[string]string{"X-Request-ID": reqID}, false)
		if got := getHeaderExact(h, "X-Request-ID"); got != reqID {
			t.Errorf("X-Request-ID = %q, want %q", got, reqID)
		}
		// 客户端保证二者相等；只给其一时也必须一致
		if got := getHeaderExact(h, "X-Conversation-Message-ID"); got != reqID {
			t.Errorf("X-Conversation-Message-ID = %q, want %q (must equal X-Request-ID)", got, reqID)
		}
	})

	t.Run("only b3", func(t *testing.T) {
		parent := "298ea3b5a5796f21"
		h := upstreamHeadersForSession(t, map[string]string{
			"b3": traceID + "-" + spanID + "-1-" + parent,
		}, false)
		if got := getHeaderExact(h, "X-Trace-ID"); got != traceID {
			t.Errorf("X-Trace-ID = %q, want %q (from b3)", got, traceID)
		}
		if got := getHeaderExact(h, "X-B3-SpanId"); got != spanID {
			t.Errorf("X-B3-SpanId = %q, want %q (from b3)", got, spanID)
		}
		if got := getHeaderExact(h, "X-B3-ParentSpanId"); got != parent {
			t.Errorf("X-B3-ParentSpanId = %q, want %q (from b3)", got, parent)
		}
		if tp := getHeaderExact(h, "traceparent"); !strings.HasPrefix(tp, "00-"+traceID+"-"+spanID+"-") {
			t.Errorf("traceparent = %q, want trace/span from b3 (%s/%s)", tp, traceID, spanID)
		}
	})

	// 下游未提供任何链路头时，合成值内部也必须自洽
	t.Run("none", func(t *testing.T) {
		h := upstreamHeadersForSession(t, nil, false)
		trace := getHeaderExact(h, "X-Trace-ID")
		span := getHeaderExact(h, "X-B3-SpanId")
		if got := getHeaderExact(h, "X-B3-TraceId"); got != trace {
			t.Errorf("X-B3-TraceId = %q, want %q", got, trace)
		}
		if tp := getHeaderExact(h, "traceparent"); !strings.HasPrefix(tp, "00-"+trace+"-"+span+"-") {
			t.Errorf("traceparent = %q, want %s/%s", tp, trace, span)
		}
		if b3 := getHeaderExact(h, "b3"); !strings.HasPrefix(b3, trace+"-"+span+"-") {
			t.Errorf("b3 = %q, want %s/%s", b3, trace, span)
		}
	})
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
// readWireRequest 从一个连接读满一个 HTTP 请求的原始字节（含 body，按 Content-Length）。
func readWireRequest(conn net.Conn) string {
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
	return string(buf)
}

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
		rawCh <- readWireRequest(conn)
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
		"Content-Encoding",
		"x-requested-with",
		"x-codebuddy-request",
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
//
// 国际站请求体按 product.json 的 RequestBodyGzip 压缩（客户端 GzipRequestProcessor 同款），
// 故先按 Content-Encoding 还原再比对正文。
func TestUpstreamWireBodyMatchesClientEncoding(t *testing.T) {
	// 含 HTML 敏感字符的 system prompt，贴近客户端真实 harness 正文
	body := `{"model":"deepseek-v3-2-volc","messages":[{"role":"system","content":"a <user_query> b & c > d"},{"role":"user","content":"hi"}]}`
	raw := captureUpstreamWire(t, body)
	got := wireBody(t, raw)

	if strings.Contains(raw, "\r\nContent-Encoding: gzip\r\n") {
		zr, err := gzip.NewReader(strings.NewReader(got))
		if err != nil {
			t.Fatalf("body declared gzip but is not: %v", err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip wire body: %v", err)
		}
		got = string(plain)
	} else {
		t.Error("intl model request body must declare Content-Encoding: gzip")
	}

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

// 登录链路第一步（auth/state）的线上字节必须与客户端逐字一致：键名大小写、必需头、
// 以及「控制面不该出现的头」（traceparent / X-IDE-* / x-stainless-* / 会话链路头）。
// 这一跳的差异同样只在比对线上字节时可见，故用原始 TCP 监听器而非 httptest。
func TestLoginWireHeadersMatchClient(t *testing.T) {
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
		rawCh <- readWireRequest(conn)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 52\r\n\r\n" +
			`{"code":0,"data":{"state":"s","authUrl":"http://x"}}`))
	}()

	origBase, origPortal := profileINTL.Base, profileINTL.PortalOrigin
	profileINTL.Base, profileINTL.PortalOrigin = "http://"+ln.Addr().String(), "http://"+ln.Addr().String()
	defer func() { profileINTL.Base, profileINTL.PortalOrigin = origBase, origPortal }()
	origClient := cfg.HttpClient
	initHTTPClient() // 走生产 transport（DisableCompression 等指纹相关设置）
	defer func() { cfg.HttpClient = origClient }()

	if _, _, err := doJSON(cfg.HttpClient, http.MethodPost, profileINTL.authStateURL(),
		loginStateHeaders(&profileINTL), bytes.NewReader([]byte("{}"))); err != nil {
		t.Fatalf("auth/state: %v", err)
	}

	var raw string
	select {
	case raw = <-rawCh:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received request")
	}

	if got, want := strings.SplitN(raw, "\r\n", 2)[0],
		"POST /v2/plugin/auth/state?platform=workbuddy-ai HTTP/1.1"; got != want {
		t.Errorf("request line = %q, want %q", got, want)
	}
	// 客户端实测逐字大小写：axios 族头保持规范化形态，trace 族与 X-Domain 亦然。
	for _, name := range []string{
		"Accept", "Content-Type", "Accept-Encoding", "User-Agent", "X-Product",
		"X-Requested-With", "X-Domain",
		"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
		"X-Trace-ID", "X-Request-ID", "b3", "X-B3-TraceId", "X-B3-ParentSpanId",
		"X-B3-SpanId", "X-B3-Sampled",
	} {
		if !strings.Contains(raw, "\r\n"+name+": ") {
			t.Errorf("wire is missing header with exact casing %q", name)
		}
	}
	for _, bad := range []string{
		"traceparent", "X-IDE-Type", "X-Ide-Type", "X-Agent-Intent", "X-Private-Data",
		"x-codebuddy-request", "X-Codebuddy-Request", "X-Conversation-ID",
		"X-Conversation-Id", "x-stainless-runtime", "X-Stainless-Runtime",
		"X-Requested-With-", "X-Domain-",
	} {
		if strings.Contains(raw, "\r\n"+bad+": ") {
			t.Errorf("control-plane wire must not carry %q", bad)
		}
	}
	// 无父 span：b3 末段为空、X-B3-ParentSpanId 为空值
	if b3 := wireHeader(t, raw, "b3"); !strings.HasSuffix(b3, "-1-") {
		t.Errorf("b3 = %q, want 4 segments with empty parent", b3)
	}
	if p, ok := wireHeaderOK(raw, "X-B3-ParentSpanId"); !ok || p != "" {
		t.Errorf("X-B3-ParentSpanId = %q (present=%v), want empty value", p, ok)
	}
	// X-Request-ID 由 injectRequestIdHeader 取 X-Trace-ID
	if tid, rid := wireHeader(t, raw, "X-Trace-ID"), wireHeader(t, raw, "X-Request-ID"); tid != rid {
		t.Errorf("X-Request-ID %q != X-Trace-ID %q", rid, tid)
	}
	t.Logf("login wire request line: %s", strings.SplitN(raw, "\r\n", 2)[0])
}

// wireHeader 取出线上原始头的值（键名区分大小写）。
func wireHeader(t *testing.T, raw, name string) string {
	t.Helper()
	v, ok := wireHeaderOK(raw, name)
	if !ok {
		t.Fatalf("wire is missing header %q", name)
	}
	return v
}

func wireHeaderOK(raw, name string) (string, bool) {
	for _, line := range strings.Split(raw, "\r\n") {
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return v, true
		}
	}
	return "", false
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
