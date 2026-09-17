package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skip2/go-qrcode"
)

const (
	version = "1.11.1"

	// 状态快照文件名：serve 后台周期写入，monitor 前台命令实时读取展示
	statusSnapshotFile = "workbuddy-status.json"
	logDir             = "logs"

	// defaultSystemPrompt 是当客户端首条消息不是 system 时注入的保底系统提示，
	// 用于满足腾讯上游「首条消息必须是 system prompt」的硬性要求 (code 11128)。
	defaultSystemPrompt = "You are a helpful assistant."
)

// -----------------------------------------------------------------------------
// 上游站点 Profile（国内站 / 国际站）
//
// 国际站 www.workbuddy.ai 与国内站 copilot.tencent.com 走同一套 /v2/plugin/* 协议
// （实测：auth/state、auth/token、login/account、auth/token/refresh、chat/completions
// 的路径与响应包络完全一致），差异仅在：
//   - 上游域名与 Web Origin（国际站位于腾讯 EdgeOne 国际 CDN）
//   - 登录 platform 参数（workbuddy-ai 而非 VSCode），登录在浏览器内完成（邮箱/验证码/SSO）
//   - 等待授权期间 auth/token 轮询返回 code 11217 (login ing...)
// 每个账号凭据文件通过 edition 字段记录所属站点，账号池支持国内/国际混挂轮询。
// -----------------------------------------------------------------------------

type upstreamProfile struct {
	Key             string        // 存储于凭据文件 edition 字段的站点标识
	Label           string        // 控制台展示名
	Base            string        // 上游 API 基础地址
	Platform        string        // auth/state 的 platform 参数
	PlatformName    string        // X-IDE-Type / X-IDE-Name（宿主产品标识）
	PlatformVersion string        // X-IDE-Version（宿主产品版本）
	CliVersion      string        // X-IDE-Version 回退值 + UA 尾段（bundled CLI 版本）
	Product         string        // X-Product
	PortalOrigin    string        // 各站 Web 控制台源（仅用于 billing/额度等 Web API 的 URL 拼装，不作为请求头发送）
	LoginTTL        time.Duration // login 命令等待授权完成的超时
}

// 客户端身份常量：与真实客户端实测流量对齐（详见 README「客户端指纹对齐」）。
// 这些取值来源于 WorkBuddyAI desktop 5.5.2 + bundled CLI 2.137.1 的真实抓包，
// 而非臆造——任何自造字段（如 X-Client-ID）都会形成可静态识别的机器特征。
const (
	// clientDomain 是上游请求携带的 X-Domain 默认值（账号未记录 domain 时使用）。
	clientDomain = "www.workbuddy.ai"
	// agentIntent 对应客户端 session meta 的 codebuddy.ai/mode，默认 craft。
	agentIntent = "craft"
	// agentPurpose 对应主会话（非子代理）的 agentPurpose。
	agentPurpose = "conversation"
	// agentType 为主会话 agent 类型。
	agentType = "main"
)

var (
	profileCN = upstreamProfile{
		Key: "cn", Label: "国内站",
		Base:     "https://copilot.tencent.com",
		Platform: "VSCode", PlatformName: "CodeBuddy", PlatformVersion: "5.5.2",
		CliVersion: "2.137.1", Product: "SaaS",
		PortalOrigin: "https://www.codebuddy.cn",
		LoginTTL:     5 * time.Minute,
	}
	profileINTL = upstreamProfile{
		Key: "intl", Label: "国际站",
		Base:     "https://www.workbuddy.ai",
		Platform: "workbuddy-ai", PlatformName: "WorkBuddy", PlatformVersion: "5.5.2",
		CliVersion: "2.137.1", Product: "SaaS",
		PortalOrigin: "https://www.workbuddy.ai",
		LoginTTL:     15 * time.Minute, // 浏览器内登录（邮箱/验证码/SSO）比扫码慢，放宽超时
	}
)

// profileForEdition 根据凭据文件中的 edition 标识返回上游站点参数；空值/未知值回退国内站。
func profileForEdition(edition string) *upstreamProfile {
	switch strings.ToLower(strings.TrimSpace(edition)) {
	case "intl", "international", "global", "workbuddy.ai":
		return &profileINTL
	default:
		return &profileCN
	}
}

func (p *upstreamProfile) authStateURL() string {
	return p.Base + "/v2/plugin/auth/state?platform=" + url.QueryEscape(p.Platform)
}

func (p *upstreamProfile) loginAcctURL(state string) string {
	return p.Base + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
}

func (p *upstreamProfile) authTokenURL(state string) string {
	return p.Base + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
}

func (p *upstreamProfile) tokenRefreshURL() string { return p.Base + "/v2/plugin/auth/token/refresh" }

func (p *upstreamProfile) chatURL() string { return p.Base + "/v2/chat/completions" }

func (p *upstreamProfile) quotaSummaryURL() string {
	return p.PortalOrigin + "/billing/meter/get-user-resource-summary"
}

func (p *upstreamProfile) dailyCheckinURL() string {
	return p.PortalOrigin + "/v2/billing/meter/daily-checkin"
}

// -----------------------------------------------------------------------------
// 数据结构定义
// -----------------------------------------------------------------------------

type StoredAuth struct {
	Auth    StoredTokens  `json:"auth"`
	Account StoredAccount `json:"account"`
	Edition string        `json:"edition,omitempty"` // 站点标识：cn（国内站，默认）| intl（国际站 www.workbuddy.ai）
}

type StoredTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type StoredAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

type quotaPackage struct {
	CycleTotalCapacity  string `json:"CycleTotalCapacity"`
	CycleUsedCapacity   string `json:"CycleUsedCapacity"`
	CycleRemainCapacity string `json:"CycleRemainCapacity"`
}

type quotaSummaryData struct {
	Packages   []quotaPackage `json:"Packages"`
	IsPaidUser bool           `json:"IsPaidUser"`
}

// -----------------------------------------------------------------------------
// 全局状态与配置
// -----------------------------------------------------------------------------

type Config struct {
	Addr            string
	Port            int
	AuthFile        string
	AuthDir         string
	AuthExplicit    bool // 用户是否显式指定了 -auth（未指定时自动扫描目录下所有 workbuddy*.json）
	LoginIntl       bool // login -intl：登录国际站 (www.workbuddy.ai，浏览器内完成登录)
	APIKey          string
	ProxyURL        string
	Verbose         bool
	ReloadInterval  int    // 账号池热加载扫描间隔（秒），0 关闭
	MonitorInterval int    // monitor 状态刷新间隔（秒）
	LogFile         string // monitor 附加展示的日志文件路径
	JournalService  string // monitor 附加展示的 systemd 服务名（journalctl -u）
	LogLines        int    // monitor 展示的最近日志行数
	ModelsRefresh   int    // 官方模型目录刷新间隔（分钟），0 关闭
	ProbeModels     string // probe 专用：逗号分隔的模型列表
	ProbeLimit      int    // probe 专用：未显式指定模型时的取用数量
	HttpClient      *http.Client
}

// Account 表示一个 CodeBuddy 账号凭据及其运行时状态。
type Account struct {
	Path           string                        // 凭据文件路径
	Auth           *StoredAuth                   // 凭据内容（失效标记恢复时可能为 nil）
	CooldownUntil  time.Time                     // 冷却截止时间（429 频率限制后自动屏蔽到该时间）
	CooldownMsg    string                        // 触发冷却的原因/上游提示
	Disabled       bool                          // 授权失效（token 过期且无法刷新/已撤销）：禁止调度
	DisabledReason string                        // 失效原因，用于控制台展示
	Nickname       string                        // 失效标记持久化字段：昵称（Auth 为 nil 时使用）
	UID            string                        // 失效标记持久化字段：UID
	Edition        string                        // 站点标识（cn/intl）：失效标记恢复时 Auth 为 nil 也可见
	QuotaTotal     float64                       // 最近一次额度查询返回的周期总额度
	QuotaUsed      float64                       // 最近一次额度查询返回的已用额度
	QuotaRemaining float64                       // 最近一次额度查询返回的剩余额度
	IsPaidUser     bool                          // 是否为付费用户
	QuotaKnown     bool                          // 是否已成功获取过额度
	QuotaExhausted bool                          // 已确认额度为 0；额度扫描发现恢复后自动解除
	ModelStates    map[string]*modelRuntimeState // 按模型隔离的成本、限流和额度阻断状态
	fingerprint    string                        // 凭据文件变更指纹（mtime+size，凭据热加载用）
	lock           sync.Mutex                    // 单账号串行锁（防止同账号并发触发 11128）
}

const (
	modelCostUnknown = "unknown"
	modelCostFree    = "free"
	modelCostPaid    = "paid"
	modelProbeDelay  = 5 * time.Minute

	// modelFreeMinTokens 判定“免费模型”所需的最小样本：上游对极小请求也可能记
	// credit=0（例如探针请求），那不是真正的免费，不能据此让零余额账号请求收费模型。
	modelFreeMinTokens = 100
)

type modelRuntimeState struct {
	CostClass     string
	CooldownUntil time.Time
	QuotaBlocked  bool
	NextProbeAt   time.Time
	LastReason    string
	ObservedAt    time.Time
}

type accountSelectionKind string

const (
	selectionNormal         accountSelectionKind = "normal"
	selectionFreeExhausted  accountSelectionKind = "free_exhausted"
	selectionProbeExhausted accountSelectionKind = "probe_exhausted"
)

// Profile 返回该账号对应的上游站点参数（国内站/国际站）。
func (acc *Account) Profile() *upstreamProfile {
	if acc.Auth != nil {
		return profileForEdition(acc.Auth.Edition)
	}
	return profileForEdition(acc.Edition)
}

// disabledMarker 是授权失效账号的持久化标记（凭据文件删除后用于控制台提示重新登录）。
type disabledMarker struct {
	Path       string `json:"path"`
	Reason     string `json:"reason"`
	DisabledAt int64  `json:"disabledAt"`
	Nickname   string `json:"nickname,omitempty"`
	UID        string `json:"uid,omitempty"`
	Edition    string `json:"edition,omitempty"` // 站点标识（cn/intl），用于控制台展示
}

// markerPath 返回与凭据文件同目录的失效标记文件路径。
func markerPath(authPath string) string {
	return authPath + ".disabled"
}

var (
	cfg        Config
	authLock   sync.RWMutex
	currAuth   *StoredAuth
	reqCounter uint64

	accountMu sync.Mutex // 账号池保护锁
	accounts  []*Account // 多账号池（单账号时长度为 1，行为与旧版完全一致）
	rrIndex   int        // 轮询游标

	quotaScanTrigger = make(chan struct{}, 1)
	checkinTrigger   = make(chan struct{}, 1)
	dailyCheckinMu   sync.Mutex
)

// -----------------------------------------------------------------------------
// 主入口与命令行控制
// -----------------------------------------------------------------------------

func main() {
	if len(os.Args) > 1 {
		first := os.Args[1]
		if first == "help" || first == "-h" || first == "--help" {
			printHelp()
			return
		}
		if first == "version" || first == "-v" || first == "--version" {
			fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
			return
		}
	}

	command := "serve"
	args := os.Args[1:]
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		command = os.Args[1]
		args = os.Args[2:]
	}

	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1", "网关监听地址")
	fs.IntVar(&cfg.Port, "port", 8317, "网关监听端口")
	fs.StringVar(&cfg.AuthFile, "auth", "workbuddy.json", "凭据存储文件路径（支持逗号分隔多个文件实现多账号）")
	fs.StringVar(&cfg.AuthDir, "auth-dir", "", "凭据目录：自动加载目录下所有 workbuddy*.json 作为多账号池")
	fs.StringVar(&cfg.APIKey, "api-key", "", "可选：访问网关所需的 API Key (客户端 Bearer 校验)")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "可选：上游请求代理 (如 http://127.0.0.1:7890)")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "输出详细调试日志")
	fs.BoolVar(&cfg.LoginIntl, "intl", false, "login 专用：登录国际站 (www.workbuddy.ai，浏览器内完成登录)；默认登录国内站")
	fs.IntVar(&cfg.MonitorInterval, "interval", 3, "monitor 状态刷新间隔（秒）")
	fs.IntVar(&cfg.ReloadInterval, "reload-interval", 5, "账号池热加载扫描间隔（秒），0 关闭：运行期自动发现新增/更新/删除的凭据文件，免重启")
	fs.StringVar(&cfg.LogFile, "logfile", "", "monitor 附加跟随的日志文件路径（如 -logfile /var/log/workbuddy-gateway.log）")
	fs.StringVar(&cfg.JournalService, "journal", "", "monitor 附加跟随的 systemd 服务名（Linux 下用 journalctl -u <服务> -f 跟随）")
	fs.IntVar(&cfg.LogLines, "lines", 15, "monitor 每次刷新展示的最近日志行数")
	fs.IntVar(&cfg.ModelsRefresh, "models-refresh", 60, "官方模型目录刷新间隔（分钟），0 关闭")
	fs.StringVar(&cfg.ProbeModels, "models", "", "probe 专用：逗号分隔的待探测模型（默认取目录前几个）")
	fs.IntVar(&cfg.ProbeLimit, "limit", 5, "probe 专用：未指定 -models 时探测的模型数量上限")
	_ = fs.Parse(args)

	// 检测 -auth 是否被显式指定：
	// 若未指定 -auth 且未指定 -auth-dir，则自动扫描当前目录下所有 workbuddy*.json 组成账号池，
	// 这样把多个凭据文件放进工作目录即可自动多账号，无需手写参数。
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auth" {
			cfg.AuthExplicit = true
		}
	})

	// 初始化 HTTP 客户端
	initHTTPClient()
	closeLog := initFileLogging(command)
	defer closeLog()

	switch command {
	case "serve", "run", "start":
		runServe()
	case "login":
		runLogin()
	case "status":
		runStatus()
	case "refresh":
		runRefresh()
	case "monitor":
		runMonitor()
	case "probe":
		runProbe()
	case "version", "-v", "--version":
		fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
	default:
		fmt.Printf("未知命令: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

// initFileLogging 将运行日志同时写入控制台和按日期命名的项目日志文件。
func initFileLogging(command string) func() {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("[Log] 无法创建日志目录 %s，将仅输出到控制台: %v", logDir, err)
		return func() {}
	}
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("[Log] 无法打开日志文件 %s，将仅输出到控制台: %v", path, err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("[Log] 审计日志已启用，文件=%s，命令=%s，版本=%s", path, command, version)
	return func() { _ = f.Close() }
}

func printHelp() {
	fmt.Println(`WorkBuddy Local Gateway - 轻量化跨平台本地 AI 网关
将腾讯 CodeBuddy / 混元反代为标准 OpenAI 协议，支持直接透传任意模型到上游。
支持多账号池：轮询使用多个账号均衡额度；账号触发 429 频率限制后自动冷却屏蔽，
由其余账号代偿，冷却到期自动恢复。

用法:
  workbuddy-gateway [command] [options]

命令:
  serve       启动本地网关 (默认操作)
  login       登录并获取/更新凭据：国内站微信扫码；-intl 登录国际站 (浏览器内完成)
  status      查看账号池状态（含站点、冷却状态与过期时间）
  refresh     手动立即刷新所有账号访问令牌 (Access Token)
  monitor     前台实时监控：周期刷新展示账号状态 + 最近日志 (Ctrl+C 退出)
  probe       主动探测账号对指定模型的免费/收费属性（需 serve 正在运行）
  version     查看版本信息
  help        查看帮助说明

参数选项:
  -addr <ip>        网关监听地址 (默认: 127.0.0.1)
  -port <port>      网关监听端口 (默认: 8317)
  -auth <path>      凭据文件路径；支持逗号分隔多个文件实现多账号
                    (默认: 自动发现当前目录下所有 workbuddy*.json)
  -auth-dir <dir>   凭据目录：自动加载目录下所有 workbuddy*.json 作为账号池
  -intl             login 专用：登录国际站 www.workbuddy.ai（浏览器内完成登录）
  -reload-interval <sec>
                    账号池热加载扫描间隔（默认 5 秒，0 关闭）：运行期自动发现
                    新增/更新/删除的凭据文件，免重启生效
  -models-refresh <min>
                    官方模型目录刷新间隔（默认 60 分钟，0 关闭）

probe 选项:
  -auth <path>      只探测指定凭据文件（文件名或路径均可）；默认探测全部账号
  -models <m1,m2>   指定要探测的模型；默认取模型目录前几个
  -limit <n>        未指定 -models 时探测的模型数量（默认 5，上限 50）
  -addr/-port       需与运行中的 serve 一致；-api-key 启用时 probe 会自动携带
  -api-key <key>    设置后，调用网关必须携带 Bearer <key> 鉴权
  -proxy <url>      设置上游转发代理 (例如 http://127.0.0.1:7890 或 socks5://...)
  -verbose          输出详细调试日志 (请求/响应体)

monitor 选项:
  -interval <sec>   状态刷新间隔秒数 (默认: 3)
  -journal <svc>    同时展示 systemd 服务最近日志 (Linux, 如 -journal workbuddy-gateway)
  -logfile <path>   同时展示指定日志文件最近内容 (如 -logfile /var/log/wb.log)
  -lines <n>        每次刷新展示的最近日志行数 (默认: 15)

多账号说明:
  # 登录第二个账号（保存到不同文件）
  workbuddy-gateway login -auth workbuddy2.json

  # 登录国际站账号（www.workbuddy.ai，浏览器内完成登录）
  workbuddy-gateway login -intl -auth workbuddy-intl.json

  # 自动发现：把多个凭据文件放进工作目录即可自动多账号（无需任何参数）
  workbuddy-gateway serve        # 自动加载 ./workbuddy*.json

  # 启动时指定多个凭据文件（轮询 + 429 自动冷却代偿；国内/国际可混挂）
  workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

  # 或使用目录模式：目录内所有 workbuddy*.json 自动组成账号池
  workbuddy-gateway serve -auth-dir ./auths

  # 运行期新增/更新/删除凭据文件会自动热加载（默认每 5 秒），无需重启 serve

示例:
  # 首次使用扫码登录（国内站）
  workbuddy-gateway login

  # 登录国际站（www.workbuddy.ai）
  workbuddy-gateway login -intl

  # 启动本地网关 (监听 127.0.0.1:8317)
  workbuddy-gateway serve

  # 启动网关并指定端口和代理
  workbuddy-gateway serve -port 9000 -proxy http://127.0.0.1:7890`)
}

func initHTTPClient() {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
		// 真实客户端（Node/Electron + openai SDK）不发送 Accept-Encoding，而 Go transport
		// 默认会自动补 "Accept-Encoding: gzip"。该差异属可静态识别的指纹偏差，故禁用自动压缩。
		// 副作用仅为不再自动解压上游响应——上游因未被请求压缩也不会压缩，行为与真实客户端一致。
		DisableCompression: true,
	}
	if cfg.ProxyURL != "" {
		pURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			log.Fatalf("错误: 无效的代理地址 %s: %v", cfg.ProxyURL, err)
		}
		transport.Proxy = http.ProxyURL(pURL)
	}
	cfg.HttpClient = &http.Client{
		Timeout:   180 * time.Second,
		Transport: transport,
		Jar:       jar,
	}
}

// -----------------------------------------------------------------------------
// 凭据加载、保存与自动续期
// -----------------------------------------------------------------------------

func loadAuth() (*StoredAuth, error) {
	authLock.RLock()
	if currAuth != nil {
		defer authLock.RUnlock()
		return currAuth, nil
	}
	authLock.RUnlock()

	data, err := os.ReadFile(cfg.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败 (%s): %w", cfg.AuthFile, err)
	}
	var sa StoredAuth
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("解析凭据文件失败: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("凭据文件中缺少 AccessToken")
	}

	authLock.Lock()
	currAuth = &sa
	authLock.Unlock()
	return &sa, nil
}

func saveAuth(sa *StoredAuth) error {
	authLock.Lock()
	currAuth = sa
	authLock.Unlock()

	dir := filepath.Dir(cfg.AuthFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.AuthFile, data, 0600); err != nil {
		return err
	}
	clearDisabledMarker(cfg.AuthFile)
	return nil
}

// saveAuthTo 将凭据写入指定路径（多账号模式使用）。
// 写入成功后清除该路径的失效标记（表示账号已重新登录）。
func saveAuthTo(path string, sa *StoredAuth) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	clearDisabledMarker(path)
	return nil
}

// loadAccountFile 从指定路径读取并解析凭据（不修改全局缓存）。
func loadAccountFile(path string) (*StoredAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败 (%s): %w", path, err)
	}
	var sa StoredAuth
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("解析凭据文件失败 (%s): %w", path, err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("凭据文件缺少 AccessToken (%s)", path)
	}
	return &sa, nil
}

// isCredentialFile 判断自动发现模式下文件名是否为凭据文件。
// 排除运行时产物：workbuddy-status.json（monitor 状态快照）等。
func isCredentialFile(name string) bool {
	if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
		return false
	}
	return name != statusSnapshotFile
}

// collectConfiguredAuthPaths 解析应加载的凭据路径列表：
//  1. -auth-dir 指定目录 → 目录下所有 workbuddy*.json
//  2. 显式 -auth → 逗号分隔的凭据文件列表（保持用户顺序）
//  3. 均未指定（自动发现模式）→ 扫描当前目录下所有 workbuddy*.json，
//     这样把多个凭据文件放进工作目录即可自动组成多账号池；若一个都没有则回退默认 cfg.AuthFile
func collectConfiguredAuthPaths() []string {
	var paths []string

	if cfg.AuthDir != "" {
		entries, err := os.ReadDir(cfg.AuthDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !isCredentialFile(e.Name()) {
					continue
				}
				paths = append(paths, filepath.Join(cfg.AuthDir, e.Name()))
			}
			sort.Strings(paths)
		}
		return paths
	}

	if cfg.AuthExplicit {
		for _, p := range strings.Split(cfg.AuthFile, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
		return paths
	}

	// 自动发现模式：扫描当前工作目录（systemd WorkingDirectory 即凭据目录）
	entries, err := os.ReadDir(".")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !isCredentialFile(e.Name()) {
				continue
			}
			paths = append(paths, e.Name())
		}
		sort.Strings(paths)
	}
	if len(paths) == 0 {
		// 无任何凭据文件时回退默认路径，便于给出"请先 login"的友好提示
		paths = append(paths, cfg.AuthFile)
	}
	return paths
}

// loadAccounts 构建账号池。
// -auth 支持逗号分隔多个凭据文件；-auth-dir 自动加载目录下所有 workbuddy*.json；
// 两者都未指定时自动发现当前目录下所有 workbuddy*.json（多账号免参数）。
func loadAccounts() error {
	accountMu.Lock()
	defer accountMu.Unlock()

	accounts = nil
	rrIndex = 0

	for _, p := range collectConfiguredAuthPaths() {
		sa, err := loadAccountFile(p)
		if err != nil {
			log.Printf("[Auth] 跳过无效凭据文件 %s: %v", p, err)
			continue
		}
		fp := ""
		if st, statErr := os.Stat(p); statErr == nil {
			fp = accountFingerprint(st)
		}
		accounts = append(accounts, &Account{Path: p, Auth: sa, Edition: profileForEdition(sa.Edition).Key, fingerprint: fp})
	}

	// 恢复持久化的失效账号标记（凭据文件已删除，仅供控制台提示重新登录）
	loadDisabledMarkers()
	restoreAccountRuntimeStateLocked()

	if len(accounts) == 0 {
		return fmt.Errorf("未找到有效凭据，请先执行 login 命令扫码登录")
	}
	return nil
}

// restoreAccountRuntimeStateLocked 从 monitor 快照恢复额度与模型级账本；调用方持有 accountMu。
func restoreAccountRuntimeStateLocked() {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return
	}
	var snap statusSnapshot
	if json.Unmarshal(data, &snap) != nil {
		return
	}
	byPath := make(map[string]accountSnapshot, len(snap.Accounts))
	for _, state := range snap.Accounts {
		byPath[state.Path] = state
	}
	for _, acc := range accounts {
		state, ok := byPath[acc.Path]
		if !ok || acc.Disabled {
			continue
		}
		acc.QuotaTotal = state.QuotaTotal
		acc.QuotaUsed = state.QuotaUsed
		acc.QuotaRemaining = state.QuotaRemaining
		acc.IsPaidUser = state.IsPaidUser
		acc.QuotaKnown = state.QuotaKnown
		acc.QuotaExhausted = state.QuotaExhausted
		if len(state.ModelStates) == 0 {
			continue
		}
		acc.ModelStates = make(map[string]*modelRuntimeState, len(state.ModelStates))
		for model, saved := range state.ModelStates {
			acc.ModelStates[normalizeModelName(model)] = &modelRuntimeState{
				CostClass: saved.CostClass, CooldownUntil: timeFromUnix(saved.CooldownUntil),
				QuotaBlocked: saved.QuotaBlocked, NextProbeAt: timeFromUnix(saved.NextProbeAt),
				LastReason: saved.LastReason, ObservedAt: timeFromUnix(saved.ObservedAt),
			}
		}
	}
}

func timeFromUnix(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}

func unixOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

// -----------------------------------------------------------------------------
// 凭据热加载：serve 运行期间自动发现凭据文件的新增/更新/删除，免重启收敛账号池
// -----------------------------------------------------------------------------

// accountFingerprint 基于文件 mtime+size 生成凭据文件变更指纹。
func accountFingerprint(st os.FileInfo) string {
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

// reloadAccounts 将磁盘上的凭据文件与内存账号池原地收敛（热加载）：
//   - 新增凭据文件 → 自动加入账号池；
//   - 凭据内容变化（重新登录/手动更新）→ 原地替换凭据、清除冷却/失效状态并恢复调度；
//   - 凭据文件被删除 → 移出账号池（已写入失效标记的幻影账号保留，用于提示重新登录）。
//
// 返回本次是否发生变更。调用方需自行处理 accountMu 之外的快照刷新。
func reloadAccounts() bool {
	accountMu.Lock()
	defer accountMu.Unlock()

	changed := false
	seen := make(map[string]bool)

	for _, p := range collectConfiguredAuthPaths() {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		seen[p] = true
		fp := accountFingerprint(st)

		var existing *Account
		for _, acc := range accounts {
			if acc.Path == p {
				existing = acc
				break
			}
		}

		if existing == nil {
			// 新凭据文件：加载并加入账号池（文件可能正在写入，失败则下轮重试）
			sa, err := loadAccountFile(p)
			if err != nil {
				log.Printf("[Reload] 凭据文件 %s 暂不可解析，下轮重试: %v", p, err)
				continue
			}
			accounts = append(accounts, &Account{
				Path:        p,
				Auth:        sa,
				Edition:     profileForEdition(sa.Edition).Key,
				fingerprint: fp,
			})
			log.Printf("[Reload] 发现新账号凭据 %s（%s），已自动加入账号池", p, profileForEdition(sa.Edition).Label)
			changed = true
			continue
		}

		if existing.fingerprint == fp {
			continue // 未变化
		}

		// 内容变化：原地替换凭据（重新登录或手动更新）
		sa, err := loadAccountFile(p)
		if err != nil {
			// 不更新指纹，下一轮重试（正常写入窗口极短，几乎必在下轮成功）
			log.Printf("[Reload] 凭据文件 %s 变更但暂不可解析，保留旧凭据: %v", p, err)
			continue
		}
		recovered := existing.Disabled
		existing.Auth = sa
		existing.Edition = profileForEdition(sa.Edition).Key
		existing.Disabled = false
		existing.DisabledReason = ""
		existing.CooldownUntil = time.Time{}
		existing.CooldownMsg = ""
		existing.QuotaKnown = false
		existing.QuotaExhausted = false
		existing.ModelStates = nil
		existing.fingerprint = fp
		clearDisabledMarker(p)
		if recovered {
			log.Printf("[Reload] 账号 %s 重新登录成功，已自动恢复调度", p)
		} else {
			log.Printf("[Reload] 账号 %s 凭据已更新（热加载，无需重启）", p)
		}
		changed = true
	}

	// 收敛：移除已删除的凭据（失效幻影账号保留）
	kept := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if !seen[acc.Path] && !acc.Disabled {
			log.Printf("[Reload] 凭据文件 %s 已删除，已移出账号池", acc.Path)
			changed = true
			continue
		}
		kept = append(kept, acc)
	}
	accounts = kept
	if len(accounts) > 0 {
		rrIndex %= len(accounts)
	} else {
		rrIndex = 0
	}
	return changed
}

// accountReloaderLoop serve 后台凭据热加载协程：周期扫描凭据变化并原地收敛账号池。
func accountReloaderLoop() {
	interval := time.Duration(cfg.ReloadInterval) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if reloadAccounts() {
			writeStatusSnapshot() // 立即刷新 monitor 状态文件
			requestQuotaScan()
			requestCheckin()
		}
	}
}

func normalizeModelName(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func modelStateLocked(acc *Account, model string) *modelRuntimeState {
	if acc.ModelStates == nil {
		acc.ModelStates = make(map[string]*modelRuntimeState)
	}
	model = normalizeModelName(model)
	state := acc.ModelStates[model]
	if state == nil {
		state = &modelRuntimeState{CostClass: modelCostUnknown}
		acc.ModelStates[model] = state
	}
	return state
}

func usableForModelLocked(acc *Account, model string, now time.Time) (accountSelectionKind, bool) {
	if acc.Disabled || acc.CooldownUntil.After(now) {
		return "", false
	}
	model = normalizeModelName(model)
	state := acc.ModelStates[model]
	if state == nil {
		if !acc.QuotaExhausted {
			return selectionNormal, true
		}
		return selectionProbeExhausted, true
	}
	if state.CooldownUntil.After(now) {
		return "", false
	}
	if state.QuotaBlocked {
		return "", false
	}
	if !acc.QuotaExhausted {
		return selectionNormal, true
	}
	if state.CostClass == modelCostFree && !state.QuotaBlocked {
		return selectionFreeExhausted, true
	}
	if state.CostClass == modelCostPaid || now.Before(state.NextProbeAt) {
		return "", false
	}
	return selectionProbeExhausted, true
}

// nextAccountForModel 按「免费耗尽账号优先 → 有余额账号 → 零余额未知模型受控探测」选号。
func nextAccountForModel(model string, attempted map[*Account]bool) (*Account, accountSelectionKind, error) {
	accountMu.Lock()
	defer accountMu.Unlock()

	if len(accounts) == 0 {
		return nil, "", fmt.Errorf("账号池为空")
	}
	now := time.Now()
	for _, wanted := range []accountSelectionKind{selectionFreeExhausted, selectionNormal, selectionProbeExhausted} {
		for i := 0; i < len(accounts); i++ {
			idx := (rrIndex + i) % len(accounts)
			acc := accounts[idx]
			if attempted != nil && attempted[acc] {
				continue
			}
			kind, ok := usableForModelLocked(acc, model, now)
			if !ok || kind != wanted {
				continue
			}
			if kind == selectionProbeExhausted {
				modelStateLocked(acc, model).NextProbeAt = now.Add(modelProbeDelay)
			}
			rrIndex = (idx + 1) % len(accounts)
			return acc, kind, nil
		}
	}

	disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting := 0, 0, 0, 0, 0
	earliest := time.Time{}
	for _, acc := range accounts {
		switch {
		case acc.Disabled:
			disabled++
		case acc.CooldownUntil.After(now):
			accountCooling++
			if earliest.IsZero() || acc.CooldownUntil.Before(earliest) {
				earliest = acc.CooldownUntil
			}
		default:
			state := acc.ModelStates[normalizeModelName(model)]
			if state == nil {
				if acc.QuotaExhausted {
					probeWaiting++
				}
				continue
			}
			if state.CooldownUntil.After(now) {
				modelCooling++
				if earliest.IsZero() || state.CooldownUntil.Before(earliest) {
					earliest = state.CooldownUntil
				}
			} else if state.QuotaBlocked || acc.QuotaExhausted && state.CostClass == modelCostPaid {
				quotaBlocked++
			} else if acc.QuotaExhausted && now.Before(state.NextProbeAt) {
				probeWaiting++
			}
		}
	}
	msg := fmt.Sprintf("当前模型 %s 暂无可用账号：授权失效=%d，账号冷却=%d，模型冷却=%d，额度阻断=%d，等待探测=%d", model, disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting)
	if !earliest.IsZero() {
		msg += "，最早恢复=" + earliest.Format("2006-01-02 15:04:05")
	}
	return nil, "", fmt.Errorf("%s", msg)
}

func nextAccount() (*Account, error) {
	acc, _, err := nextAccountForModel("", nil)
	return acc, err
}

// markCooldown 将账号屏蔽至指定时间。
func markCooldown(acc *Account, until time.Time, msg string) {
	accountMu.Lock()
	acc.CooldownUntil = until
	acc.CooldownMsg = msg
	accountMu.Unlock()
	log.Printf("[Cooldown] 账号 %s 触发频率限制，自动屏蔽至 %s (提示: %s)",
		acc.Path, until.Format("2006-01-02 15:04:05"), msg)
}

func markModelCooldown(acc *Account, model string, until time.Time, msg string) {
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	state.CooldownUntil = until
	state.LastReason = msg
	accountMu.Unlock()
	log.Printf("[ModelCooldown] 账号 %s 模型 %s 触发模型级频率限制，仅屏蔽该模型至 %s；其他模型仍可调度", acc.Path, model, until.Format("2006-01-02 15:04:05"))
	writeStatusSnapshot()
}

func markModelQuotaBlocked(acc *Account, model, msg string) {
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	state.CostClass = modelCostPaid
	state.QuotaBlocked = true
	state.LastReason = msg
	state.ObservedAt = time.Now()
	accountMu.Unlock()
	log.Printf("[ModelQuota] 账号 %s 模型 %s 返回额度耗尽，仅阻断该账号的当前收费模型；已知免费模型仍可使用", acc.Path, model)
	writeStatusSnapshot()
}

// disableAccount 将账号标记为失效（授权过期/撤销），禁止调度并删除凭据文件，
// 同时写入持久化失效标记，便于控制台提示用户重新登录。
func disableAccount(acc *Account, reason string) {
	accountMu.Lock()
	acc.Disabled = true
	acc.DisabledReason = reason
	acc.CooldownUntil = time.Time{}
	path := acc.Path
	nickname := ""
	uid := ""
	edition := ""
	if acc.Auth != nil {
		nickname = acc.Auth.Account.Nickname
		uid = acc.Auth.Account.UID
		edition = profileForEdition(acc.Auth.Edition).Key
		acc.Nickname = nickname
		acc.UID = uid
		acc.Edition = edition
	}
	accountMu.Unlock()

	// 写持久化失效标记（凭据删除后仍能在 status 中提示重新登录）
	marker := disabledMarker{
		Path:       path,
		Reason:     reason,
		DisabledAt: time.Now().Unix(),
		Nickname:   nickname,
		UID:        uid,
		Edition:    edition,
	}
	if data, err := json.MarshalIndent(marker, "", "  "); err == nil {
		if werr := os.WriteFile(markerPath(path), data, 0600); werr != nil {
			log.Printf("[Auth] 账号 %s 失效标记写入失败: %v", path, werr)
		}
	}

	// 删除失效的凭据文件，方便用户下次重新登录
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[Auth] 账号 %s 凭据文件删除失败: %v", path, err)
	}
	log.Printf("[Auth] 账号 %s 授权失效，已禁止调度并删除凭据文件: %s", path, reason)
	writeStatusSnapshot()
}

// loadDisabledMarkers 扫描失效标记文件，将其恢复为账号池中的失效账号（Auth 为 nil）。
func loadDisabledMarkers() {
	// 收集标记文件路径：
	// - AuthDir 模式：目录下 *.json.disabled 即为标记文件
	// - 显式 -auth 模式：<凭据路径>.disabled 为标记文件
	// - 自动发现模式：扫描当前目录下所有 *.json.disabled 标记文件
	var markerPaths []string
	if cfg.AuthDir != "" {
		entries, err := os.ReadDir(cfg.AuthDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasSuffix(name, ".json.disabled") {
				markerPaths = append(markerPaths, filepath.Join(cfg.AuthDir, name))
			}
		}
	} else if cfg.AuthExplicit {
		for _, p := range strings.Split(cfg.AuthFile, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				markerPaths = append(markerPaths, markerPath(p))
			}
		}
	} else {
		// 自动发现模式：扫描当前工作目录
		entries, err := os.ReadDir(".")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if strings.HasPrefix(name, "workbuddy") && strings.HasSuffix(name, ".json.disabled") {
					markerPaths = append(markerPaths, name)
				}
			}
			sort.Strings(markerPaths)
		}
	}

	for _, mp := range markerPaths {
		data, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var m disabledMarker
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		// 避免与已加载的有效账号重复
		dup := false
		for _, a := range accounts {
			if a.Path == m.Path {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		accounts = append(accounts, &Account{
			Path:           m.Path,
			Disabled:       true,
			DisabledReason: m.Reason,
			Nickname:       m.Nickname,
			UID:            m.UID,
			Edition:        m.Edition,
		})
	}
}

// clearDisabledMarker 在重新登录成功后清除失效标记。
func clearDisabledMarker(path string) {
	mp := markerPath(path)
	if err := os.Remove(mp); err != nil && !os.IsNotExist(err) {
		log.Printf("[Auth] 失效标记清除失败 (%s): %v", mp, err)
	}
}

// isAuthFailure 判断上游响应是否为授权失效（401/403 / invalid token / 登录过期等）。
func isAuthFailure(statusCode int, body string) bool {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return true
	}
	low := strings.ToLower(body)
	if strings.Contains(low, "invalid token") ||
		strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "登录已过期") ||
		strings.Contains(low, "登录失效") ||
		strings.Contains(low, "token 已失效") ||
		strings.Contains(low, "authentication required") {
		return true
	}
	return false
}

var resetTimeRe = regexp.MustCompile(`将在\s*(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})\s*(UTC[+-]\d+(?::\d{2})?)?`)

// resetTimeGenericRe 兜底匹配任意「日期 + 时间(可选 UTC 偏移)」片段，
// 用于国际站等英文限流消息（如 "will reset at 2026-09-05 01:57:00 UTC+8"）。
var resetTimeGenericRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(?:\s*(UTC[+-]\d+(?::\d{2})?))?`)

// parseResetTime 从上游 429 错误消息中解析频率限制重置时间。
// 国内站示例: "您的使用量已超出频率限制，将在 2026-09-04 07:48:15 UTC+8 重置"
// 国际站示例: "Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8."
func parseResetTime(s string) (time.Time, bool) {
	m := resetTimeRe.FindStringSubmatch(s)
	if m == nil {
		m = resetTimeGenericRe.FindStringSubmatch(s)
	}
	if m == nil {
		return time.Time{}, false
	}
	return parseDateTimeTZ(m[1], m[2], m[3])
}

// parseDateTimeTZ 按「日期 时间 (可选 UTC 偏移)」解析时间；未提供时区时默认按 UTC+8。
func parseDateTimeTZ(date, clock, zone string) (time.Time, bool) {
	offset := 8 * 3600 // 默认按 UTC+8 解析
	if zone != "" {
		z := zone
		z = strings.TrimPrefix(z, "UTC")
		z = strings.TrimPrefix(z, "utc")
		var h, mi int
		if _, err := fmt.Sscanf(z, "%d:%d", &h, &mi); err == nil {
			offset = h*3600 + mi*60
		} else if _, err := fmt.Sscanf(z, "%d", &h); err == nil {
			offset = h * 3600
		}
	}
	loc := time.FixedZone("UTC", offset)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// isRateLimited 判断上游响应是否属于频率限制（429 或 code 6004 / 频率限制提示，含中英文）。
func isRateLimited(statusCode int, body string) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if strings.Contains(body, `"code":6004`) || strings.Contains(body, "频率限制") || strings.Contains(body, "frequency limit") {
		return true
	}
	low := strings.ToLower(body)
	if strings.Contains(low, "rate limit") || strings.Contains(low, "ratelimit") || strings.Contains(low, "too many requests") {
		return true
	}
	return false
}

func isModelRateLimited(body string) bool {
	low := strings.ToLower(body)
	return strings.Contains(body, `"code":6004`) || strings.Contains(body, "切换其他模型") || strings.Contains(low, "switch to another model")
}

func isQuotaExhausted(statusCode int, body string) bool {
	if statusCode != http.StatusTooManyRequests && !strings.Contains(body, `"code":14018`) {
		return false
	}
	low := strings.ToLower(body)
	return strings.Contains(body, `"code":14018`) || strings.Contains(body, "额度已用尽") || strings.Contains(low, "credits exhausted")
}

// ensureValidTokenFor 对指定账号检查并刷新令牌（多账号版）。
// 失效账号（Disabled 或 Auth 为 nil）直接跳过，不参与调度。
func ensureValidTokenFor(acc *Account) error {
	if acc == nil {
		return nil
	}
	accountMu.Lock()
	disabled := acc.Disabled
	hasAuth := acc.Auth != nil
	expiresAt := int64(0)
	if hasAuth {
		expiresAt = acc.Auth.Auth.ExpiresAt
	}
	accountMu.Unlock()
	if disabled || !hasAuth {
		return nil
	}
	now := time.Now()
	if now.Unix() > expiresAt-900 {
		log.Printf("[Auth] 账号 %s Token 需要续期，当前过期时间=%s，刷新阈值=过期前15分钟，开始调用上游刷新接口", acc.Path, time.Unix(expiresAt, 0).Format("2006-01-02 15:04:05"))
		if err := doRefreshTokenFor(acc); err != nil {
			accountMu.Lock()
			isDisabled := acc.Disabled
			accountMu.Unlock()
			if !isDisabled {
				markCooldown(acc, now.Add(time.Minute), "Token 自动续期失败: "+err.Error())
			}
			return err
		}
	}
	return nil
}

func ensureValidToken() error {
	sa, err := loadAuth()
	if err != nil {
		return err
	}
	// 如果距过期不足 15 分钟，则自动刷新
	if time.Now().Unix() > sa.Auth.ExpiresAt-900 {
		log.Printf("[Auth] 访问令牌即将或已经过期 (ExpiresAt=%s)，正在自动刷新...", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
		return doRefreshToken(sa)
	}
	return nil
}

func doRefreshToken(sa *StoredAuth) error {
	if sa == nil || sa.Auth.RefreshToken == "" {
		return fmt.Errorf("无法刷新：缺少 RefreshToken")
	}
	if _, err := refreshTokenPayload(sa); err != nil {
		return err
	}
	if err := saveAuth(sa); err != nil {
		return fmt.Errorf("写回凭据失败: %w", err)
	}
	log.Printf("[Auth] Token 刷新成功！新过期时间: %s", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	return nil
}

// doRefreshTokenFor 刷新指定账号的令牌并保存回其凭据文件（多账号版）。
// 若刷新因授权失效失败（401/403/refresh token 无效），自动禁用该账号并删除凭据文件。
func doRefreshTokenFor(acc *Account) error {
	if acc == nil {
		return fmt.Errorf("无法刷新：账号缺少 RefreshToken 或已失效")
	}

	// 与该账号的上游请求串行，避免刷新过程中请求读到一半更新的 Token。
	acc.lock.Lock()
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.RefreshToken == "" {
		accountMu.Unlock()
		return fmt.Errorf("无法刷新：账号缺少 RefreshToken 或已失效")
	}
	refreshed := *acc.Auth
	path := acc.Path
	oldExpiresAt := refreshed.Auth.ExpiresAt
	accountMu.Unlock()

	log.Printf("[Auth] 账号 %s 开始刷新 Token，站点=%s，刷新前过期时间=%s", path, profileForEdition(refreshed.Edition).Label, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"))
	status, err := refreshTokenPayload(&refreshed)
	if err != nil {
		log.Printf("[Auth] 账号 %s Token 刷新失败，HTTP=%d，原因=%v，旧凭据未覆盖", path, status, err)
		if isAuthFailure(status, err.Error()) {
			disableAccount(acc, fmt.Sprintf("令牌刷新失败 (HTTP %d): %v", status, err))
		}
		return err
	}
	if err := saveAuthTo(path, &refreshed); err != nil {
		log.Printf("[Auth] 账号 %s Token 已从上游刷新，但写回凭据失败，内存与磁盘均保留旧凭据: %v", path, err)
		return fmt.Errorf("写回凭据失败: %w", err)
	}
	fingerprint := ""
	if st, statErr := os.Stat(path); statErr == nil {
		fingerprint = accountFingerprint(st)
	}
	accountMu.Lock()
	if !acc.Disabled {
		acc.Auth = &refreshed
		acc.Edition = profileForEdition(refreshed.Edition).Key
		acc.fingerprint = fingerprint
	}
	accountMu.Unlock()
	log.Printf("[Auth] 账号 %s Token 刷新成功，过期时间由 %s 更新为 %s，凭据已安全写回", path, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"), time.Unix(refreshed.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	writeStatusSnapshot()
	return nil
}

// refreshTokenPayload 调用上游刷新接口并更新内存中的令牌字段（不落盘）。
// 按凭据文件中的 edition 路由到对应站点（国内站/国际站）的刷新接口。
// 返回上游 HTTP 状态码（成功或失败时均为实际状态；网络错误为 0）。
func refreshTokenPayload(sa *StoredAuth) (int, error) {
	prof := profileForEdition(sa.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		// 客户端刷新链路实测同样携带 OTel/B3 传播头（traceSpan.requestHeaders）。
		// 刷新是一次独立操作、无下游会话语义，故不参与会话作用域：全部 ID 新生成。
		setTraceHeaders(r, resolveLinkIDs(nil, sessionScope{}))
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", "plugin")
	}

	data, status, err := doJSON(cfg.HttpClient, http.MethodPost, prof.tokenRefreshURL(), headers, nil)
	if err != nil {
		return status, fmt.Errorf("上游刷新拒绝 (HTTP %d): %w", status, err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return status, fmt.Errorf("解析新 Token 失败: %w", err)
	}

	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return status, nil
}

func parseQuotaCapacity(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return strconv.ParseFloat(value, 64)
}

func parseQuotaSummary(data []byte) (total, used, remaining float64, paid bool, err error) {
	var summary quotaSummaryData
	if err = json.Unmarshal(data, &summary); err != nil {
		return 0, 0, 0, false, fmt.Errorf("解析额度响应失败: %w", err)
	}
	for _, pkg := range summary.Packages {
		pkgTotal, parseErr := parseQuotaCapacity(pkg.CycleTotalCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, fmt.Errorf("解析总额度 %q 失败: %w", pkg.CycleTotalCapacity, parseErr)
		}
		pkgUsed, parseErr := parseQuotaCapacity(pkg.CycleUsedCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, fmt.Errorf("解析已用额度 %q 失败: %w", pkg.CycleUsedCapacity, parseErr)
		}
		pkgRemaining, parseErr := parseQuotaCapacity(pkg.CycleRemainCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, fmt.Errorf("解析剩余额度 %q 失败: %w", pkg.CycleRemainCapacity, parseErr)
		}
		total += pkgTotal
		used += pkgUsed
		remaining += pkgRemaining
	}
	return total, used, remaining, summary.IsPaidUser, nil
}

func formatQuota(value float64) string {
	if value > -0.005 && value < 0.005 {
		value = 0
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func usageCredit(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	value, exists := usage["credit"]
	if !exists {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func usageFromCompletion(data []byte) map[string]any {
	var completion map[string]any
	if json.Unmarshal(data, &completion) != nil {
		return nil
	}
	usage, _ := completion["usage"].(map[string]any)
	return usage
}

func usageTotalTokens(usage map[string]any) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	switch v := usage["total_tokens"].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func observeModelCredit(acc *Account, model string, usage map[string]any, reqID uint64) {
	credit, ok := usageCredit(usage)
	if !ok || acc == nil {
		return
	}
	// credit=0 只有样本足够大时才判定为免费：极小请求（例如探针）上游也可能记 0，
	// 那不代表该模型真的免费，不能据此让零余额账号去请求收费模型。
	if credit <= 0 {
		total, hasTotal := usageTotalTokens(usage)
		if !hasTotal || total < modelFreeMinTokens {
			log.Printf("[ModelCost] requestId=%d 账号 %s 模型 %s credit=0 但样本过小（total_tokens=%d < %d），忽略本次免费判定",
				reqID, acc.Path, model, total, modelFreeMinTokens)
			return
		}
	}
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	oldClass := state.CostClass
	if credit <= 0 {
		state.CostClass = modelCostFree
		state.QuotaBlocked = false
	} else {
		state.CostClass = modelCostPaid
	}
	state.ObservedAt = time.Now()
	quotaExhausted := acc.QuotaExhausted
	path := acc.Path
	accountMu.Unlock()
	recordModelCostClass(model, acc.Profile().Key, credit <= 0)

	if credit <= 0 {
		if oldClass != modelCostFree {
			log.Printf("[ModelCost] requestId=%d 账号 %s 模型 %s 实测 usage.credit=0，已学习为免费模型", reqID, path, model)
		}
		if quotaExhausted {
			log.Printf("[FreeModel] requestId=%d 请求的是免费模型 %s，已明确使用付费余额耗尽账号 %s 完成请求，usage.credit=0", reqID, model, path)
		}
	} else if oldClass != modelCostPaid {
		log.Printf("[ModelCost] requestId=%d 账号 %s 模型 %s 实测 usage.credit=%s，已学习为收费模型", reqID, path, model, formatQuota(credit))
	}
	writeStatusSnapshot()
}

// refreshAccountQuota 通过用户中心只读计费接口刷新账号额度，不记录或回传 Token。
func refreshAccountQuota(ctx context.Context, acc *Account) error {
	if acc == nil {
		return nil
	}
	if !lockAccountWithContext(ctx, &acc.lock) {
		return ctx.Err()
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		return nil
	}
	auth := *acc.Auth
	path := acc.Path
	prof := acc.Profile()
	accountMu.Unlock()

	log.Printf("[Quota] 账号 %s 开始查询额度，站点=%s，接口=%s", path, prof.Label, prof.quotaSummaryURL())
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
	}
	data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.quotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		log.Printf("[Quota] 账号 %s 查询额度失败，HTTP=%d，原因=%v，保留上一次额度数据", path, status, err)
		return err
	}
	total, used, remaining, paid, err := parseQuotaSummary(data)
	if err != nil {
		log.Printf("[Quota] 账号 %s 查询额度响应无法解析，原因=%v，保留上一次额度数据", path, err)
		return err
	}
	accountMu.Lock()
	wasExhausted := acc.QuotaExhausted
	acc.QuotaTotal = total
	acc.QuotaUsed = used
	acc.QuotaRemaining = remaining
	acc.IsPaidUser = paid
	acc.QuotaKnown = true
	acc.QuotaExhausted = remaining <= 0
	if remaining > 0 {
		for _, state := range acc.ModelStates {
			state.QuotaBlocked = false
			state.NextProbeAt = time.Time{}
		}
	}
	accountMu.Unlock()
	switch {
	case !wasExhausted && remaining <= 0:
		log.Printf("[Quota] 账号 %s 额度查询成功，总额度=%s，已用=%s，剩余=%s，付费用户=%t；账号已冻结调度，等待额度恢复", path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid)
	case wasExhausted && remaining > 0:
		log.Printf("[Quota] 账号 %s 额度已恢复，总额度=%s，已用=%s，剩余=%s，付费用户=%t；账号已自动解除冻结并恢复调度", path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid)
	default:
		log.Printf("[Quota] 账号 %s 额度查询成功，总额度=%s，已用=%s，剩余=%s，付费用户=%t", path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid)
	}
	return nil
}

func lockAccountWithContext(ctx context.Context, mu *sync.Mutex) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if mu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func refreshAllAccountQuotas(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	log.Printf("[Quota] 开始批量更新额度，账号数=%d", len(accs))
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			log.Printf("[Quota] 本轮额度更新被取消，尚未扫描的账号将等待下一轮: %v", err)
			break
		}
		accountCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := refreshAccountQuota(accountCtx, acc)
		cancel()
		if err != nil {
			log.Printf("[Quota] 账号 %s 本轮额度更新未完成: %v", acc.Path, err)
		}
	}
	writeStatusSnapshot()
	log.Printf("[Quota] 本轮额度更新完成，状态快照已写入")
}

func backgroundQuotaRefresher() {
	const interval = 300 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var cancel context.CancelFunc
	var done chan struct{}
	start := func() {
		if cancel != nil && done != nil {
			select {
			case <-done:
			default:
				log.Printf("[Quota] 新一轮额度扫描到达，正在强制取消上一轮未完成的请求")
				cancel()
			}
		}
		ctx, nextCancel := context.WithCancel(context.Background())
		cancel = nextCancel
		done = make(chan struct{})
		go refreshAllAccountQuotas(ctx, done)
	}
	start()
	for {
		select {
		case <-ticker.C:
			start()
		case <-quotaScanTrigger:
			log.Printf("[Quota] 凭据池发生变化，立即触发一轮额度扫描")
			start()
		}
	}
}

func requestQuotaScan() {
	select {
	case quotaScanTrigger <- struct{}{}:
	default:
	}
}

func isAlreadyCheckedIn(status int, message string) bool {
	if status == 0 && message == "" {
		return false
	}
	low := strings.ToLower(message)
	return strings.Contains(message, `"code":10001`) || strings.Contains(message, `"code":14001`) ||
		strings.Contains(message, "已签到") || strings.Contains(low, "already checked in")
}

// checkinAccount 执行国内站每日签到。国际站没有已确认可用的签到体系，明确跳过。
func checkinAccount(ctx context.Context, acc *Account) (string, error) {
	if acc == nil {
		return "skipped", nil
	}
	acc.lock.Lock()
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		return "skipped", nil
	}
	if acc.Profile().Key != profileCN.Key {
		accountMu.Unlock()
		return "global_skipped", nil
	}
	auth := *acc.Auth
	path := acc.Path
	prof := acc.Profile()
	accountMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	log.Printf("[Checkin] 账号 %s 开始每日签到，站点=%s，接口=%s", path, prof.Label, prof.dailyCheckinURL())
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if auth.Account.UID != "" {
			r.Header.Set("X-User-Id", auth.Account.UID)
		}
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
			r.Header.Set("X-Tenant-Id", auth.Account.EnterpriseID)
		}
		if auth.Auth.Domain != "" {
			r.Header.Set("X-Domain", auth.Auth.Domain)
		}
	}
	_, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.dailyCheckinURL(), headers, strings.NewReader("{}"))
	if err == nil {
		log.Printf("[Checkin] 账号 %s 每日签到成功", path)
		return "ok", nil
	}
	if isAlreadyCheckedIn(status, err.Error()) {
		log.Printf("[Checkin] 账号 %s 今天已经签到，本次按幂等成功处理", path)
		return "already", nil
	}
	log.Printf("[Checkin] 账号 %s 每日签到失败，HTTP=%d，原因=%v；不改变账号调度状态", path, status, err)
	return "failed", err
}

func checkinAllAccounts(ctx context.Context) {
	if !dailyCheckinMu.TryLock() {
		log.Printf("[Checkin] 已有一轮签到正在执行，本次重复触发已跳过")
		return
	}
	defer dailyCheckinMu.Unlock()

	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	log.Printf("[Checkin] 开始每日签到，账号总数=%d，国际站账号将跳过", len(accs))
	ok, already, failed, skipped := 0, 0, 0, 0
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			log.Printf("[Checkin] 本轮签到被取消，未处理账号等待下一次触发: %v", err)
			break
		}
		result, err := checkinAccount(ctx, acc)
		switch result {
		case "ok":
			ok++
		case "already":
			already++
		case "failed":
			failed++
			_ = err
		default:
			skipped++
		}
	}
	log.Printf("[Checkin] 本轮签到完成，总数=%d，成功=%d，今日已签=%d，失败=%d，跳过=%d", len(accs), ok, already, failed, skipped)
	requestQuotaScan()
}

func nextDailyCheckin(now time.Time) time.Time {
	loc := time.FixedZone("UTC+8", 8*60*60)
	localNow := now.In(loc)
	next := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 9, 0, 0, 0, loc)
	if !next.After(localNow) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func requestCheckin() {
	select {
	case checkinTrigger <- struct{}{}:
	default:
	}
}

func backgroundDailyCheckin() {
	go checkinAllAccounts(context.Background())
	for {
		next := nextDailyCheckin(time.Now())
		log.Printf("[Checkin] 下一次定时签到时间=%s", next.Format("2006-01-02 15:04:05 MST"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
			go checkinAllAccounts(context.Background())
		case <-checkinTrigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			log.Printf("[Checkin] 凭据池发生变化，立即触发一轮签到")
			go checkinAllAccounts(context.Background())
		}
	}
}

// -----------------------------------------------------------------------------
// CLI 子命令实现: login, status, refresh
// -----------------------------------------------------------------------------

// formatAccountStatus 生成单个账号的完整状态文本（status 命令与 serve 启动横幅共用）。
// idx 从 1 开始的账号序号；now 为当前时间（用于冷却/过期判定）。
func formatAccountStatus(acc *Account, idx int, now time.Time) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n--- 账号 #%d ---\n", idx))
	sb.WriteString(fmt.Sprintf("凭据文件:     %s\n", acc.Path))
	prof := acc.Profile()
	sb.WriteString(fmt.Sprintf("站点:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://")))

	if acc.Disabled {
		// 失效账号（Auth 可能为 nil：凭据文件已删除，信息来自失效标记）
		nickname := acc.Nickname
		uid := acc.UID
		if acc.Auth != nil {
			nickname = acc.Auth.Account.Nickname
			uid = acc.Auth.Account.UID
		}
		sb.WriteString(fmt.Sprintf("用户昵称:     %s\n", ifEmpty(nickname, "(未知)")))
		sb.WriteString(fmt.Sprintf("用户 UID:     %s\n", ifEmpty(uid, "(未知)")))
		sb.WriteString("账号状态:     授权失效（禁止调度）\n")
		sb.WriteString(fmt.Sprintf("失效原因:     %s\n", truncate(acc.DisabledReason, 200)))
		sb.WriteString(fmt.Sprintf("处理建议:     凭据文件已删除，请重新执行: workbuddy-gateway login -auth %s\n", acc.Path))
		return sb.String()
	}

	if acc.Auth == nil {
		return sb.String()
	}

	expTime := time.Unix(acc.Auth.Auth.ExpiresAt, 0)
	remaining := time.Until(expTime)
	statusStr := "有效"
	if remaining <= 0 {
		statusStr = "已过期"
	}

	sb.WriteString(fmt.Sprintf("用户昵称:     %s\n", acc.Auth.Account.Nickname))
	sb.WriteString(fmt.Sprintf("用户 UID:     %s\n", acc.Auth.Account.UID))
	sb.WriteString(fmt.Sprintf("企业 ID:      %s\n", ifEmpty(acc.Auth.Account.EnterpriseID, "(个人账号)")))
	sb.WriteString(fmt.Sprintf("认证域名:     %s\n", ifEmpty(acc.Auth.Auth.Domain, "www.codebuddy.cn")))
	if acc.CooldownUntil.After(now) {
		sb.WriteString(fmt.Sprintf("冷却状态:     冷却中 (解封: %s, 剩余 %v)\n",
			acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			time.Until(acc.CooldownUntil).Round(time.Minute)))
		sb.WriteString(fmt.Sprintf("冷却原因:     %s\n", truncate(acc.CooldownMsg, 120)))
	} else {
		sb.WriteString("冷却状态:     可用\n")
	}
	sb.WriteString(fmt.Sprintf("Token 状态:   %s\n", statusStr))
	sb.WriteString(fmt.Sprintf("过期时间:     %s (剩余 %v)\n", expTime.Format("2006-01-02 15:04:05"), remaining.Round(time.Minute)))
	return sb.String()
}

func runStatus() {
	if err := loadAccounts(); err != nil {
		fmt.Printf("未找到有效凭据: %v\n请先执行: workbuddy-gateway login 扫码登录。\n", err)
		return
	}

	accountMu.Lock()
	defer accountMu.Unlock()

	fmt.Println("================== WorkBuddy 账号池状态 ==================")
	fmt.Printf("账号总数: %d\n", len(accounts))
	now := time.Now()
	for i, acc := range accounts {
		fmt.Print(formatAccountStatus(acc, i+1, now))
	}
	fmt.Println("\n=======================================================")
}

func runRefresh() {
	if err := loadAccounts(); err != nil {
		fmt.Printf("读取凭据失败: %v\n", err)
		return
	}
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	if len(accs) == 0 {
		fmt.Println("账号池为空")
		return
	}
	ok := 0
	fail := 0
	skipped := 0
	for i, acc := range accs {
		fmt.Printf("正在刷新账号 #%d (%s)... ", i+1, acc.Path)
		accountMu.Lock()
		disabled := acc.Disabled || acc.Auth == nil
		accountMu.Unlock()
		if disabled {
			fmt.Printf("跳过（授权失效，请重新登录）\n")
			skipped++
			continue
		}
		if err := doRefreshTokenFor(acc); err != nil {
			fmt.Printf("失败: %v\n", err)
			fail++
		} else {
			fmt.Println("成功")
			ok++
		}
	}
	fmt.Printf("\n刷新完成: 成功 %d 个，失败 %d 个，跳过 %d 个（授权失效）\n", ok, fail, skipped)
}

// -----------------------------------------------------------------------------
// 状态快照与前台实时监控 (monitor)
// -----------------------------------------------------------------------------

// accountSnapshot 是写入状态快照文件的单个账号状态。
type accountSnapshot struct {
	Path           string                        `json:"path"`
	Edition        string                        `json:"edition,omitempty"` // 站点标识（cn/intl）
	Nickname       string                        `json:"nickname"`
	UID            string                        `json:"uid"`
	State          string                        `json:"state"` // active | cooldown | paid_exhausted | expired | disabled
	CooldownUntil  int64                         `json:"cooldownUntil,omitempty"`
	CooldownMsg    string                        `json:"cooldownMsg,omitempty"`
	DisabledReason string                        `json:"disabledReason,omitempty"`
	TokenExpiresAt int64                         `json:"tokenExpiresAt,omitempty"`
	QuotaTotal     float64                       `json:"quotaTotal,omitempty"`
	QuotaUsed      float64                       `json:"quotaUsed,omitempty"`
	QuotaRemaining float64                       `json:"quotaRemaining"`
	IsPaidUser     bool                          `json:"isPaidUser"`
	QuotaKnown     bool                          `json:"quotaKnown,omitempty"`
	QuotaExhausted bool                          `json:"quotaExhausted,omitempty"`
	ModelStates    map[string]modelStateSnapshot `json:"modelStates,omitempty"`
	FreeModels     int                           `json:"freeModels,omitempty"`
	ModelCooldowns int                           `json:"modelCooldowns,omitempty"`
}

type modelStateSnapshot struct {
	CostClass     string `json:"costClass,omitempty"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
	QuotaBlocked  bool   `json:"quotaBlocked,omitempty"`
	NextProbeAt   int64  `json:"nextProbeAt,omitempty"`
	LastReason    string `json:"lastReason,omitempty"`
	ObservedAt    int64  `json:"observedAt,omitempty"`
}

// statusSnapshot 是写入 workbuddy-status.json 的完整快照。
type statusSnapshot struct {
	UpdatedAt int64               `json:"updatedAt"`
	Accounts  []accountSnapshot   `json:"accounts"`
	Models    []modelStatSnapshot `json:"models,omitempty"`
}

// writeStatusSnapshot 将账号池实时状态（含冷却/失效）原子写入状态快照文件。
// serve 后台周期调用；monitor 命令前台读取展示。
func writeStatusSnapshot() {
	accountMu.Lock()
	snap := statusSnapshot{UpdatedAt: time.Now().Unix()}
	now := time.Now()
	for _, acc := range accounts {
		as := accountSnapshot{Path: acc.Path, Edition: acc.Profile().Key}
		as.QuotaTotal = acc.QuotaTotal
		as.QuotaUsed = acc.QuotaUsed
		as.QuotaRemaining = acc.QuotaRemaining
		as.IsPaidUser = acc.IsPaidUser
		as.QuotaKnown = acc.QuotaKnown
		as.QuotaExhausted = acc.QuotaExhausted
		if len(acc.ModelStates) > 0 {
			as.ModelStates = make(map[string]modelStateSnapshot, len(acc.ModelStates))
			for model, state := range acc.ModelStates {
				if state.CostClass == modelCostFree {
					as.FreeModels++
				}
				if state.CooldownUntil.After(now) {
					as.ModelCooldowns++
				}
				as.ModelStates[model] = modelStateSnapshot{
					CostClass: state.CostClass, CooldownUntil: unixOrZero(state.CooldownUntil),
					QuotaBlocked: state.QuotaBlocked, NextProbeAt: unixOrZero(state.NextProbeAt),
					LastReason: state.LastReason, ObservedAt: unixOrZero(state.ObservedAt),
				}
			}
		}
		switch {
		case acc.Disabled:
			as.State = "disabled"
			as.DisabledReason = acc.DisabledReason
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
			} else {
				as.Nickname = acc.Nickname
				as.UID = acc.UID
			}
		case acc.CooldownUntil.After(now):
			as.State = "cooldown"
			as.CooldownUntil = acc.CooldownUntil.Unix()
			as.CooldownMsg = acc.CooldownMsg
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		case acc.Auth != nil && acc.Auth.Auth.ExpiresAt > 0 && acc.Auth.Auth.ExpiresAt <= now.Unix():
			as.State = "expired"
			as.Nickname = acc.Auth.Account.Nickname
			as.UID = acc.Auth.Account.UID
			as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
		case acc.QuotaExhausted:
			as.State = "paid_exhausted"
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		default:
			as.State = "active"
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		}
		snap.Accounts = append(snap.Accounts, as)
	}
	snap.Models = buildModelStatSnapshots(now, accounts)
	accountMu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	// 原子写：先写临时文件再改名，避免 monitor 读到半截内容
	tmp := statusSnapshotFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, statusSnapshotFile)
}

func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe6f) ||
			(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6)) {
			width += 2
		} else {
			width++
		}
	}
	return width
}

func fitCell(s string, width int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if displayWidth(s) <= width {
		return s + strings.Repeat(" ", width-displayWidth(s))
	}
	limit := width - 3
	var b strings.Builder
	used := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		rw := displayWidth(string(r))
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
		s = s[size:]
	}
	return b.String() + "..." + strings.Repeat(" ", width-used-3)
}

func renderAccountTable(accs []accountSnapshot) string {
	widths := []int{4, 20, 24, 8, 10, 19, 10, 10, 10, 10, 10, 10}
	headers := []string{"序号", "凭据文件", "账号", "站点", "状态", "Token 有效期", "总额度", "已用", "剩余", "付费用户", "免费模型", "模型冷却"}
	border := func() string {
		var b strings.Builder
		b.WriteByte('+')
		for _, width := range widths {
			b.WriteString(strings.Repeat("-", width+2))
			b.WriteByte('+')
		}
		return b.String()
	}
	row := func(cells []string) string {
		var b strings.Builder
		b.WriteByte('|')
		for i, cell := range cells {
			b.WriteByte(' ')
			b.WriteString(fitCell(cell, widths[i]))
			b.WriteString(" |")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(border() + "\n")
	b.WriteString(row(headers) + "\n")
	b.WriteString(border() + "\n")
	for i, a := range accs {
		status := "可用"
		expires := "-"
		total, used, remaining, paid := "-", "-", "-", "-"
		if a.QuotaKnown {
			total = formatQuota(a.QuotaTotal)
			used = formatQuota(a.QuotaUsed)
			remaining = formatQuota(a.QuotaRemaining)
			paid = "否"
			if a.IsPaidUser {
				paid = "是"
			}
		}
		if a.TokenExpiresAt > 0 {
			expires = time.Unix(a.TokenExpiresAt, 0).Format("2006-01-02 15:04:05")
		}
		switch a.State {
		case "cooldown":
			status = "冷却"
		case "expired":
			status = "已过期"
		case "quota_exhausted", "paid_exhausted":
			status = "付费耗尽"
		case "disabled":
			status = "失效"
		}
		b.WriteString(row([]string{
			strconv.Itoa(i + 1), filepath.Base(a.Path), ifEmpty(a.Nickname, "-"),
			profileForEdition(a.Edition).Label, status, expires, total, used, remaining, paid,
			strconv.Itoa(a.FreeModels), strconv.Itoa(a.ModelCooldowns),
		}) + "\n")
	}
	b.WriteString(border())
	return b.String()
}

// statusSnapshotLoop serve 后台每 3 秒刷新一次状态快照。
func statusSnapshotLoop() {
	writeStatusSnapshot()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		writeStatusSnapshot()
	}
}

// tailLines 读取文件末尾 n 行（用于 monitor 展示最近日志）。
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// 先定位到文件末尾，从后向前扫描 n 个换行符
	const chunk = 4096
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	var lines []string
	buf := make([]byte, chunk)
	pos := size
	lineBuf := make([]byte, 0, chunk)
	newlines := 0
	for pos > 0 && newlines <= n {
		read := int64(chunk)
		if pos < chunk {
			read = pos
		}
		pos -= read
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			break
		}
		rn, err := f.Read(buf[:read])
		if err != nil && rn == 0 {
			break
		}
		for i := rn - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				if len(lineBuf) > 0 {
					lines = append([]string{string(lineBuf)}, lines...)
					lineBuf = lineBuf[:0]
					newlines++
					if newlines > n {
						break
					}
				}
			} else {
				lineBuf = append([]byte{buf[i]}, lineBuf...)
			}
		}
		if newlines > n {
			break
		}
	}
	if len(lineBuf) > 0 && newlines <= n {
		lines = append([]string{string(lineBuf)}, lines...)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// journalLines 获取 systemd 服务最近 n 行日志（Linux journalctl）。
func journalLines(service string, n int) ([]string, error) {
	cmd := exec.Command("journalctl", "-u", service, "-n", strconv.Itoa(n), "--no-pager", "-o", "short")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// runMonitor 前台实时监控：周期刷新展示账号池状态 + 最近日志（Ctrl+C 退出）。
func runMonitor() {
	interval := time.Duration(cfg.MonitorInterval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	logN := cfg.LogLines
	if logN <= 0 {
		logN = 15
	}

	// 判断 stdout 是否为终端（是则用 ANSI 清屏重绘，否则滚动输出）
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}

	fmt.Println("================ WorkBuddy 实时监控 ================")
	fmt.Println("按 Ctrl+C 退出 | 状态文件: " + statusSnapshotFile)
	fmt.Println("---------------------------------------------------------------")

	for {
		if isTTY {
			fmt.Print("\033[H\033[2J") // 清屏
		} else {
			fmt.Println()
		}
		fmt.Printf("更新时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))

		data, err := os.ReadFile(statusSnapshotFile)
		if err != nil {
			fmt.Printf("未找到状态文件 %s（服务是否在运行？请确认已在服务工作目录执行 monitor）\n", statusSnapshotFile)
		} else {
			var snap statusSnapshot
			if err := json.Unmarshal(data, &snap); err == nil {
				active, cooldown, exhausted, expired, disabled := 0, 0, 0, 0, 0
				for _, a := range snap.Accounts {
					switch a.State {
					case "active":
						active++
					case "cooldown":
						cooldown++
					case "expired":
						expired++
					case "quota_exhausted", "paid_exhausted":
						exhausted++
					case "disabled":
						disabled++
					}
				}
				fmt.Printf("账号池: 共 %d 个 | 可用 %d | 冷却 %d | 付费耗尽 %d | 过期 %d | 失效 %d\n",
					len(snap.Accounts), active, cooldown, exhausted, expired, disabled)
				fmt.Println(renderAccountTable(snap.Accounts))
				if len(snap.Models) > 0 {
					fmt.Printf("\n模型统计 (来源 %s):\n", modelSourceLabel(modelSourceFromSnapshots(snap.Models)))
					fmt.Println(renderModelTable(snap.Models))
				}
			} else {
				fmt.Println("状态文件解析失败")
			}
		}

		// 最近日志
		if cfg.JournalService != "" {
			if lines, err := journalLines(cfg.JournalService, logN); err == nil && len(lines) > 0 {
				fmt.Printf("\n最近日志 (journalctl -u %s):\n", cfg.JournalService)
				for _, l := range lines {
					fmt.Println("  " + l)
				}
			}
		} else if cfg.LogFile != "" {
			if lines, err := tailLines(cfg.LogFile, logN); err == nil && len(lines) > 0 {
				fmt.Printf("\n最近日志 (%s):\n", cfg.LogFile)
				for _, l := range lines {
					fmt.Println("  " + l)
				}
			}
		}

		fmt.Println("---------------------------------------------------------------")
		time.Sleep(interval)
	}
}

func runLogin() {
	prof := &profileCN
	if cfg.LoginIntl {
		prof = &profileINTL
	}

	fmt.Println("================ WorkBuddy 登录 ================")
	fmt.Printf("目标站点:     %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
	if cfg.LoginIntl {
		fmt.Println("国际站登录将在浏览器中完成（邮箱 / 验证码 / SSO 等），凭据由网关自动接管。")
	}
	fmt.Println("正在生成登录凭据与二维码...")

	loginClient := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     cfg.HttpClient.Jar,
	}

	// 轮询 auth/token 时与官方客户端一致，显式声明无 Authorization
	pollHeaders := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("X-No-Authorization", "true")
	}

	data, _, err := doJSON(loginClient, http.MethodPost, prof.authStateURL(), nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		fmt.Printf("获取登录状态失败: %v\n", err)
		return
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		fmt.Println("上游返回的登录状态信息异常，请重试。")
		return
	}

	// 终端字符二维码
	qr, err := qrcode.New(st.AuthURL, qrcode.Medium)
	if err == nil {
		if cfg.LoginIntl {
			fmt.Println("\n请用手机扫描下方二维码，并在浏览器中完成登录（邮箱 / 验证码 / SSO 等）：")
		} else {
			fmt.Println("\n请使用 微信 或 企业微信 扫描下方二维码登录：")
		}
		fmt.Println(qr.ToSmallString(false))
	}

	fmt.Println("如无法扫码，也可在浏览器中直接打开以下链接：")
	fmt.Printf("%s\n\n", st.AuthURL)
	fmt.Println("等待登录授权完成 (按 Ctrl+C 可取消)...")

	deadline := time.Now().Add(prof.LoginTTL)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				fmt.Println("\n登录已超时，请重新执行 login 命令。")
				return
			}
			tokRaw, _, errTok := doJSON(loginClient, http.MethodGet, prof.authTokenURL(st.State), pollHeaders, nil)
			if errTok != nil {
				continue
			}
			var tok tokenData
			if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
				continue
			}

			// 登录成功，拉取账号信息
			var acct accountData
			acctHeaders := func(r *http.Request) {
				commonHeaders(r, prof)
				r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			}
			if acctRaw, _, errAcct := doJSON(loginClient, http.MethodGet, prof.loginAcctURL(st.State), acctHeaders, nil); errAcct == nil {
				_ = json.Unmarshal(acctRaw, &acct)
			}

			sa := &StoredAuth{
				Edition: prof.Key,
				Auth: StoredTokens{
					AccessToken:  tok.AccessToken,
					RefreshToken: tok.RefreshToken,
					ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
					Domain:       tok.Domain,
				},
				Account: StoredAccount{
					UID:          acct.UID,
					EnterpriseID: acct.EnterpriseID,
					Nickname:     acct.Nickname,
				},
			}

			if err := saveAuth(sa); err != nil {
				fmt.Printf("\n登录成功但保存凭据失败: %v\n", err)
				return
			}

			fmt.Println("\n登录成功")
			fmt.Printf("站点:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
			fmt.Printf("欢迎，%s (UID: %s)\n", sa.Account.Nickname, sa.Account.UID)
			fmt.Printf("凭据已成功保存至: %s\n", cfg.AuthFile)
			fmt.Printf("令牌有效期至: %s\n", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
			fmt.Println("\n现在您可以运行以下命令启动网关服务：")
			fmt.Println("  workbuddy-gateway serve")
			return
		}
	}
}

// -----------------------------------------------------------------------------
// 本地 HTTP API 网关服务 (OpenAI 协议兼容)
// -----------------------------------------------------------------------------

func runServe() {
	loadModelsCache()
	initModelsHTTPClient()
	if err := loadAccounts(); err != nil {
		fmt.Printf("警告: 未检测到有效凭据 (%v)。\n请先执行: workbuddy-gateway login 扫码登录，或确保凭据文件存在。\n\n", err)
	} else {
		accountMu.Lock()
		accs := append([]*Account(nil), accounts...)
		accountMu.Unlock()
		for _, acc := range accs {
			if acc.Disabled || acc.Auth == nil {
				continue
			}
			_ = ensureValidTokenFor(acc)
		}
	}

	// 启动后台自动刷新协程
	go backgroundTokenRefresher()
	go backgroundQuotaRefresher()
	go backgroundDailyCheckin()
	if cfg.ModelsRefresh > 0 {
		go modelsRefreshLoop(time.Duration(cfg.ModelsRefresh) * time.Minute)
	}

	// 启动状态快照协程（monitor 命令实时读取展示）
	go statusSnapshotLoop()

	// 启动凭据热加载协程（新增/更新/删除凭据文件免重启生效）
	if cfg.ReloadInterval > 0 {
		go accountReloaderLoop()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/responses", handleResponses)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ping", handleHealth)
	mux.HandleFunc("/admin/probe", handleAdminProbe)
	mux.HandleFunc("/", handleIndex)

	listenAddr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	server := &http.Server{
		Addr:         listenAddr,
		Handler:      requestAuditMiddleware(corsMiddleware(authMiddleware(mux))),
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 300 * time.Second,
	}

	fmt.Println("================================================================")
	fmt.Printf("WorkBuddy 本地网关已启动\n")
	fmt.Printf("   服务监听地址:  http://%s\n", listenAddr)
	fmt.Printf("   Chat 接口地址: http://%s/v1/chat/completions\n", listenAddr)
	fmt.Printf("   Models 接口:   http://%s/v1/models\n", listenAddr)
	fmt.Printf("   模型转发策略:  【完全透传】客户端请求的任意 model 原样中继至上游\n")
	_, modelSource := mergedModelIDs()
	fmt.Printf("   模型列表来源:  %s\n", modelSourceLabel(modelSource))
	if cfg.ReloadInterval > 0 {
		fmt.Printf("   凭据热加载:    每 %ds 自动扫描，新增/更新/删除凭据免重启生效\n", cfg.ReloadInterval)
	} else {
		fmt.Printf("   凭据热加载:    已关闭 (-reload-interval 0)\n")
	}
	if cfg.APIKey != "" {
		fmt.Printf("   API 鉴权:      已启用 (Bearer %s)\n", cfg.APIKey)
	} else {
		fmt.Printf("   API 鉴权:      未启用 (任何客户端均可直连)\n")
	}
	if cfg.ProxyURL != "" {
		fmt.Printf("   上游出口代理:  %s\n", cfg.ProxyURL)
	}

	// 启动时展示所有账号状态（与 status 命令一致）
	accountMu.Lock()
	accCount := len(accounts)
	activeCount := 0
	disabledCount := 0
	now := time.Now()
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil {
			disabledCount++
		} else {
			activeCount++
		}
	}
	fmt.Printf("   账号池:        %d 个账号 (有效 %d, 失效 %d)\n", accCount, activeCount, disabledCount)
	if accCount > 0 {
		fmt.Println("   ----------------------------------------------------------")
		for i, acc := range accounts {
			fmt.Print(formatAccountStatus(acc, i+1, now))
		}
		fmt.Println("   ----------------------------------------------------------")
	}
	// 末尾汇总：加载到的凭据文件清单（即使上方日志被截断也能确认）
	fileList := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		fileList = append(fileList, acc.Path)
	}
	if len(fileList) > 0 {
		fmt.Printf("   已加载凭据文件 (%d): %s\n", len(fileList), strings.Join(fileList, ", "))
	} else {
		fmt.Println("   未加载到任何凭据文件，请先执行 login 命令扫码登录")
	}
	accountMu.Unlock()

	fmt.Println("================================================================")
	fmt.Println("等待客户端请求中 (按 Ctrl+C 安全停止)...")

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	<-stopChan
	fmt.Println("\n正在关闭网关服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	fmt.Println("网关已安全停止。")
}

func backgroundTokenRefresher() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		accountMu.Lock()
		accs := make([]*Account, len(accounts))
		copy(accs, accounts)
		accountMu.Unlock()
		for _, acc := range accs {
			if err := ensureValidTokenFor(acc); err != nil {
				log.Printf("[BackgroundAuth] 账号 %s 自动检查/续期令牌异常: %v", acc.Path, err)
			}
		}
	}
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *auditResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := r.Header.Get("X-Trace-ID")
		if traceID == "" {
			traceID = uuid.NewString()
		}
		w.Header().Set("X-Trace-ID", traceID)
		aw := &auditResponseWriter{ResponseWriter: w}
		log.Printf("[请求到达] traceId=%s 方法=%s 路径=%s 来源=%s 说明=请求已进入网关", traceID, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(aw, r)
		if aw.status == 0 {
			aw.status = http.StatusOK
		}
		log.Printf("[响应返回] traceId=%s 状态码=%d 耗时=%v 结果=%s", traceID, aw.status, time.Since(start), http.StatusText(aw.status))
	})
}

// -----------------------------------------------------------------------------
// 中间件: CORS 与可选 APIKey 校验
// -----------------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			log.Printf("[中间件-CORS] traceId=%s 结果=通过并直接响应 说明=预检请求未进入业务方法", w.Header().Get("X-Trace-ID"))
			w.WriteHeader(http.StatusOK)
			return
		}
		log.Printf("[中间件-CORS] traceId=%s 结果=通过", w.Header().Get("X-Trace-ID"))
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.APIKey != "" && r.URL.Path != "/health" && r.URL.Path != "/ping" && r.URL.Path != "/" {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != cfg.APIKey {
				log.Printf("[请求被拦截] traceId=%s 拦截层=API鉴权 结果=拒绝 原因=未提供有效API密钥 返回状态码=401 业务影响=请求未进入业务方法", w.Header().Get("X-Trace-ID"))
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
				return
			}
		}
		log.Printf("[中间件-鉴权] traceId=%s 结果=通过", w.Header().Get("X-Trace-ID"))
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// 路由处理: /v1/chat/completions (支持任意 model 透传)
// -----------------------------------------------------------------------------

// upstreamChat 完成「多账号轮询 + 429 冷却代偿 + 授权失效禁用 + 单账号串行」的上游调度。
// 成功时返回 200 响应（调用方负责关闭 Body）与命中的账号/站点；失败时函数内部已写回
// 错误响应并返回 ok=false。Chat Completions 与 Responses 两个入口共用此逻辑。
func upstreamChat(w http.ResponseWriter, r *http.Request, reqID uint64, modelName string, upstreamBytes []byte, startTime time.Time) (*http.Response, *Account, *upstreamProfile, bool) {
	traceID := w.Header().Get("X-Trace-ID")
	log.Printf("[业务入口] traceId=%s requestId=%d 业务=上游模型调用 请求体字节数=%d", traceID, reqID, len(upstreamBytes))
	recordModelRequest(modelName)
	accountMu.Lock()
	poolSize := len(accounts)
	accountMu.Unlock()
	if poolSize == 0 {
		recordModelFailure(modelName, "no_auth")
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "未找到有效登录凭据，请先执行 login 命令扫码登录")
		return nil, nil, nil, false
	}

	var lastRateErr string
	var lastAuthErr string
	// 会话作用域在一次下游请求内解析一次：重试换账号不应改变链路 ID，
	// 否则同一逻辑请求会在上游留下多条互不关联的会话链路。
	sess := newSessionScope(clientConversationIDs(r.Header))
	attempted := make(map[*Account]bool, poolSize)
	for attempt := 0; attempt < poolSize; attempt++ {
		acc, selection, err := nextAccountForModel(modelName, attempted)
		if err != nil {
			// 所有账号均不可用（冷却或失效）
			msg := fmt.Sprintf("无可用账号: %v", err)
			if lastAuthErr != "" {
				msg += " | 最近一次授权失效: " + truncate(lastAuthErr, 200)
			}
			if lastRateErr != "" {
				msg += " | 最近一次频率限制: " + truncate(lastRateErr, 200)
			}
			log.Printf("[#%d] %s", reqID, msg)
			recordModelFailure(modelName, "无可用账号")
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_available_account", msg)
			return nil, nil, nil, false
		}
		attempted[acc] = true
		switch selection {
		case selectionFreeExhausted:
			log.Printf("[FreeModel] requestId=%d 请求的是已知免费模型 %s，选择付费余额耗尽账号 %s 发起请求", reqID, modelName, acc.Path)
		case selectionProbeExhausted:
			log.Printf("[ModelProbe] requestId=%d 账号 %s 付费余额已耗尽，但模型 %s 收费属性未知；执行一次受控探测，5分钟内不重复探测", reqID, acc.Path, modelName)
		}
		if err := ensureValidTokenFor(acc); err != nil {
			lastAuthErr = err.Error()
			continue
		}
		// 令牌刷新时若发现授权失效会禁用账号；若被禁用则跳过换下一个
		accountMu.Lock()
		disabled := acc.Disabled
		accountMu.Unlock()
		if disabled {
			lastAuthErr = acc.DisabledReason
			continue
		}

		// 按账号所属站点（国内站/国际站）路由上游与指纹 Header
		prof := acc.Profile()

		upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, prof.chatURL(), bytes.NewReader(upstreamBytes))
		if err != nil {
			recordModelFailure(modelName, "req_create_error")
			writeOpenAIError(w, http.StatusInternalServerError, "req_create_error", err.Error())
			return nil, nil, nil, false
		}
		// 注入 CodeBuddy 凭据与指纹 Header。
		// 限制同一账号向腾讯上游的请求严格单并发串行排队，防止并发双发触发腾讯风控
		acc.lock.Lock()
		backendHeaders(upstreamReq, acc.Auth, prof, r.Header, sess)
		resp, err := cfg.HttpClient.Do(upstreamReq)
		acc.lock.Unlock()
		if err != nil {
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游网络调用 账号=%s 异常=%v 业务影响=本次模型请求失败 是否已处理=是", traceID, reqID, acc.Path, err)
			log.Printf("[#%d] 账号 %s [%s] 上游请求失败: %v", reqID, acc.Path, prof.Label, err)
			recordModelFailure(modelName, "网络错误")
			writeOpenAIError(w, http.StatusBadGateway, "upstream_network_error", fmt.Sprintf("网络转发失败: %v", err))
			return nil, nil, nil, false
		}

		// 上游非 200 响应处理
		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			errStr := string(errBody)
			log.Printf("[#%d] 账号 %s [%s] 上游返回 HTTP %d: %s (耗时 %v)", reqID, acc.Path, prof.Label, resp.StatusCode, errStr, time.Since(startTime))
			log.Printf("[外部接口] traceId=%s requestId=%d 上游=%s 状态码=%d 结果=失败 账号=%s", traceID, reqID, prof.Base, resp.StatusCode, acc.Path)

			if isQuotaExhausted(resp.StatusCode, errStr) {
				markModelQuotaBlocked(acc, modelName, errStr)
				if selection == selectionProbeExhausted {
					msg := fmt.Sprintf("当前模型 %s 已在余额耗尽账号 %s 上完成受控探测并确认需要付费额度，本次不再探测其他耗尽账号", modelName, acc.Path)
					log.Printf("[#%d] %s", reqID, msg)
					recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
					writeOpenAIError(w, http.StatusServiceUnavailable, "model_requires_quota", msg)
					return nil, nil, nil, false
				}
				lastRateErr = errStr
				continue
			}

			if isModelRateLimited(errStr) {
				until, ok := parseResetTime(errStr)
				if !ok {
					until = time.Now().Add(60 * time.Second)
				}
				markModelCooldown(acc, modelName, until, errStr)
				if selection == selectionProbeExhausted {
					msg := fmt.Sprintf("当前模型 %s 在余额耗尽账号 %s 的受控探测中触发模型级限流，本次不再探测其他耗尽账号", modelName, acc.Path)
					recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
					writeOpenAIError(w, http.StatusServiceUnavailable, "model_rate_limited", msg)
					return nil, nil, nil, false
				}
				lastRateErr = errStr
				continue
			}

			if isRateLimited(resp.StatusCode, errStr) {
				// 429 频率限制：解析重置时间并屏蔽该账号，交由其他账号代偿
				until, ok := parseResetTime(errStr)
				if !ok {
					until = time.Now().Add(60 * time.Second) // 无法解析时默认冷却 60 秒
				}
				markCooldown(acc, until, errStr)
				lastRateErr = errStr
				continue // 尝试下一个账号
			}

			if isAuthFailure(resp.StatusCode, errStr) {
				// 授权失效（401/403 / token 无效 / 登录过期）：禁用该账号并删除凭据文件，
				// 自动改用下一个可用账号，控制台提示用户重新登录
				disableAccount(acc, fmt.Sprintf("上游鉴权失败 (HTTP %d): %s", resp.StatusCode, truncate(errStr, 200)))
				lastAuthErr = errStr
				continue // 尝试下一个账号
			}

			recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
			writeOpenAIError(w, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream %d: %s", resp.StatusCode, errStr))
			return nil, nil, nil, false
		}

		log.Printf("[外部接口] traceId=%s requestId=%d 上游=%s 状态码=%d 结果=成功 账号=%s", traceID, reqID, prof.Base, resp.StatusCode, acc.Path)
		recordModelSuccess(modelName)
		return resp, acc, prof, true
	}

	// 理论上不可达（poolSize 次尝试后未成功即已在循环内返回）
	recordModelFailure(modelName, "all_cooldown")
	writeOpenAIError(w, http.StatusTooManyRequests, "all_accounts_cooldown", "所有账号均处于冷却状态")
	return nil, nil, nil, false
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	reqID := atomic.AddUint64(&reqCounter, 1)
	startTime := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	// 用保序解析替代 map[string]any：客户端以 JSON.stringify 发送，键序即属性插入顺序，
	// 经 map 中转后重编码会按键名字典序输出（messages 跑到 model 之前），构成机器特征。
	reqObj, err := decodeOrderedJSON(bodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	// 核心特性：完全透传 model 字段
	// 客户端传什么 model，我们就透传什么 model 给上游，不做任何硬编码限制！
	modelName, _ := reqObj.Get("model")
	modelStr, _ := modelName.(string)
	if modelStr == "" {
		modelStr = "default-model" // 保底取官方客户端默认模型（product config isDefault）
		reqObj.Set("model", modelStr)
	}

	isStream, _ := reqObj.Get("stream")
	isStreamBool, _ := isStream.(bool)

	// 腾讯上游强制要求 stream 必须为 true，非流式会被拦截 (code 11101)
	reqObj.Set("stream", true)

	// 思考等级归一化：把 OpenAI 兼容写法（顶层 reasoning_effort / 嵌套 reasoning.effort、
	// 以及 Responses 风格的 text.verbosity）落到上游认识的扁平字段上，并做大小写与
	// 关闭语义（none/off）归一化。绝不注入，客户端未传则不发送。
	applyThinkingRules(reqObj)

	// 消息归一化：developer 角色（OpenAI 新版 system 别名）在上游会被拒，统一转 system
	sanitizeMessages(reqObj)

	// 会话结构归一化：保证首条消息为 system，修复部分非 harness 客户端
	//（以 assistant / tool 续写或回传工具结果）触发的上游 11128 错误
	ensureLeadingSystemMessage(reqObj)

	// 以客户端口径序列化：不转义 < > &，无尾随换行
	upstreamBytes, err := marshalJSON(reqObj)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}

	if cfg.Verbose {
		log.Printf("[#%d][Req] Model: %s | Stream: %v | BodyLen: %d", reqID, modelStr, isStreamBool, len(upstreamBytes))
	} else {
		log.Printf("[#%d] POST /v1/chat/completions -> Upstream [Model: %s, Stream: %v]", reqID, modelStr, isStreamBool)
	}

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelStr, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStreamBool {
		streamChatResponse(w, resp, modelStr, reqID, acc, prof, startTime)
	} else {
		writeChatAggregate(w, resp, modelStr, reqID, acc, prof, startTime)
	}
}

// streamChatResponse 将上游 SSE 逐行透传为 OpenAI Chat Completions 流式响应。
func streamChatResponse(w http.ResponseWriter, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	body := newTTFTReader(resp.Body, startTime)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var usage map[string]any
	for scanner.Scan() {
		cleanData := stripDataPrefix(scanner.Text())
		if cleanData == "" {
			continue
		}
		// 心跳是 SSE 注释，不能包装成 JSON data，否则客户端会解析失败。
		if strings.HasPrefix(cleanData, ":") {
			_, _ = fmt.Fprintf(w, "%s\n\n", cleanData)
			flusher.Flush()
			continue
		}
		if cleanData == "[DONE]" {
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(cleanData), &chunk) == nil {
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = u
			}
		}
		if cleanedChunk := cleanChunkJSON(cleanData); cleanedChunk != "" {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", cleanedChunk)
			flusher.Flush()
		}
	}
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	log.Printf("[#%d] 流式输出完成 (账号 %s [%s], 耗时 %v, 首字 %v)", reqID, acc.Path, prof.Label, time.Since(startTime), body.duration())
}

// writeChatAggregate 聚合上游 SSE 为完整 Chat Completions JSON 响应。
func writeChatAggregate(w http.ResponseWriter, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		log.Printf("[#%d] 聚合响应失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	observeModelCredit(acc, modelName, usageFromCompletion(completionJSON), reqID)
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionJSON)
	log.Printf("[#%d] 非流式响应完成 (账号 %s [%s], 耗时 %v, 首字 %v)", reqID, acc.Path, prof.Label, time.Since(startTime), body.duration())
}

// -----------------------------------------------------------------------------
// 路由处理: /v1/models (模型清单由 models.go 动态同步 + 静态兜底合并)
// -----------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	modelIDs, source := mergedModelIDs()
	modelsList := make([]map[string]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		modelsList = append(modelsList, map[string]any{
			"id": id, "object": "model", "owned_by": "workbuddy", "permission": []any{},
		})
	}
	resp := map[string]any{
		"object": "list",
		"data":   modelsList,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Source", modelSourceLabel(source))
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	modelIDs, source := mergedModelIDs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "healthy",
		"timestamp":    time.Now().Unix(),
		"version":      version,
		"model_count":  len(modelIDs),
		"model_source": modelSourceLabel(source),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "WorkBuddy Local Gateway v%s is running.\n\nEndpoints:\n- POST /v1/chat/completions\n- POST /v1/responses\n- GET  /v1/models\n- GET  /health\n", version)
}

// -----------------------------------------------------------------------------
// 请求转译与净化工具函数
// -----------------------------------------------------------------------------

// reasoningEffortDisable 是语义等价于「关闭推理」的输入取值，归一化后一律删除该字段。
//
// none 是 OpenAI 规范值。实测上游收到 none 时仍会注入推理脚手架
// （prompt_tokens 18 -> 43，与 minimal/low/high 完全一致），与 OpenAI「不推理」语义相反，
// 故必须删除字段而非原样转发。off / disabled 为兼容别名。
var reasoningEffortDisable = map[string]bool{
	"none": true, "off": true, "disabled": true,
}

// reasoningEffortModelDefault 表示「交由上游按模型默认档位决定」，同样删除字段。
// auto / default 并非 OpenAI 规范值，但被部分兼容客户端使用；上游收到会直接 400 (11150)。
var reasoningEffortModelDefault = map[string]bool{
	"auto": true, "default": true,
}

// normalizeReasoningEffort 归一化客户端传入的思考等级。
// 返回空串表示不发送该字段（关闭推理，或交由上游默认档位决定）。
//
// 上游对取值大小写与空白敏感（HIGH / " high " 一律 HTTP 400 code 11150），
// 故统一小写去空白；取值本身不做白名单校验，模型维度的支持度由上游裁决
// （上游 11150 报文为 "not supported by the current model"）。
func normalizeReasoningEffort(raw any) string {
	eff, ok := raw.(string)
	if !ok {
		// null / 非字符串（数字、布尔）上游一律 400 (11101)，此处按缺省处理。
		return ""
	}
	eff = strings.ToLower(strings.TrimSpace(eff))
	if eff == "" || reasoningEffortDisable[eff] || reasoningEffortModelDefault[eff] {
		return ""
	}
	return eff
}

// hoistNestedReasoningFields 把嵌套 reasoning.{effort,summary} 与 text.verbosity
// 提升为上游认识的扁平字段 reasoning_effort / reasoning_summary / verbosity。
//
// 上游只解析扁平字段：实测嵌套 reasoning.effort 被完全忽略（prompt_tokens 保持 18，
// 即推理根本没开启）。若不提升，客户端以为设置了思考等级、实际静默失效。
// 顶层已有同名扁平字段时以顶层为准（Chat Completions 的顶层写法优先）。
func hoistNestedReasoningFields(obj *jsonObject) {
	if nested, ok := obj.Get("reasoning"); ok {
		if reasoning, ok := nested.(*jsonObject); ok {
			hoistFlatField(obj, reasoning, "effort", "reasoning_effort")
			hoistFlatField(obj, reasoning, "summary", "reasoning_summary")
		}
	}
	if nested, ok := obj.Get("text"); ok {
		if text, ok := nested.(*jsonObject); ok {
			hoistFlatField(obj, text, "verbosity", "verbosity")
		}
	}
}

// hoistFlatField 在顶层目标键缺失时，用嵌套对象中的同义键补齐。
func hoistFlatField(dst, src *jsonObject, srcKey, dstKey string) {
	if _, exists := dst.Get(dstKey); exists {
		return
	}
	if v, ok := src.Get(srcKey); ok && v != nil {
		dst.Set(dstKey, v)
	}
}

// applyThinkingRules 归一化思考等级相关字段，使对外暴露的 OpenAI 兼容写法
// 正确落到上游 /v2/chat/completions 认识的扁平字段上。
//
// 对外契约（OpenAI 兼容）：
//   - Chat Completions：顶层 reasoning_effort
//   - Responses：reasoning.effort / reasoning.summary、text.verbosity
//
// 上游契约：只识别扁平 reasoning_effort / reasoning_summary / verbosity。
//
// 绝不主动注入：客户端未显式传思考参数时，网关不会凭空造 reasoning_effort，
// 也不会追加 reasoning_summary——强行注入会触发上游内容安全拦截
// （code 11102/11128），且与官方客户端发往同一上游的报文不一致（见 README「思考等级传参」）。
func applyThinkingRules(obj *jsonObject) {
	hoistNestedReasoningFields(obj)

	effRaw, _ := obj.Get("reasoning_effort")
	eff := normalizeReasoningEffort(effRaw)
	if eff == "" {
		// 关闭推理：清掉思考等级与摘要，避免留下「已关闭却仍索要摘要」的矛盾组合。
		// verbosity 与推理开关正交（上游在无 effort 时也接受），不在此处处理。
		obj.Delete("reasoning_effort")
		obj.Delete("reasoning_summary")
		return
	}
	obj.Set("reasoning_effort", eff)
}

func sanitizeMessages(obj *jsonObject) {
	messages, ok := obj.Get("messages")
	if !ok {
		return
	}
	arr, ok := messages.([]any)
	if !ok {
		return
	}
	for _, m := range arr {
		msg, ok := m.(*jsonObject)
		if !ok {
			continue
		}
		// OpenAI 新版 developer 角色（GPT-5 系客户端/Codex）不被腾讯上游接受，
		// 会返回 11128 "Illegal API invocation from an unapproved channel"，
		// 统一归一化为 system（语义等价）。
		if roleOfMessage(msg) == "developer" {
			msg.Set("role", "system")
		}
	}
}

// ensureLeadingSystemMessage 保证 messages 的首条消息符合腾讯上游的会话结构校验。
//
// 上游要求首条消息为 system prompt，否则返回 HTTP 400
// {"code":11128,"msg":"first message is not system prompt"}。实测国内站对 user 开头较宽容，
// 但国际站（workbuddy.ai）严格校验；账号池混挂时表现为约 50% 请求随机失败。部分非 harness
// 客户端在续写或仅回传工具结果时还会以 assistant / tool 作为首条消息。此处统一归一化为：
//  1. 首条已是 system：原样透传；
//  2. 首条是 developer（OpenAI 新版 system 别名）：重命名为 system；
//  3. 后续存在 system/developer：提升到首位（developer 归一化为 system），其余保持原序；
//  4. 其余情况（user / assistant / tool 开头且无 system）：在最前注入一条保底 system。
func ensureLeadingSystemMessage(obj *jsonObject) {
	raw, ok := obj.Get("messages")
	if !ok {
		obj.Set("messages", []any{newObject("role", "system", "content", defaultSystemPrompt)})
		return
	}
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		obj.Set("messages", []any{newObject("role", "system", "content", defaultSystemPrompt)})
		return
	}

	switch roleOfMessage(messages[0]) {
	case "system":
		return
	case "developer":
		if msg, ok := messages[0].(*jsonObject); ok {
			msg.Set("role", "system")
		}
		return
	}

	// 后续存在 system/developer：提升到首位，其余保持原序
	for i := 1; i < len(messages); i++ {
		switch roleOfMessage(messages[i]) {
		case "system", "developer":
			if msg, ok := messages[i].(*jsonObject); ok {
				msg.Set("role", "system")
			}
			reordered := make([]any, 0, len(messages))
			reordered = append(reordered, messages[i])
			reordered = append(reordered, messages[:i]...)
			reordered = append(reordered, messages[i+1:]...)
			obj.Set("messages", reordered)
			return
		}
	}

	// 无任何 system：在最前注入保底 system（兼容国内站/国际站）
	injected := make([]any, 0, len(messages)+1)
	injected = append(injected, newObject("role", "system", "content", defaultSystemPrompt))
	injected = append(injected, messages...)
	obj.Set("messages", injected)
}

// roleOfMessage 读取消息的 role 字段并归一化为小写去空格；非法结构返回空串。
func roleOfMessage(m any) string {
	msg, ok := m.(*jsonObject)
	if !ok {
		return ""
	}
	role, _ := msg.Get("role")
	s, _ := role.(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// newTraceID 生成 32 位小写十六进制 trace id（OTel traceId 形态，无连字符）。
func newTraceID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// newSpanID 生成 16 位小写十六进制 span id（OTel spanId 形态）。
func newSpanID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
}

// sessionScope 是「同一条下游会话」内恒定的链路标识。
//
// 官方客户端把链路 ID 分成两个作用域（源码 chat_headers_full 实测）：
//
//	ey[CONVERSATION_ID_HEADER]         = ew.id                    // 会话
//	ey[CONVERSATION_REQUEST_ID_HEADER] = ew.conversationRequestId  // 会话
//	ey[CONVERSATION_MESSAGE_ID_HEADER] = ew.messageId              // 每请求
//	ey[REQUEST_ID_HEADER]              = ew.messageId              // 每请求
//
// 9 条会话 / 19 次请求的抓包全部满足：X-Conversation-Request-ID、X-Root-Request-ID、
// X-Trace-ID 三者同值且会话内恒定；X-Request-ID == X-Conversation-Message-ID 且每请求变化。
// 网关此前每次请求都新生成前一组，导致「同一会话的连续请求携带互不相同的会话链路 ID」，
// 这是比对两次请求即可判定的机器特征。
//
// conversationRequestID 由会话键派生（而非随机生成后缓存），原因有三：
//   - 无需跨请求共享可变状态，无并发问题、无 TTL、无容量上限、重启后仍稳定；
//   - 不泄露会话键本身（客户端传 32 位 hex，网关发出的同样是 32 位 hex）；
//   - 不同会话键必然得到不同 ID，符合「会话隔离」语义。
type sessionScope struct {
	conversationID        string
	conversationRequestID string
}

// newSessionScope 解析会话级链路 ID。
//
// 优先级（与「客户端透传优先」一致）：
//  1. 下游给出 X-Conversation-Request-ID 时直接采用 —— 它本身就是会话级恒定值，
//     采用它才能保证 X-Conversation-Request-ID == X-Root-Request-ID == X-Trace-ID 恒成立；
//  2. 仅给出 X-Conversation-ID 时，由它哈希派生出会话级请求 ID；
//  3. 两者都缺失时返回零值，调用方回退到每请求新值 —— 不臆造会话关联，
//     避免把无会话语义的请求错误地归并到同一条链路。
//
// 派生（而非随机生成后缓存）的取舍：无需跨请求共享可变状态，故无并发问题、无 TTL、
// 无容量上限，且重启后仍稳定；不同会话键必然得到不同 ID，符合会话隔离语义。
func newSessionScope(conversationID, conversationRequestID string) sessionScope {
	if conversationRequestID != "" {
		// 下游只带会话请求 ID 时，补一个形态自洽的会话 ID（带连字符 UUID）。
		if conversationID == "" {
			conversationID = derivedUUID(conversationRequestID)
		}
		return sessionScope{conversationID: conversationID, conversationRequestID: conversationRequestID}
	}
	if conversationID == "" {
		return sessionScope{}
	}
	sum := sha256.Sum256([]byte(sessionScopeSeed + conversationID))
	// 取前 16 字节（32 位 hex），与客户端 X-Conversation-Request-ID 同形态。
	return sessionScope{
		conversationID:        conversationID,
		conversationRequestID: hex.EncodeToString(sum[:16]),
	}
}

// sessionScopeSeed 是派生用的域分隔前缀，避免与其它哈希用途产生同值。
const sessionScopeSeed = "workbuddy-gateway/session/"

// derivedUUID 由会话键派生一个带连字符的 UUID 形态标识（不用于承载任何真实语义，
// 仅当下游缺失 X-Conversation-ID 时保持该头的形态恒定）。
func derivedUUID(key string) string {
	sum := sha256.Sum256([]byte(sessionScopeSeed + "conv-id/" + key))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// ok 表示会话键存在，会话级 ID 可用；否则调用方应回退到每请求新值。
func (s sessionScope) ok() bool { return s.conversationRequestID != "" }

// buildUserAgent 复刻客户端的 UA 组装口径：
// `${platform}/${platformVersion} ${productName}/${productVersion} ${userAgentExtension}`。
// 真实桌面客户端实测值为 `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1`。
func buildUserAgent(prof *upstreamProfile) string {
	return fmt.Sprintf("%s/%s %s AI/%s CLI/%s",
		prof.PlatformName, prof.PlatformVersion,
		prof.PlatformName, prof.PlatformVersion,
		prof.CliVersion)
}

// setHeaderExact 以调用方给定的大小写逐字写入 Header，绕过 net/http 的 MIME 规范化。
//
// http.Header.Set 会经 textproto.CanonicalMIMEHeaderKey 归一化键名，把 `ID` 变成 `Id`、
// `IDE` 变成 `Ide`、`TraceId` 变成 `Traceid`。HTTP/1.1 在线上按 map 中存储的键名逐字发送
// （Header.Write 不重新规范化），因此规范化后的键名会与官方客户端产生可静态识别的偏差：
//
//	客户端           网关（规范化后）
//	X-Request-ID  →  X-Request-Id
//	X-Trace-ID    →  X-Trace-Id
//	X-IDE-Type    →  X-Ide-Type
//	X-B3-TraceId  →  X-B3-Traceid
//	x-requested-with → X-Requested-With
//
// 直接写入 map 可保留客户端原始大小写。
func setHeaderExact(h http.Header, name, value string) {
	h[name] = []string{value}
}

// getHeaderExact 读取 setHeaderExact 写入的 Header：先按原始键名精确命中，
// 再回退到规范化查找（兼容由 net/http 服务端解析器规范化过的输入）。
func getHeaderExact(h http.Header, name string) string {
	if v, ok := h[name]; ok && len(v) > 0 {
		return v[0]
	}
	return h.Get(name)
}

// linkIDs 是一次上游请求要写入的全部链路 ID，已按客户端不变量两两对齐。
//
// 不变量（源码 + 抓包实测）：
//
//	traceID      : X-Trace-ID == X-Conversation-Request-ID == X-Root-Request-ID
//	               == X-B3-TraceId == traceparent 的 trace 段 == b3 的 trace 段
//	requestID    : X-Request-ID == X-Conversation-Message-ID
//	spanID       : traceparent 的 span 段 == b3 的 span 段 == X-B3-SpanId
//	parentSpanID : b3 的 parent 段 == X-B3-ParentSpanId
type linkIDs struct {
	conversationID string
	requestID      string
	traceID        string
	spanID         string
	parentSpanID   string
}

// resolveLinkIDs 决定本次请求的链路 ID，优先级：下游提供的值 > 会话派生值 > 新生成。
//
// 关键点：同一组内**任一项**由下游提供，则该组整体采用该值。客户端保证组内相等，
// 若只透传其中一项、其余另生成，网关反而会制造出客户端不可能产生的组合
// （例如 X-Request-ID 与 X-Conversation-Message-ID 不等、X-B3-SpanId 与 traceparent 的
// span 段不等）。
func resolveLinkIDs(in http.Header, sess sessionScope) linkIDs {
	tp := getHeaderExact(in, "traceparent")
	b3 := getHeaderExact(in, "b3")

	// 会话组：下游显式给出的任一项即可确定整组（客户端保证这六项相等）。
	traceID := firstNonEmpty(
		getHeaderExact(in, "X-Trace-ID"),
		getHeaderExact(in, "X-Conversation-Request-ID"),
		getHeaderExact(in, "X-Root-Request-ID"),
		getHeaderExact(in, "X-B3-TraceId"),
		traceIDFromTraceparent(tp),
		traceIDFromB3(b3),
		sess.conversationRequestID,
	)
	if traceID == "" {
		traceID = newTraceID()
	}

	// 每请求组。
	requestID := firstNonEmpty(
		getHeaderExact(in, "X-Request-ID"),
		getHeaderExact(in, "X-Conversation-Message-ID"),
	)
	if requestID == "" {
		requestID = newTraceID()
	}

	// span 组（每请求级）：traceparent 的 span 段 == b3 的 span 段 == X-B3-SpanId。
	spanID := firstNonEmpty(
		spanIDFromTraceparent(tp),
		spanIDFromB3(b3),
		getHeaderExact(in, "X-B3-SpanId"),
	)
	if spanID == "" {
		spanID = newSpanID()
	}

	// parent span 组（每请求级）：b3 的 parent 段 == X-B3-ParentSpanId。
	parentSpanID := firstNonEmpty(
		parentSpanIDFromB3(b3),
		getHeaderExact(in, "X-B3-ParentSpanId"),
	)
	if parentSpanID == "" {
		parentSpanID = newSpanID()
	}

	conversationID := firstNonEmpty(getHeaderExact(in, "X-Conversation-ID"), sess.conversationID)
	if conversationID == "" {
		conversationID = uuid.New().String()
	}

	return linkIDs{
		conversationID: conversationID,
		requestID:      requestID,
		traceID:        traceID,
		spanID:         spanID,
		parentSpanID:   parentSpanID,
	}
}

// firstNonEmpty 返回首个非空值；全为空时返回空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// isHexLen 判断 s 是否为 n 位小写十六进制。
func isHexLen(s string, n int) bool {
	return len(s) == n && strings.Trim(s, "0123456789abcdef") == ""
}

// traceIDFromTraceparent 从 W3C traceparent 头中取出 trace id。
//
// 格式 `00-<32hex traceId>-<16hex spanId>-<2hex flags>`。客户端实测 X-Trace-ID 与
// traceparent 的 trace 段相等，故下游只带 traceparent 时也应据此对齐，
// 否则网关会生成一个与 traceparent 不一致的 X-Trace-ID —— 客户端不会产生该组合。
func traceIDFromTraceparent(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || !isHexLen(parts[1], 32) {
		return ""
	}
	return parts[1]
}

// spanIDFromTraceparent 从 traceparent 中取出 span id。
func spanIDFromTraceparent(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || !isHexLen(parts[2], 16) {
		return ""
	}
	return parts[2]
}

// traceIDFromB3 从 B3 单头（`<traceId>-<spanId>-<sampled>-<parentSpanId>`）取出 trace id。
func traceIDFromB3(b3 string) string {
	parts := strings.Split(b3, "-")
	if len(parts) < 3 || !isHexLen(parts[0], 32) {
		return ""
	}
	return parts[0]
}

// spanIDFromB3 从 B3 单头取出 span id。
func spanIDFromB3(b3 string) string {
	parts := strings.Split(b3, "-")
	if len(parts) < 3 || !isHexLen(parts[1], 16) {
		return ""
	}
	return parts[1]
}

// parentSpanIDFromB3 从 B3 单头取出 parent span id（第 4 段，可选）。
func parentSpanIDFromB3(b3 string) string {
	parts := strings.Split(b3, "-")
	if len(parts) < 4 || !isHexLen(parts[3], 16) {
		return ""
	}
	return parts[3]
}

// clientConversationIDs 从下游入站 Header 中取出会话标识（会话 ID、会话请求 ID）。
//
// 官方客户端两者都发；非官方客户端可能只带其一，故分别返回，由 newSessionScope 决定键。
//
// 经 getHeaderExact 读取：下游入站键名已被 net/http 规范化为 X-Conversation-Id，
// 而精确键名不存在时 getHeaderExact 会回退到 Header.Get。
func clientConversationIDs(in http.Header) (string, string) {
	if in == nil {
		return "", ""
	}
	return getHeaderExact(in, "X-Conversation-ID"), getHeaderExact(in, "X-Conversation-Request-ID")
}

// setTraceHeaders 写入 OTel + B3 双份链路传播头（客户端两套同时发送）。
//
// 两套传播头必须与 ids 保持一致：客户端实测 X-Trace-ID == traceparent 的 trace 段 ==
// b3 的 trace 段 == X-B3-TraceId，span 段同理。若下游已提供其中任一项（透传路径），
// ids 会带回该值，此处只是把同一组值铺到全部同义头上。
// 键名大小写与客户端逐字一致，见 setHeaderExact。
func setTraceHeaders(req *http.Request, ids linkIDs) {
	setHeaderExact(req.Header, "X-Request-ID", ids.requestID)
	setHeaderExact(req.Header, "X-Trace-ID", ids.traceID)
	setHeaderExact(req.Header, "traceparent", fmt.Sprintf("00-%s-%s-01", ids.traceID, ids.spanID))
	setHeaderExact(req.Header, "b3", fmt.Sprintf("%s-%s-1-%s", ids.traceID, ids.spanID, ids.parentSpanID))
	setHeaderExact(req.Header, "X-B3-TraceId", ids.traceID)
	setHeaderExact(req.Header, "X-B3-ParentSpanId", ids.parentSpanID)
	setHeaderExact(req.Header, "X-B3-SpanId", ids.spanID)
	setHeaderExact(req.Header, "X-B3-Sampled", "1")
}

// clientPassthroughHeaders 是允许从下游客户端原样透传到上游的 Header 白名单。
//
// 仅包含「客户端身份 / 会话链路 / SDK 指纹」这类与账号凭据无关的字段。鉴权类字段
// （Authorization / X-User-Id / X-Enterprise-Id / X-Domain / X-Refresh-Token / X-Product
// / Cookie）一律不在此列——它们必须由网关按当前账号池中的账号生成，透传会串号或泄权。
//
// 语义：客户端真实携带时原样转发（避免网关臆造值造成特征偏差）；未携带时由
// backendHeaders 回退到合成值。
//
// 注意：链路 ID 类头（X-Conversation-*、X-Root-Request-ID、X-Trace-ID、X-Request-ID、
// traceparent、b3、X-B3-*）**不在此列**，它们由 resolveLinkIDs 统一决定。原因是这些头
// 之间存在客户端保证的相等关系（如 traceparent 的 span 段 == b3 的 span 段 == X-B3-SpanId）。
// 逐头原样透传时，若下游只提供了其中一部分，网关会为其余头另生成值，从而产出客户端
// 不可能产生的组合 —— 比缺失更显眼。resolveLinkIDs 采用下游值并按不变量补齐同组伙伴，
// 既保留了下游语义，又保证输出自洽，故这些头必须只有一个写入方。
//
// 键名大小写与官方客户端实测逐字一致（客户端 SDK 混用大写驼峰与全小写，
// 网关必须复现该大小写，见 setHeaderExact）。查找时经 getHeaderExact 兼容
// net/http 服务端已规范化过的输入键名。
var clientPassthroughHeaders = []string{
	// Agent 语义
	"X-Agent-Type",
	"X-Agent-Intent",
	"X-Agent-Purpose",
	// 宿主 IDE 标识
	"X-IDE-Type",
	"X-IDE-Name",
	"X-IDE-Version",
	// 隐私通道与本地安全头
	"X-Private-Data",
	"x-codebuddy-request",
	// B3 采样标志（链路 ID 本身由 resolveLinkIDs 决定，见上）
	"X-B3-Sampled",
	"x-requested-with",
	"User-Agent",
	"Accept",
}

// applyClientPassthrough 把下游客户端携带的白名单 Header 原样复制到上游请求。
// 客户端未提供对应头时不写入，交由调用方回退合成值。
func applyClientPassthrough(dst *http.Request, in http.Header) {
	if in == nil {
		return
	}
	for _, name := range clientPassthroughHeaders {
		if v := getHeaderExact(in, name); v != "" {
			setHeaderExact(dst.Header, name, v)
		}
	}
	// OpenAI SDK（stainless）指纹族按前缀整体透传。客户端发出的键名为全小写，
	// 故透传时同样写全小写（Go 服务端已把入站键名规范化为 X-Stainless-*，
	// 这里统一还原为客户端实际发送的小写形态）。
	for name, vals := range in {
		if len(vals) == 0 || !strings.HasPrefix(strings.ToLower(name), "x-stainless-") {
			continue
		}
		setHeaderExact(dst.Header, strings.ToLower(name), vals[0])
	}
}

// stainlessFingerprint 复刻官方客户端 chat 链路的 OpenAI Node SDK（stainless）指纹族。
// 客户端 chat 请求由 bundled openai SDK 发出，因此上游始终能看到这一组头；
// 网关自身用 Go net/http 发起请求，若不合成即为可识别的缺失特征。
// 取值来自真实抓包（WorkBuddyAI desktop 5.5.2 / Node v22.22.2）。
// 键名为全小写：openai SDK 以全小写发出这一族头，上游按原样可见。
var stainlessFingerprint = map[string]string{
	"x-stainless-arch":            "x64",
	"x-stainless-lang":            "js",
	"x-stainless-os":              "Windows",
	"x-stainless-package-version": "6.25.0",
	"x-stainless-retry-count":     "0",
	"x-stainless-runtime":         "node",
	"x-stainless-runtime-version": "v22.22.2",
}

// backendHeaders 设置上游专用鉴权 Header 与链路追踪 Header（按站点 Profile 生成）。
//
// 与真实客户端实测流量的对齐要点：
//   - 不发送 X-Client-ID / X-Client-Version：官方客户端全库无此字段，属可静态识别的机器特征。
//   - X-Request-ID 为 32 位 hex（客户端 generateUUUID().replace(/-/g,"")），
//     与 X-Trace-ID 解耦：后者是 OTel traceId，全链路同一值。
//   - 补齐会话/Agent/IDE 骨架、OTel + B3 双份传播头与 stainless 指纹族，缺失同样是可识别特征。
//   - 下游客户端自带身份头时优先透传（applyClientPassthrough），仅在缺失时合成。
//
// 链路 ID 分两个作用域（见 sessionScope）：会话级由 sess 提供，每请求级在此新生成。
// 下游若已提供其中任一项，则采用下游值，并按客户端的不变量补齐其同组伙伴。
func backendHeaders(req *http.Request, sa *StoredAuth, prof *upstreamProfile, in http.Header, sess sessionScope) {
	commonHeaders(req, prof)

	ids := resolveLinkIDs(in, sess)
	setTraceHeaders(req, ids)

	setHeaderExact(req.Header, "X-Conversation-Message-ID", ids.requestID)
	setHeaderExact(req.Header, "X-Conversation-Request-ID", ids.traceID)
	setHeaderExact(req.Header, "X-Root-Request-ID", ids.traceID)
	setHeaderExact(req.Header, "X-Conversation-ID", ids.conversationID)

	// Agent 语义头
	setHeaderExact(req.Header, "X-Agent-Type", agentType)
	setHeaderExact(req.Header, "X-Agent-Intent", agentIntent)
	setHeaderExact(req.Header, "X-Agent-Purpose", agentPurpose)

	// 隐私通道标记：客户端 enableModelOptimization 未启用时为 "true"
	// （国际站 product.json 的 DisableYuanbaoChannel=true ⇒ 恒为 true）。
	setHeaderExact(req.Header, "X-Private-Data", "true")

	// OpenAI SDK 指纹族
	for k, v := range stainlessFingerprint {
		setHeaderExact(req.Header, k, v)
	}

	// 客户端自带身份/链路头优先（覆盖上面的合成值）
	applyClientPassthrough(req, in)

	// 鉴权与账号标识。实测两种形态：
	//   - 已鉴权：Authorization + X-User-Id + X-Domain + X-Product（不发任何 X-No-*）
	//   - 未鉴权（如 auth/state 轮询）：仅 X-No-* 置 "true"
	// 因此 X-No-* 只在缺少对应凭据时成组出现，不与已鉴权字段混发。
	authed := sa != nil && sa.Auth.AccessToken != ""
	if authed {
		setHeaderExact(req.Header, "Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		setHeaderExact(req.Header, "X-No-Authorization", "true")
	}

	if sa != nil && sa.Account.UID != "" {
		setHeaderExact(req.Header, "X-User-Id", sa.Account.UID)
	} else {
		setHeaderExact(req.Header, "X-No-User-Id", "true")
	}

	if sa != nil && sa.Account.EnterpriseID != "" {
		setHeaderExact(req.Header, "X-Enterprise-Id", sa.Account.EnterpriseID)
	} else if !authed {
		setHeaderExact(req.Header, "X-No-Enterprise-Id", "true")
		setHeaderExact(req.Header, "X-No-Department-Info", "true")
	}

	// 注意：X-Refresh-Token 只出现在 auth/token/refresh 与 account/switch 链路
	// （客户端源码 enterpriseHeaders 之外单独注入），chat 请求实测不携带，故此处不设置。

	// X-Domain 是 chat 请求的常驻头（客户端实测始终携带），无凭据时回退站点默认域名。
	if sa != nil && sa.Auth.Domain != "" {
		setHeaderExact(req.Header, "X-Domain", sa.Auth.Domain)
	} else {
		setHeaderExact(req.Header, "X-Domain", clientDomain)
	}

	setHeaderExact(req.Header, "X-Product", prof.Product)
}

// commonHeaders 按站点 Profile 设置通用 Header。
//
// 与真实客户端对齐：Node/Electron 运行时不会产生浏览器语义的 Origin / Referer，
// 客户端实测亦不发送；发送它们反而构成与官方客户端不一致的特征。
// Accept 客户端实测为单一 application/json（非浏览器默认的 */* 列表）。
func commonHeaders(req *http.Request, prof *upstreamProfile) {
	setHeaderExact(req.Header, "Content-Type", "application/json")
	setHeaderExact(req.Header, "Accept", "application/json")
	// 客户端实测为全小写 `x-requested-with`（axios 默认头，未经规范化）
	setHeaderExact(req.Header, "x-requested-with", "XMLHttpRequest")
	setHeaderExact(req.Header, "User-Agent", buildUserAgent(prof))
	setHeaderExact(req.Header, "X-IDE-Type", prof.PlatformName)
	setHeaderExact(req.Header, "X-IDE-Name", prof.PlatformName)
	setHeaderExact(req.Header, "X-IDE-Version", prof.PlatformVersion)
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	return doJSONContext(context.Background(), client, method, fullURL, headers, body)
}

func doJSONContext(ctx context.Context, client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req, &profileCN) // 兜底默认（当前所有调用方均显式传入站点 Profile）
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %s", resp.StatusCode, string(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("解析上游 JSON 失败: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// 流式聚合与格式清理
// -----------------------------------------------------------------------------

// mergedToolCall 累积合并上游按 index 分片下发的工具调用增量。
type mergedToolCall struct {
	ID   string
	Type string
	Name string
	Args strings.Builder
}

// applyToolCallDelta 将上游 tool_calls 增量按 index 归并进 map；order 记录首次出现的
// index 顺序，保证最终输出顺序稳定。Chat Completions 聚合与 Responses 流式共用。
func applyToolCallDelta(toolCalls map[int]*mergedToolCall, order *[]int, tcs []any) {
	for _, tcAny := range tcs {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		st, exists := toolCalls[idx]
		if !exists {
			st = &mergedToolCall{Type: "function"}
			toolCalls[idx] = st
			*order = append(*order, idx)
		}
		if id, ok := tc["id"].(string); ok && id != "" {
			st.ID = id
		}
		if t, ok := tc["type"].(string); ok && t != "" {
			st.Type = t
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				st.Name = n
			}
			if a, ok := fn["arguments"].(string); ok && a != "" {
				st.Args.WriteString(a)
			}
		}
	}
}

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					applyToolCallDelta(toolCalls, &toolOrder, tcs)
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	message := map[string]any{"role": ifEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			st := toolCalls[idx]
			id := st.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), idx)
			}
			calls = append(calls, map[string]any{
				"id":    id,
				"type":  ifEmpty(st.Type, "function"),
				"index": idx,
				"function": map[string]any{
					"name":      st.Name,
					"arguments": st.Args.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      ifEmpty(respID, fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": created,
		"model":   ifEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": ifEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

// cleanChunkJSON 清洗单个上游 chat 分片，使其符合 OpenAI 流式分片规范后再透传。
//
// 上游是「类 OpenAI」实现，分片里带有若干偏离规范的噪声。普通 OpenAI 客户端能忍，
// 但 Anthropic 协议翻译层（Claude Code 链路）会据此误判流状态，导致工具不执行：
//
//  1. finish_reason:"" → null（本 bug 的直接原因）。规范要求中间分片为 null、只有终止分片
//     给出真实原因；上游却在整条流的每一个分片上都下发 finish_reason:""。
//     翻译层会取流中「第一个非 null 的 finish_reason」作为最终 stop_reason，
//     于是首个分片就把 stop_reason 锁成 end_turn，真实终止分片的 "tool_calls" 不再被采纳。
//     Claude Code 只在 stop_reason=tool_use 时才执行工具，表现为「网关日志成功但客户端没结果」。
//  2. 旧版 function_call 空壳：上游在含工具调用的终止片追加
//     {"function_call":{"name":"","arguments":""}}，属同类非规范噪声，一并清除。
//  3. delta 中的空值字段（content:""、tool_calls:[] 等）。
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		// SSE 元数据及非 JSON 行不能作为 Chat Completions 数据事件发出。
		return ""
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			// 空 finish_reason 归一化为 null：只有终止分片才应携带真实原因。
			if fr, ok := choice["finish_reason"].(string); ok && fr == "" {
				choice["finish_reason"] = nil
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if k == "function_call" && isEmptyFunctionCall(v) {
						delete(delta, k)
						continue
					}
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// isEmptyFunctionCall 判断是否为旧版 function_call 空壳（name 与 arguments 均为空）。
// 真正的旧版函数调用会带 name 或 arguments，不应误删。
func isEmptyFunctionCall(v any) bool {
	fc, ok := v.(map[string]any)
	if !ok {
		return false
	}
	name, _ := fc["name"].(string)
	args, _ := fc["arguments"].(string)
	return strings.TrimSpace(name) == "" && strings.TrimSpace(args) == ""
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    statusCode,
		},
	})
}

func ifEmpty(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
