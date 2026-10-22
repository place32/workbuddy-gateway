package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
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

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/skip2/go-qrcode"
)

const (
	version = "1.14.24"

	// 状态快照文件名：serve 后台周期写入，monitor 前台命令实时读取展示
	statusSnapshotFile = "workbuddy-status.json"
	logDir             = "logs"

	// defaultSystemPrompt 是当客户端首条消息不是 system 时注入的保底系统提示，
	// 用于满足腾讯上游「首条消息必须是 system prompt」的硬性要求 (code 11128)。
	defaultSystemPrompt = "You are a helpful assistant."

	// upstreamHeaderTimeout 限定「请求发送完成 → 收到上游响应头」的等待上限。
	//
	// 该值必须足够宽松：上游对 3MB+ 大请求的排队与预处理本身就可能接近 1 分钟
	// （实测 2.3MB 请求出现过 50.8s 才回响应头）。早期版本用 60s 会误杀这类
	// 正常但缓慢的请求，表现为 502「timeout awaiting response headers」。
	// 默认 5 分钟，只用于兜底「上游彻底无响应」；长时间无数据的流由
	// upstreamIdleTimeout 的空闲看门狗负责中断。可在 config.json 覆盖。
	upstreamHeaderTimeoutDefault = 5 * time.Minute

	// upstreamIdleTimeoutDefault 是流式响应的「空闲读超时」：只要持续有数据到达
	// 就永不超时，一旦超过该时长没有任何新数据，才判定上游卡死并中断。
	upstreamIdleTimeoutDefault = 120 * time.Second

	// defaultRequestTimeout 用于令牌刷新、额度查询、模型目录等控制类短请求。
	defaultRequestTimeout = 60 * time.Second

	// upstreamTransientRetries 是「请求体发送阶段」遇到瞬时网络错误的额外重试次数。
	//
	// 背景：网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置
	// （connection reset by peer）、被关闭（use of closed network connection）、
	// 或命中已被回收的 keep-alive 连接。这类错误属于瞬时故障，且与请求体大小无关
	// （实测 >5MB 请求 95% 成功，而 0.5MB 请求也会偶发失败）。
	//
	// 只有确认请求头尚未写出时才重试；没有收到响应头不能证明上游没处理 POST。
	// 重试强制使用新连接，默认最多额外尝试 2 次，可在 config.json 覆盖。
	upstreamTransientRetriesDefault = 2

	// upstreamRetryBackoff 是两次重试之间的等待，给上游边缘节点留出恢复时间。
	upstreamRetryBackoffDefault = 300 * time.Millisecond

	// streamKeepaliveDefault 是等待上游首个有效数据期间向下游发送 SSE 注释的间隔。
	// 腾讯边缘节点对「响应头已发出但长时间无首个字节」的连接较敏感，默认 15s。
	streamKeepaliveDefault = 15 * time.Second
	// enableFirstByteRetryDefault 允许在客户端尚未收到任何有效数据时安全重放一次。
	enableFirstByteRetryDefault = true
)

var errModelAccountPolicy = errors.New("model account policy rejected all accounts")

// errStreamClosedWithoutFinish 表示上游连接正常读完（没有读错误），
// 但整段流里从未出现非空 finish_reason。空串不算：上游会在每个中间分片上带 finish_reason:""。
// 这种结束不能当成完整回复，否则下游会把残缺输出当成成功结果。
var errStreamClosedWithoutFinish = errors.New("上游流正常结束，但没有 finish_reason，不能当成完整回复")

// 生效的超时值（默认取上面的 Default，可由 config.json 的 upstream 段覆盖）。
// 定义为变量既便于测试调小阈值，也便于运维按网络状况调整。
var (
	upstreamHeaderTimeout = upstreamHeaderTimeoutDefault
	upstreamIdleTimeout   = upstreamIdleTimeoutDefault
	// upstreamTransientRetries 为生效的瞬时网络错误重试次数。
	upstreamTransientRetries = upstreamTransientRetriesDefault
	// upstreamRetryBackoff 为生效的重试间隔（测试可调小）。
	upstreamRetryBackoff = upstreamRetryBackoffDefault
	// streamKeepalive 为生效的流式心跳间隔（0 表示关闭）。
	streamKeepalive = streamKeepaliveDefault
	// enableFirstByteRetry 为生效的首字节前重放开关。
	enableFirstByteRetry = enableFirstByteRetryDefault
)

// isTransientNetworkError 判断错误是否为「瞬时网络故障」，这类错误值得重试。
//
// 覆盖：连接被重置/中止（RST）、连接被对端关闭、管道破裂、连接被强制关闭，
// 以及 Go 在复用 keep-alive 连接时常见的 "server closed idle connection"。
// 注意：context.Canceled 与 DeadlineExceeded 不属于此类——前者是客户端主动断开，
// 后者是明确超时，重试都无意义甚至有害。
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// 客户端取消或超时：不重试
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// 网络层超时（如 ResponseHeaderTimeout）说明上游确实没响应，可重试
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"connection refused",
		"server closed idle connection",
		"unexpected eof",
		"http2: server sent goaway",
		"stream error",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// upstreamBody 按站点 Profile 决定模型请求体是否 gzip，返回最终字节与是否已压缩。
//
// 客户端 GzipRequestProcessor 的判定（源码）：仅当 productFeature
// `RequestBodyGzip` 为真、请求体非空、且运行时环境不是 internal 时压缩 —— 与请求体
// 大小无关。国际站 product.json 该特性为 true，抓包实测模型请求带
// `Content-Encoding: gzip`（38KB 请求体）。
func upstreamBody(prof *upstreamProfile, raw []byte) ([]byte, bool) {
	if !prof.GzipBody || len(raw) == 0 {
		return raw, false
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return raw, false
	}
	if err := zw.Close(); err != nil {
		return raw, false
	}
	return buf.Bytes(), true
}

// doUpstreamRequest 发送上游请求，并对瞬时网络错误做有界重试。
//
// 仅在「尚未写出完整请求头」时重试；已发送 POST 后收到 EOF 无法判定上游
// 是否执行过生成，不能安全重放。每次重试都重建请求对象与请求体 reader。
//
// 返回的 cancel 用于在流式读取结束后取消上游 context（调用方负责）。
func doUpstreamRequest(r *http.Request, acc *Account, prof *upstreamProfile, upstreamBytes []byte, reqID uint64, traceID string, sess sessionScope) (*http.Response, context.CancelFunc, error) {
	prepareConversationContext(r, nil)
	profile := requestProfileFor(r)
	wireBytes, encodeErr := encodeModelRequest(upstreamBytes, profile, prof)
	if encodeErr != nil {
		log.Printf("[出站兼容] traceId=%s requestId=%d 阶段=请求编码 结果=失败 业务影响=未发送上游", traceID, reqID)
		debugEvent(r, "error", "upstream_body_encode_failed", map[string]any{"status_code": 502})
		return nil, nil, encodeErr
	}
	var lastErr error
	_, _, retries := upstreamSettings()
	// 请求体压缩在重试循环外只做一次：客户端 GzipRequestProcessor 对每个模型请求体
	// 压缩一次，重试属网关自身的网络层行为，不应改变上游可见的字节。
	gzipped := prof.GzipBody && len(upstreamBytes) > 0
	for try := 0; try <= retries; try++ {
		upstreamCtx, upstreamCancel := context.WithCancel(r.Context())
		var headersWritten, bodyWritten atomic.Bool
		trace := &httptrace.ClientTrace{
			WroteHeaders: func() { headersWritten.Store(true) },
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					bodyWritten.Store(true)
				}
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(upstreamCtx, trace), http.MethodPost, prof.chatURL(), bytes.NewReader(wireBytes))
		if err != nil {
			upstreamCancel()
			return nil, nil, err
		}
		backendHeaders(req, acc.Auth, prof, r.Header, sess)
		applyObservedDesktopHeaders(req, prof, profile)
		if gzipped {
			setHeaderExact(req.Header, "Content-Encoding", "gzip")
		}
		debugEvent(r, "debug", "upstream_request_profile_applied", map[string]any{
			"profile": profile, "desktop_headers": usesObservedDesktopHeaders(profile, prof),
			"decoded_body_bytes": len(upstreamBytes), "wire_body_bytes": len(wireBytes),
			"site": prof.Key, "business_impact": "仅模型出站；保留站点客户端指纹与会话链路，不记录认证或正文",
		})
		if try > 0 {
			// 重试时不复用已被对端关闭的空闲连接。
			req.Close = true
			cfg.HttpClient.CloseIdleConnections()
			log.Printf("[网络重试] traceId=%s requestId=%d 账号=%s 第 %d/%d 次重试，上一尝试请求头未写出，已重建连接（错误: %s）",
				traceID, reqID, acc.Path, try, retries, safeDebugError(lastErr))
		}

		debugFullUpstreamRequest(r, req, upstreamBytes, try)
		resp, err := cfg.HttpClient.Do(req)
		debugFullUpstreamResponse(r, req, resp, try, headersWritten.Load(), bodyWritten.Load(), err)
		if err == nil {
			return resp, upstreamCancel, nil
		}
		upstreamCancel()
		lastErr = err
		if !isTransientNetworkError(err) {
			return nil, nil, err
		}
		if headersWritten.Load() || bodyWritten.Load() {
			log.Printf("[网络重试跳过] traceId=%s requestId=%d 账号=%s 请求头已写出=%t 请求体已写完=%t 原因=上游可能已处理POST，重放可能重复生成 错误=%s",
				traceID, reqID, acc.Path, headersWritten.Load(), bodyWritten.Load(), safeDebugError(err))
			debugEvent(r, "warn", "upstream_retry_skipped", map[string]any{
				"reason": "request_may_have_reached_upstream", "headers_written": headersWritten.Load(),
				"body_written": bodyWritten.Load(), "business_impact": "避免重复执行模型请求",
			})
			return nil, nil, err
		}
		if try < retries {
			debugEvent(r, "warn", "upstream_send_retry", map[string]any{
				"retry": try + 1, "reason": "request_headers_not_written",
				"business_impact": "换新连接重试尚未发出的模型请求",
			})
			select {
			case <-r.Context().Done():
				return nil, nil, err
			case <-time.After(upstreamRetryBackoff):
			}
		}
	}
	return nil, nil, lastErr
}

// -----------------------------------------------------------------------------
// 上游站点 Profile（国内站 / 国际站）
//
// 国际站 www.workbuddy.ai 与国内站 copilot.tencent.com 走同一套 /v2/plugin/* 协议
// （实测：auth/state、auth/token、login/account、auth/token/refresh、chat/completions
// 的路径与响应包络完全一致），差异仅在：
//   - 上游域名与 Web Origin（国际站位于腾讯 EdgeOne 国际 CDN）
//   - 登录 platform 参数（国内站 workbuddy、国际站 workbuddy-ai，均取各站 product.json 的
//     authentication.attributes.platform），国际站在浏览器内完成登录（邮箱/验证码/SSO）
//   - 等待授权期间 auth/token 轮询返回 code 11217 (login ing...)
// 每个账号凭据文件通过 edition 字段记录所属站点，账号池支持国内/国际混挂轮询。
// -----------------------------------------------------------------------------

type upstreamProfile struct {
	Key             string // 存储于凭据文件 edition 字段的站点标识
	Label           string // 控制台展示名
	Base            string // 上游 API 基础地址
	Platform        string // auth/state 的 platform 参数
	PlatformName    string // X-IDE-Type / X-IDE-Name（宿主产品标识）
	PlatformVersion string // X-IDE-Version（宿主产品版本）
	ProductName     string // UA 产品名；空则回退「PlatformName AI」
	AppName         string // 桌面 UA 的 platform / applicationName；空则用 PlatformName
	CliVersion      string // 桌面 UA 尾段（bundled CLI 版本）；CLI 宿主本身为空
	Product         string // X-Product
	// NodeVersion 是 x-stainless-runtime-version：**运行 bundled OpenAI SDK 的 Node 进程**版本。
	// 桌面宿主以 ELECTRON_RUN_AS_NODE=1 用自带 Electron 启动 CLI，故该值随宿主客户端版本走。
	// 两站当前实测同值：两个安装的 Electron 均为 37.10.3 → v22.21.1（国内站 5.5.3、国际站 5.7.6）。
	NodeVersion string // x-stainless-runtime-version
	// AcceptEncoding 是请求携带的压缩协商头；空串表示**不发送该头**（国内站实测如此）。
	// 该头由客户端 Node http 适配器补齐，值随 bundled CLI 版本走：国际站 CLI 2.156.0 发
	// `gzip, compress, deflate, br`（控制面与模型请求都发），国内站 CLI 2.137.1 两族都不发。
	AcceptEncoding string
	PortalOrigin   string        // 各站 Web 控制台源（仅用于 billing/额度等 Web API 的 URL 拼装，不作为请求头发送）
	LoginTTL       time.Duration // login 命令等待授权完成的超时
	// GzipBody 表示模型请求体是否 gzip 压缩（Content-Encoding: gzip）。
	// 两站当前实测都压缩（国内站 5.5.3 抓包：模型请求带 `Content-Encoding: gzip`，
	// 请求体以 gzip 魔数 1f 8b 开头）。
	GzipBody bool
}

// 客户端身份常量：与真实客户端实测流量对齐（详见 README「客户端指纹对齐」）。
// 国内站取值来源于本机安装的 WorkBuddy desktop 5.5.3（`D:\workbuddy`）+
// bundled CLI 2.137.1：桌面注入的 CLIENT_INFO_* 决定身份，模型请求走本地 sink 抓包实测；
// 国际站取值来源于 WorkBuddyAI desktop 5.7.6 + bundled CLI 2.156.0 的抓包。
// 任何自造字段（如 X-Client-ID）都会形成可静态识别的机器特征。
const (
	// clientDomain 是上游请求携带的 X-Domain 兜底值（账号与站点都没给出域名时使用）。
	// 正常路径取账号凭据里的 domain，其次取站点主机名（见 backendHeaders / profile.domain）。
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
		Base: "https://copilot.tencent.com",
		// auth/state 的 platform 取 product.json authentication.attributes.platform：国内站
		// `workbuddy`（CLI 源码 `configuration.authentication?.attributes?.platform ?? configuration.platform`）。
		Platform: "workbuddy",
		// 桌面注入的 CLIENT_INFO_*（desktop client-info-env 模块）：
		//   CLIENT_INFO_PLATFORM / IDE_TYPE = WORKBUDDY_PLATFORM = "WorkBuddy"
		//   PLATFORM_VERSION / PRODUCT_VERSION / PLUGIN_VERSION = app.getVersion() = 5.5.3
		//   PRODUCT_NAME = bundled product.json productName = "WorkBuddy"
		//   USER_AGENT_EXTENSION = `CLI/${bundled cli version}` = CLI/2.137.1
		// 故 X-IDE-* 为 WorkBuddy / WorkBuddy / 5.5.3，UA 为
		// `WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1`（本机 5.5.3 客户端抓包逐字一致）。
		PlatformName: "WorkBuddy", PlatformVersion: "5.5.3",
		AppName: "WorkBuddy", ProductName: "WorkBuddy",
		CliVersion: "2.137.1", Product: "SaaS",
		// 宿主 Electron 37.10.3 内置 Node（ELECTRON_RUN_AS_NODE=1 实测）。
		NodeVersion: "v22.21.1",
		// 国内站 CLI 2.137.1 的模型请求与控制面请求都不带 Accept-Encoding（抓包实测）。
		AcceptEncoding: "",
		PortalOrigin:   "https://www.codebuddy.cn",
		LoginTTL:       5 * time.Minute,
		// 模型请求体 gzip：5.5.3 抓包实测 `Content-Encoding: gzip` + 请求体 gzip 魔数。
		GzipBody: true,
	}
	profileINTL = upstreamProfile{
		Key: "intl", Label: "国际站",
		Base: "https://www.workbuddy.ai",
		// auth/state 的 platform 取 product.json authentication.attributes.platform。
		Platform: "workbuddy-ai",
		// 桌面宿主注入 bundled CLI 的 CLIENT_INFO_*（Desktop 源码 buildWorkbuddyClientInfoEnv）：
		//   CLIENT_INFO_PLATFORM/IDE_TYPE = WorkBuddy
		//   CLIENT_INFO_PLATFORM_VERSION/PRODUCT_VERSION/PLUGIN_VERSION = app.getVersion() = 5.7.6
		//   CLIENT_INFO_PRODUCT_NAME = product.json productName = WorkBuddy AI
		//   CLIENT_INFO_USER_AGENT_EXTENSION = CLI/<bundled cli version> = CLI/2.156.0
		// 因此 X-IDE-* 为 WorkBuddy / WorkBuddy / 5.7.6，UA 为
		// `WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0`（抓包逐字一致）。
		PlatformName: "WorkBuddy", PlatformVersion: "5.7.6",
		AppName: "WorkBuddy", ProductName: "WorkBuddy AI", CliVersion: "2.156.0",
		Product: "SaaS",
		// 5.7.6 内置 Electron 37.10.3 → 自带 Node 22.21.1（ELECTRON_RUN_AS_NODE 实测）。
		NodeVersion: "v22.21.1",
		// 国际站 CLI 2.156.0 由 Node http 适配器补写该头（axios 默认值），控制面与模型
		// 请求都带；线上字节抓包实测位于 Content-Length 与 Host 之间。
		AcceptEncoding: "gzip, compress, deflate, br",
		PortalOrigin:   "https://www.workbuddy.ai",
		LoginTTL:       15 * time.Minute, // 浏览器内登录（邮箱/验证码/SSO）比扫码慢，放宽超时
		// 模型请求体 gzip（抓包实测 Content-Encoding: gzip）。
		GzipBody: true,
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

// domain 返回站点 endpoint 的主机名。
//
// 客户端 enterpriseHeaders() 在账号尚无 domain 时用 URI.parse(endpoint).authority 作为
// X-Domain（登录链路每步都带该头），故登录/续期请求的 X-Domain 取站点主机名。
func (p *upstreamProfile) domain() string {
	if u, err := url.Parse(p.Base); err == nil && u.Host != "" {
		return u.Host
	}
	return clientDomain
}

func (p *upstreamProfile) chatURL() string { return p.Base + "/v2/chat/completions" }

func (p *upstreamProfile) quotaSummaryURL() string {
	return p.PortalOrigin + "/billing/meter/get-user-resource-summary"
}

func (p *upstreamProfile) enterpriseUsageURL() string {
	// 外部项目的企业号实测表明，get-user-resource* 对 enterpriseId 返回空包，
	// 必须改问分配额度：credit=本周期已用，limitNum=分配额度。
	return p.PortalOrigin + "/v2/billing/meter/get-enterprise-user-usage"
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
	// 以下为本地续期元数据，官方客户端不读；旧凭据文件缺省为 0，不影响解析。
	RefreshExpiresAt int64 `json:"refreshExpiresAt,omitempty"` // 刷新令牌到期时间（Unix 秒），0=未知
	LastRefreshTime  int64 `json:"lastRefreshTime,omitempty"`  // 最近一次续期成功时间（Unix 秒），0=从未续期
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
	Packages []quotaPackage `json:"Packages"`
	// IsPaidUser 为上游「正式付费订阅」标记；Pro 试用用户该字段同样为 false。
	IsPaidUser bool `json:"IsPaidUser"`
	// ProTrialStatus 不能单独证明试用仍有效；必须结合当前权益资源。
	ProTrialStatus any `json:"ProTrialStatus"`
	// SubscriptionPackageCode 为当前订阅包编码，非空表示存在订阅包。
	SubscriptionPackageCode string `json:"SubscriptionPackageCode"`
}

// -----------------------------------------------------------------------------
// 全局状态与配置
// -----------------------------------------------------------------------------

type Config struct {
	Addr               string
	Port               int
	PortExplicit       bool
	WebPort            int
	AdminKey           string
	AuthFile           string
	AuthDir            string
	AuthExplicit       bool // 用户是否显式指定了 -auth（未指定时自动扫描目录下所有 workbuddy*.json）
	LoginIntl          bool // login -intl：登录国际站 (www.workbuddy.ai，浏览器内完成登录)
	APIKey             string
	ProxyURL           string
	Verbose            bool
	DebugEnabled       bool   // 仅由工作目录 config.json 的 debug.enabled 控制
	ReloadInterval     int    // 账号池热加载扫描间隔（秒），0 关闭
	MonitorInterval    int    // monitor 状态刷新间隔（秒）
	LogFile            string // monitor 附加展示的日志文件路径
	JournalService     string // monitor 附加展示的 systemd 服务名（journalctl -u）
	LogLines           int    // monitor 展示的最近日志行数
	ModelsRefresh      int    // 官方模型目录刷新间隔（分钟），0 关闭
	AllowPriceProbes   bool   // 仅显式开启后台价格探测；真实生成请求可能消耗积分
	DisablePriceProbes bool   // 旧禁用参数兼容，优先于 AllowPriceProbes；不影响客户端请求及显式 probe 命令
	KeepaliveHours     []int  // 主动续期时刻（本地小时），空表示关闭；到点主动刷新全部账号
	ProbeModels        string // probe 专用：逗号分隔的模型列表
	ProbeLimit         int    // probe 专用：未显式指定模型时的取用数量
	WebUI              bool   // 仅显式 -webui 才启动独立管理端口
	HttpClient         *http.Client
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
	IsPaidUser     bool                          // 是否为付费用户（上游 IsPaidUser 原值）
	PlanLabel      string                        // 官方套餐名称或已知套餐短名称
	PlanCheckedAt  int64                         // 最近成功校验权益的时间；旧快照为0
	PlanStale      bool                          // 最近查询失败，展示时标记旧结果
	QuotaKnown     bool                          // 是否已成功获取过额度
	QuotaExhausted bool                          // 已确认额度为 0；额度扫描发现恢复后自动解除
	QuotaUnlimited bool                          // 企业版 limitNum=-1：分配额度不限，禁止按 0 余额阻断
	ModelStates    map[string]*modelRuntimeState // 按模型隔离的成本、限流和额度阻断状态
	fingerprint    string                        // 凭据文件变更指纹（mtime+size，凭据热加载用）
	lock           sync.Mutex                    // 单账号串行锁（防止同账号并发触发 11128）
	// 续期失败计数（仅内存，重启清零）：单次失败可能是上游抖动，连续失败才判定登录态失效。
	RefreshFailCount int
	LastRefreshError string
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

func registerAutomaticPriceProbeFlags(fs *flag.FlagSet, target *Config) {
	fs.BoolVar(&target.AllowPriceProbes, "allow-price-probes", false, "允许后台自动价格探测（默认关闭；真实生成请求可能消耗积分）")
	// 兼容旧 systemd / 启动脚本，但不再出现在主要用法说明中。
	// 即使传入 -disable-price-probes=false，也不能绕过显式开启的要求。
	fs.BoolVar(&target.DisablePriceProbes, "disable-price-probes", false, "")
}

func (c Config) automaticPriceProbesAllowed() bool {
	// 两个参数同时出现时，禁用优先，且不依赖命令行参数的排列顺序。
	return c.AllowPriceProbes && !c.DisablePriceProbes
}

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
	// legacyAPIKeyFlag 仅用于吞掉旧启动命令里的 -api-key，避免升级后报未知参数而启动失败。
	// 该值不会被任何逻辑读取，也不会产生任何日志或状态变化；用法说明留空，
	// 使其不出现在 --help 与默认参数列表中。
	// TODO(remove): 纯兼容占位，计划在后续版本连同下面这行注册一起删除。
	var legacyAPIKeyFlag string
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1", "网关监听地址")
	fs.IntVar(&cfg.Port, "port", defaultAPIPort, "API 监听端口（显式传入时覆盖 config.json 的 gateway.apiPort）")
	fs.StringVar(&cfg.AuthFile, "auth", "workbuddy.json", "凭据存储文件路径（支持逗号分隔多个文件实现多账号）")
	fs.StringVar(&cfg.AuthDir, "auth-dir", "", "凭据目录：自动加载目录下所有 workbuddy*.json 作为多账号池")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "可选：上游请求代理 (如 http://127.0.0.1:7890)")
	fs.StringVar(&legacyAPIKeyFlag, "api-key", "", "")
	// 自定义参数错误提示，避免默认 PrintDefaults 把兼容占位参数列出来。
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "参数有误，请运行 %s --help 查看可用参数\n", os.Args[0])
	}
	fs.BoolVar(&cfg.Verbose, "verbose", false, "输出详细调试日志")
	fs.BoolVar(&cfg.LoginIntl, "intl", false, "login 专用：登录国际站 (www.workbuddy.ai，浏览器内完成登录)；默认登录国内站")
	fs.IntVar(&cfg.MonitorInterval, "interval", 3, "monitor 状态刷新间隔（秒）")
	fs.IntVar(&cfg.ReloadInterval, "reload-interval", 5, "账号池热加载扫描间隔（秒），0 关闭：运行期自动发现新增/更新/删除的凭据文件，免重启")
	fs.StringVar(&cfg.LogFile, "logfile", "", "monitor 附加跟随的日志文件路径（如 -logfile /var/log/workbuddy-gateway.log）")
	fs.StringVar(&cfg.JournalService, "journal", "", "monitor 附加跟随的 systemd 服务名（Linux 下用 journalctl -u <服务> -f 跟随）")
	fs.IntVar(&cfg.LogLines, "lines", 15, "monitor 每次刷新展示的最近日志行数")
	fs.IntVar(&cfg.ModelsRefresh, "models-refresh", 60, "模型目录刷新间隔（分钟），0 关闭（实时接口 + npm 合并）")
	registerAutomaticPriceProbeFlags(fs, &cfg)
	keepaliveHours := hourList{22}
	fs.Var(&keepaliveHours, "keepalive-hours", "主动续期时刻（本地小时，逗号分隔，默认 22）；留空关闭")
	fs.StringVar(&cfg.ProbeModels, "models", "", "probe 专用：逗号分隔的待探测模型（默认取目录前几个）")
	fs.IntVar(&cfg.ProbeLimit, "limit", 5, "probe 专用：未指定 -models 时探测的模型数量上限")
	fs.BoolVar(&cfg.WebUI, "webui", false, "启用独立网页控制台；管理 Key、API 鉴权与端口由 config.json 配置")
	_ = fs.Parse(args)
	cfg.KeepaliveHours = []int(keepaliveHours)

	// 检测 -auth 是否被显式指定：
	// 若未指定 -auth 且未指定 -auth-dir，则自动扫描当前目录下所有 workbuddy*.json 组成账号池，
	// 这样把多个凭据文件放进工作目录即可自动多账号，无需手写参数。
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auth" {
			cfg.AuthExplicit = true
		}
		if f.Name == "port" {
			cfg.PortExplicit = true
		}
	})
	serveCommand := command == "serve" || command == "run" || command == "start"
	if serveCommand || command == "probe" {
		if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
			fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
			os.Exit(1)
		}
		ignoreLegacyAPIKeyFlag(legacyAPIKeyFlag)
	}

	// 初始化 HTTP 客户端
	initHTTPClient()
	closeLog := initFileLogging(command)
	defer closeLog()
	closeDebug := func() {}
	if serveCommand {
		var err error
		closeDebug, err = initDebugLogging()
		if err != nil {
			log.Fatalf("[Debug] 调试日志初始化失败: %v", err)
		}
	}
	defer closeDebug()

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
	case "reset":
		runReset()
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
	gatewayConfigMu.RLock()
	registerSecrets(cfg.APIKey, cfg.AdminKey, gatewaySettings.APIKey)
	gatewayConfigMu.RUnlock()
	// 文件初始化失败时，控制台仍要隐藏秘密值。
	log.SetOutput(&redactingLogWriter{writer: os.Stderr})
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("[Log] 无法创建日志目录 %s，将仅输出到控制台: %v", logDir, err)
		return func() {}
	}
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	f, err := newDailyLogWriter("gateway", ".log")
	if err != nil {
		log.Printf("[Log] 无法打开日志文件 %s，将仅输出到控制台: %v", path, err)
		return func() {}
	}
	log.SetOutput(&redactingLogWriter{writer: io.MultiWriter(os.Stderr, f)})
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
  reset       清空除登录凭据外的全部本地数据（状态/缓存/日志/失效标记），并重新拉取模型与倍率
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
                    模型目录刷新间隔（默认 60 分钟，0 关闭）
                    （目录来源：实时接口 + npm 静态包，合并去重）
  -allow-price-probes
                    开启后台自动价格探测（默认关闭）；
                    会发起真实模型生成请求，可能消耗积分，请确认后开启；
                    不影响客户端请求、目录刷新及显式 probe 命令
  -keepalive-hours <h,h,...>
                    主动续期时刻（本地小时，逗号分隔，默认 22）
                    到点主动刷新全部账号登录凭据，不等访问令牌临近过期；
                    留空关闭。只调刷新接口，不请求模型、不消耗额度
  -webui            启用独立网页控制台 (默认 http://<addr>:8316/ui/)：
                    展示账号池/模型统计/日志/config.json/凭据（令牌脱敏），
                    并可切换模型 API Key 校验；管理 Key 与端口在 config.json
                    的 gateway 段配置，管理 Key 为空时可在服务器本机首次生成

probe 选项:
  -auth <path>      只探测指定凭据文件（文件名或路径均可）；默认探测全部账号
  -models <m1,m2>   指定要探测的模型；默认取模型目录前几个
  -limit <n>        未指定 -models 时探测的模型数量（默认 5，上限 50）
  -addr/-port       需与运行中的 serve 一致；模型 API 开启 Key 校验时 probe 自动携带 gateway.apiKey
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
	headerTimeout, _, _ := upstreamSettings()
	transport := &http.Transport{
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
		// 压缩协商一律按客户端形状自行写入（模型请求 `gzip, compress, deflate, br`，
		// 控制面 / 守护进程各自一套，见各 Headers 函数），故关闭 transport 的自动行为：
		// 否则 Go 会给未声明的请求补上值不同的 `Accept-Encoding: gzip` 并自动解压响应体。
		// 响应解压由 wrapUpstreamDecoding（模型请求）与 readControlBody（控制面）显式处理。
		DisableCompression: true,
		// 只约束到「收到响应头」为止；流式响应体的长时间传输由 upstreamIdleTimeout 的空闲看门狗保护。
		ResponseHeaderTimeout: headerTimeout,
	}
	if cfg.ProxyURL != "" {
		pURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			log.Fatalf("错误: 无效的代理地址 %s: %v", cfg.ProxyURL, err)
		}
		transport.Proxy = http.ProxyURL(pURL)
	}
	// 头部顺序复刻（见 wireorder.go）：net/http 按规范化键名字典序写头部，官方客户端按
	// 属性插入顺序写，顺序本身即静态特征。此处把钩子挂在拨号层，在 TLS 之上重排头部块。
	installWireOrder(transport)
	// 注意：这里刻意不设置 http.Client.Timeout。
	// 总超时会给整条流设一个硬上限（原为 180s），大请求长时间但持续有输出的流会被中途掐断，
	// 导致上游 usage 丢失、客户端收到不完整响应，并被 CPA 判为 provider 故障而触发 503。
	cfg.HttpClient = &http.Client{
		Transport: &hotConfigTransport{transport: transport},
		Jar:       jar,
	}
}

// idleReadCloser 为上游流式响应体提供「空闲读超时」保护。
// 只要持续有数据到达就不断顺延截止时间；一旦超过 timeout 没有任何新数据，
// 就取消上游请求上下文，使阻塞中的 Read 立即返回错误，从而中断卡死的流。
type idleReadCloser struct {
	rc      io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc
	mu      sync.Mutex
	timer   *time.Timer
	last    time.Time // 最近一次读到数据的时刻
	stopped bool
}

func newIdleReadCloser(rc io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReadCloser {
	b := &idleReadCloser{rc: rc, timeout: timeout, cancel: cancel, last: time.Now()}
	b.mu.Lock()
	b.timer = time.AfterFunc(timeout, b.check)
	b.mu.Unlock()
	return b
}

// check 在计时到期时判断是否真的空闲超时；若期间有新数据到达则按剩余时间续期，
// 避免「数据刚好到达」与「计时器触发」之间的竞态导致误杀活跃流。
func (b *idleReadCloser) check() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	idle := time.Since(b.last)
	if idle >= b.timeout {
		b.stopped = true
		b.cancel()
		return
	}
	b.timer.Reset(b.timeout - idle)
}

func (b *idleReadCloser) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.last = time.Now() // 有数据到达即视为活跃
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleReadCloser) Close() error {
	b.mu.Lock()
	b.stopped = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
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
	registerCredentialSecrets(&sa)

	authLock.Lock()
	currAuth = &sa
	authLock.Unlock()
	return &sa, nil
}

func saveAuth(sa *StoredAuth) error {
	registerCredentialSecrets(sa)
	traceID := newTraceID()
	log.Printf("[凭据保存] traceId=%s 文件=%s 阶段=开始 说明=先写入磁盘，成功后再更新当前内存凭据", traceID, cfg.AuthFile)
	dir := filepath.Dir(cfg.AuthFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		log.Printf("[凭据保存] traceId=%s 阶段=编码 结果=失败 原因=%v 业务影响=当前内存凭据未切换", traceID, err)
		return err
	}
	if err := os.WriteFile(cfg.AuthFile, data, 0600); err != nil {
		log.Printf("[凭据保存] traceId=%s 阶段=写入磁盘 结果=失败 原因=%v 业务影响=当前内存凭据未切换", traceID, err)
		return err
	}
	clearDisabledMarker(cfg.AuthFile)
	authLock.Lock()
	currAuth = sa
	authLock.Unlock()
	log.Printf("[凭据保存] traceId=%s 文件=%s 结果=成功 说明=磁盘写入完成，当前内存凭据已更新", traceID, cfg.AuthFile)
	return nil
}

// saveAuthTo 将凭据写入指定路径（多账号模式使用）。
// 写入成功后清除该路径的失效标记（表示账号已重新登录）。
func saveAuthTo(path string, sa *StoredAuth) error {
	registerCredentialSecrets(sa)
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
	registerCredentialSecrets(&sa)
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
	restoreStatsAccountStatesLocked()

	if len(accounts) == 0 {
		return fmt.Errorf("未找到有效凭据，请先执行 login 命令扫码登录")
	}
	return nil
}

// restoreAccountRuntimeStateLocked 从 monitor 快照恢复额度与模型级账本；调用方持有 accountMu。
func restoreAccountRuntimeStateLocked() {
	data, err := readStatusSnapshotFile()
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
		acc.PlanLabel = state.PlanLabel
		acc.PlanCheckedAt = state.PlanCheckedAt
		acc.PlanStale = state.PlanStale || state.PlanCheckedAt == 0 ||
			time.Now().Unix()-state.PlanCheckedAt >= 300
		if acc.PlanCheckedAt == 0 {
			acc.PlanLabel = planUnknown // 不沿用旧版本仅凭试用标记猜出的标签。
		}
		acc.QuotaKnown = state.QuotaKnown
		acc.QuotaUnlimited = state.QuotaUnlimited
		acc.QuotaExhausted = state.QuotaExhausted && !state.QuotaUnlimited
		if len(state.ModelStates) == 0 {
			continue
		}
		acc.ModelStates = make(map[string]*modelRuntimeState, len(state.ModelStates))
		for model, saved := range state.ModelStates {
			restored := &modelRuntimeState{
				CostClass: saved.CostClass, CooldownUntil: timeFromUnix(saved.CooldownUntil),
				QuotaBlocked: saved.QuotaBlocked, NextProbeAt: timeFromUnix(saved.NextProbeAt),
				LastReason: saved.LastReason, ObservedAt: timeFromUnix(saved.ObservedAt),
			}
			sanitizeRestoredModelState(restored)
			acc.ModelStates[normalizeModelName(model)] = restored
		}
	}
}

// sanitizeRestoredModelState 修正历史版本写入快照的脏数据。
//
// 旧版本在收到 14018（账号额度耗尽）时会错误地把 CostClass 覆盖成 paid，
// 但 14018 只说明该账号没额度，不能证明模型收费。快照里若出现
// 「QuotaBlocked=true + CostClass=paid + 原因是 14018」这一旧版本特征组合，
// 就把收费属性降回 unknown，交给后续 usage.credit 证据重新学习。
// 新版本不会再产生这种组合（markModelQuotaBlocked 不再改 CostClass）。
func sanitizeRestoredModelState(state *modelRuntimeState) {
	if state == nil || !state.QuotaBlocked || state.CostClass != modelCostPaid {
		return
	}
	reason := strings.ToLower(state.LastReason)
	if strings.Contains(reason, "14018") || strings.Contains(reason, "credits exhausted") ||
		strings.Contains(state.LastReason, "额度已用尽") {
		state.CostClass = modelCostUnknown
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
	changed, _ := reloadAccountsResult()
	return changed
}

func reloadAccountsResult() (bool, error) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if cfg.AuthDir != "" || !cfg.AuthExplicit {
		dir := cfg.AuthDir
		if dir == "" {
			dir = "."
		}
		if _, err := os.ReadDir(dir); err != nil {
			return false, fmt.Errorf("凭据目录不可读取，账号池保持不变")
		}
	}

	changed := false
	var failures []error
	seen := make(map[string]bool)

	for _, p := range collectConfiguredAuthPaths() {
		st, err := os.Stat(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("凭据文件无法读取"))
			}
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
				failures = append(failures, fmt.Errorf("新增凭据不可解析，等待下轮重试"))
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
			failures = append(failures, fmt.Errorf("已更新凭据不可解析，保留旧内容"))
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
		existing.QuotaUnlimited = false
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
	return changed, errors.Join(failures...)
}

// accountReloaderLoop serve 后台凭据热加载协程：周期扫描凭据变化并原地收敛账号池。
func accountReloaderLoop() {
	interval := time.Duration(cfg.ReloadInterval) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastLogged time.Time
	var previousStatus string
	logScheduledTask("credential_reload", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(interval), "")
	for tick := range ticker.C {
		next := tick.Add(interval)
		started := time.Now()
		changed, err := reloadAccountsResult()
		if changed {
			writeStatusSnapshot() // 立即刷新 monitor 状态文件
			requestQuotaScan()
			requestCheckin()
			// 目录刷新成功后会自动触发价格探测；这里再直接触发一次，
			// 保证「原本没有某站点账号、后来加入」时也能立刻对该站点模型探测。
			requestModelsScan()
			requestModelsProbe()
		}
		status, detail := "success", "扫描完成，凭据没有变化"
		if changed {
			detail = "账号池变化已应用，相关刷新已通知"
		}
		if err != nil {
			status, detail = "partial", safeDebugError(err)
			if strings.HasPrefix(err.Error(), "凭据目录不可读取") {
				status = "failed"
			}
		}
		logFrequentTask("credential_reload", status, detail, started, next, &lastLogged, &previousStatus, changed)
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
	// 站点优先级必须在加锁前计算：preferredFreeSites 会读取账号账本并自行获取 accountMu，
	// 若在持锁期间调用会自锁。语义：该模型「一个站点免费、另一个站点收费」时优先用免费站点，
	// 直到该站点账号全部不可用；其余情况不搞优先，正常轮询。
	preferred := preferredFreeSites(model)

	accountMu.Lock()
	defer accountMu.Unlock()

	if len(accounts) == 0 {
		return nil, "", fmt.Errorf("账号池为空")
	}
	allowed := 0
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed(model, acc, accounts); ok {
			allowed++
		}
	}
	if allowed == 0 && modelAccountRuleConfigured(model) {
		return nil, "", fmt.Errorf("%w: 模型 %s 没有符合账号黑白名单的凭据文件", errModelAccountPolicy, model)
	}
	now := time.Now()

	pick := func(siteFilter func(string) bool) (*Account, accountSelectionKind, bool) {
		for _, wanted := range []accountSelectionKind{selectionFreeExhausted, selectionNormal, selectionProbeExhausted} {
			for i := 0; i < len(accounts); i++ {
				idx := (rrIndex + i) % len(accounts)
				acc := accounts[idx]
				if attempted != nil && attempted[acc] {
					continue
				}
				if ok, _ := modelAccountAllowed(model, acc, accounts); !ok {
					continue
				}
				if siteFilter != nil && !siteFilter(accSiteLocked(acc)) {
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
				return acc, kind, true
			}
		}
		return nil, "", false
	}

	if len(preferred) > 0 {
		if acc, kind, ok := pick(func(site string) bool { return preferred[site] }); ok {
			return acc, kind, nil
		}
		log.Printf("[FreeSite] 模型 %s 的免费站点账号当前均不可用，回退到其余站点代偿", model)
	}
	if acc, kind, ok := pick(nil); ok {
		return acc, kind, nil
	}

	disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting, policyBlocked := 0, 0, 0, 0, 0, 0
	earliest := time.Time{}
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed(model, acc, accounts); !ok {
			policyBlocked++
			continue
		}
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
	msg := fmt.Sprintf("当前模型 %s 暂无可用账号：账号名单排除=%d，授权失效=%d，账号冷却=%d，模型冷却=%d，额度阻断=%d，等待探测=%d", model, policyBlocked, disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting)
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

// markModelQuotaBlocked 记录一次「账号额度耗尽」观测。
//
// 注意：14018 的语义是「该账号的额度用光了」，与模型是否收费无关。
// 上游对额度为 0 的账号会整体拒绝（实测连免费模型也返回 14018），
// 因此这里只标记 QuotaBlocked（该账号暂时无法服务该模型），
// 绝不覆盖 CostClass —— 模型收费属性只能由 usage.credit 证据决定。
func markModelQuotaBlocked(acc *Account, model, msg string) {
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	oldClass := state.CostClass
	state.QuotaBlocked = true
	state.LastReason = msg
	state.ObservedAt = time.Now()
	accountMu.Unlock()
	log.Printf("[ModelQuota] 账号 %s 模型 %s 返回额度耗尽(14018)：仅阻断该账号对该模型的调度，账号额度恢复后自动解除；模型收费属性保持=%s，未被本次错误覆盖",
		acc.Path, model, ifEmpty(oldClass, modelCostUnknown))
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
	var authSnapshot *StoredAuth
	if acc.Auth != nil {
		nickname = acc.Auth.Account.Nickname
		uid = acc.Auth.Account.UID
		edition = profileForEdition(acc.Auth.Edition).Key
		acc.Nickname = nickname
		acc.UID = uid
		acc.Edition = edition
		copied := *acc.Auth
		authSnapshot = &copied
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

	// 删除凭据前先做一次只读校验：删除不可逆，只有确认凭据真的不可用才销毁。
	if keep, note := credentialStillUsableForDelete(authSnapshot); keep {
		log.Printf("[Auth] 账号 %s 已停止调度；%s；凭据文件保留在 %s（如需彻底移除请手动删除），重新登录会自动覆盖",
			path, note, path)
		writeStatusSnapshot()
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[Auth] 账号 %s 凭据文件删除失败: %v", path, err)
	}
	log.Printf("[Auth] 账号 %s 授权失效，已禁止调度并删除凭据文件（删除前只读校验确认不可用）: %s", path, reason)
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
		// 凭据文件比标记更新，说明用户已重新登录或手动续期，旧标记已过期。
		if fi, statErr := os.Stat(m.Path); statErr == nil {
			if mi, mErr := os.Stat(mp); mErr == nil && fi.ModTime().After(mi.ModTime()) {
				log.Printf("[Auth] 账号 %s 凭据文件比失效标记更新，视为已重新登录，清除失效标记", m.Path)
				_ = os.Remove(mp)
				continue
			}
		}
		// 凭据文件仍在时，标记负责把该账号重新置为失效（保留文件是为了可恢复）。
		applied := false
		for _, a := range accounts {
			if a.Path != m.Path {
				continue
			}
			a.Disabled = true
			a.DisabledReason = m.Reason
			if a.Nickname == "" {
				a.Nickname = m.Nickname
			}
			if a.UID == "" {
				a.UID = m.UID
			}
			applied = true
			break
		}
		if applied {
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
func doRefreshTokenFor(acc *Account) error {
	return refreshAccountToken(acc, "按需续期")
}

// refreshAccountToken 刷新指定账号的登录凭据并原子写回。
//
// 失败分两档处理：命中上游「登录态失效」标志（如 12153 / invalid_grant）或连续失败
// 达到 refreshDisableThreshold 次，才禁用账号；普通网络错误、5xx 以及未命中标志的
// 401/403 只累计计数并保留原凭据，避免一次抖动就删掉用户凭据。
func refreshAccountToken(acc *Account, reason string) error {
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
	oldTokens := refreshed.Auth // 覆盖前校验要比对旧凭据，先留存副本。
	path := acc.Path
	oldExpiresAt := refreshed.Auth.ExpiresAt
	accountMu.Unlock()

	log.Printf("[Auth] 账号 %s 开始刷新 Token，触发方式=%s，站点=%s，刷新前访问令牌过期时间=%s",
		path, reason, profileForEdition(refreshed.Edition).Label, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"))
	status, err := refreshTokenPayload(&refreshed)
	if err != nil {
		kind := classifyRefreshFailure(status, err.Error())
		summary := refreshFailureSummary(kind, status, err)
		count := recordRefreshFailure(acc, summary)
		if count >= refreshDisableThreshold {
			log.Printf("[Auth] 账号 %s 判定登录态失效，已停止调度并写入失效标记；连续失败=%d 判定依据=%s", path, count, summary)
			disableAccount(acc, fmt.Sprintf("令牌刷新连续失败 %d 次: %s", count, summary))
		} else if kind == refreshFailureSessionDead {
			// 上游明确说登录态没了，但先不删凭据：保留原凭据继续重试，达到阈值才停止调度。
			log.Printf("[Auth] 账号 %s 续期被上游判定登录态失效（连续 %d/%d 次）：%s；暂时保留凭据，达到阈值后才停止调度，建议尽快重新登录",
				path, count, refreshDisableThreshold, summary)
		} else {
			log.Printf("[Auth] 账号 %s Token 刷新失败，旧凭据未覆盖；连续失败=%d/%d 判定=%s 业务影响=本次未续期，账号仍可用到访问令牌过期，稍后自动重试",
				path, count, refreshDisableThreshold, summary)
		}
		return err
	}
	// 覆盖前校验：新凭据必须仍属于同一账号且确实可用，否则保留旧凭据不覆盖。
	if err := validateRefreshedCredential(path, &refreshed, oldTokens); err != nil {
		accountMu.Lock()
		acc.LastRefreshError = "新凭据未通过覆盖前校验: " + err.Error()
		accountMu.Unlock()
		log.Printf("[Auth] 账号 %s 新凭据未通过覆盖前校验，已放弃本次覆盖并保留原凭据；原因=%v 业务影响=账号继续使用原凭据，不销毁、不降级",
			path, err)
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
		acc.RefreshFailCount = 0
		acc.LastRefreshError = ""
	}
	accountMu.Unlock()
	newExpiresAt := refreshed.Auth.ExpiresAt
	if oldExpiresAt > 0 && newExpiresAt <= oldExpiresAt {
		// 刷新成功但访问令牌期限没有顺延：该账号可能已接近上游的登录态上限。
		log.Printf("[Auth] 账号 %s 刷新成功但访问令牌期限未顺延（%s -> %s），该账号可能已接近登录态上限，建议留意是否需要重新登录",
			path, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"), time.Unix(newExpiresAt, 0).Format("2006-01-02 15:04:05"))
	}
	log.Printf("[Auth] 账号 %s Token 刷新成功，访问令牌过期时间由 %s 更新为 %s，触发方式=%s，凭据已安全写回",
		path, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"), time.Unix(newExpiresAt, 0).Format("2006-01-02 15:04:05"), reason)
	writeStatusSnapshot()
	return nil
}

// recordRefreshFailure 累计连续刷新失败次数并返回当前次数。
func recordRefreshFailure(acc *Account, summary string) int {
	accountMu.Lock()
	defer accountMu.Unlock()
	acc.RefreshFailCount++
	acc.LastRefreshError = summary
	return acc.RefreshFailCount
}

// refreshTokenPayload 调用上游刷新接口并更新内存中的令牌字段（不落盘）。
// 按凭据文件中的 edition 路由到对应站点（国内站/国际站）的刷新接口。
// 返回上游 HTTP 状态码（成功或失败时均为实际状态；网络错误为 0）。
func refreshTokenPayload(sa *StoredAuth) (int, error) {
	registerCredentialSecrets(sa)
	prof := profileForEdition(sa.Edition)
	headers := func(r *http.Request) {
		controlHeaders(r, prof)
		// 客户端 refreshSession 实测：enterpriseHeaders(auth)（X-Domain）+ X-Refresh-Token
		// + X-Auth-Refresh-Source: plugin + traceSpan 头；AuthenticationHttpInterceptor 再补
		// Authorization 与 X-User-Id/X-Enterprise-Id/X-Tenant-Id（该步不声明任何 X-No-*）。
		setControlTraceHeaders(r)
		if sa.Auth.AccessToken != "" {
			setHeaderExact(r.Header, "Authorization", "Bearer "+sa.Auth.AccessToken)
		}
		if sa.Account.UID != "" {
			setHeaderExact(r.Header, "X-User-Id", sa.Account.UID)
		}
		setHeaderExact(r.Header, "X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			setHeaderExact(r.Header, "X-Enterprise-Id", sa.Account.EnterpriseID)
			setHeaderExact(r.Header, "X-Tenant-Id", sa.Account.EnterpriseID)
		}
		if sa.Auth.Domain != "" {
			setHeaderExact(r.Header, "X-Domain", sa.Auth.Domain)
		} else {
			setHeaderExact(r.Header, "X-Domain", prof.domain())
		}
		setHeaderExact(r.Header, "X-Auth-Refresh-Source", "plugin")
	}

	// 客户端 refreshSession 是 `post(url, {})`（axios 因此补 Content-Type 并发送 2 字节 body），
	// 不是空 body：此处必须与之一致，否则 Content-Length / Content-Type 都与客户端不同。
	data, status, err := doJSON(cfg.HttpClient, http.MethodPost, prof.tokenRefreshURL(), headers, strings.NewReader("{}"))
	if err != nil {
		return status, fmt.Errorf("上游刷新拒绝 (HTTP %d): %w", status, err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return status, fmt.Errorf("解析新 Token 失败: %w", err)
	}
	registerSecrets(tok.AccessToken, tok.RefreshToken)

	now := time.Now()
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	// 接口未返回 expiresIn 时保留旧过期时间（preserveExpiry），避免刷新风暴。
	if tok.ExpiresIn > 0 {
		sa.Auth.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// 刷新令牌到期时间：优先用接口返回的 refreshExpiresIn；接口没给时，
	// 从当前刷新令牌未验签的令牌声明读取，避免在界面上显示成未知。
	if tok.RefreshExpiresIn > 0 {
		sa.Auth.RefreshExpiresAt = now.Add(time.Duration(tok.RefreshExpiresIn) * time.Second).Unix()
	} else if exp, ok := jwtExpiry(sa.Auth.RefreshToken); ok {
		sa.Auth.RefreshExpiresAt = exp.Unix()
	}
	sa.Auth.LastRefreshTime = now.Unix()
	return status, nil
}

func parseQuotaCapacity(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return strconv.ParseFloat(value, 64)
}

// summary 只有额度与历史标记，不能独立判定有效套餐。
// 由完整权益查询成功后替换“待确认”，禁止据试用标记猜成Pro试用。
func planLabelFromSummary(summary quotaSummaryData) string {
	return planUnknown
}

func parseQuotaSummary(data []byte) (total, used, remaining float64, paid bool, plan string, err error) {
	var summary quotaSummaryData
	if err = json.Unmarshal(data, &summary); err != nil {
		return 0, 0, 0, false, "", fmt.Errorf("解析额度响应失败: %w", err)
	}
	for _, pkg := range summary.Packages {
		pkgTotal, parseErr := parseQuotaCapacity(pkg.CycleTotalCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("解析总额度 %q 失败: %w", pkg.CycleTotalCapacity, parseErr)
		}
		pkgUsed, parseErr := parseQuotaCapacity(pkg.CycleUsedCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("解析已用额度 %q 失败: %w", pkg.CycleUsedCapacity, parseErr)
		}
		pkgRemaining, parseErr := parseQuotaCapacity(pkg.CycleRemainCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("解析剩余额度 %q 失败: %w", pkg.CycleRemainCapacity, parseErr)
		}
		total += pkgTotal
		used += pkgUsed
		remaining += pkgRemaining
	}
	return total, used, remaining, summary.IsPaidUser, planLabelFromSummary(summary), nil
}

// parseEnterpriseUsage 把企业分配额度换成网关的总额/已用/剩余。
// credit（或 used_num）是本周期已用，和个人口径的「剩余」相反；
// limitNum（或 limit_num）是分给这个成员的额度。limitNum=-1 表示不限量。
func parseEnterpriseUsage(data []byte) (total, used, remaining float64, unlimited bool, err error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, 0, 0, false, fmt.Errorf("企业额度响应为空")
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(trimmed, &raw); err != nil {
		return 0, 0, 0, false, fmt.Errorf("解析企业额度响应失败: %w", err)
	}
	limit, hasLimit, err := firstEnterpriseNumber(raw, "limitNum", "limit_num")
	if err != nil {
		return 0, 0, 0, false, err
	}
	if !hasLimit {
		return 0, 0, 0, false, fmt.Errorf("企业额度响应缺少 limitNum")
	}
	used, hasUsed, err := firstEnterpriseNumber(raw, "credit", "used_num", "usedNum")
	if err != nil {
		return 0, 0, 0, false, err
	}
	if !hasUsed {
		return 0, 0, 0, false, fmt.Errorf("企业额度响应缺少 credit/used_num")
	}
	if used < 0 || (limit < 0 && limit != -1) {
		return 0, 0, 0, false, fmt.Errorf("企业额度响应包含无效负数")
	}
	if limit == -1 {
		// 不限量没有「剩余<=0」这回事。总额/剩余用 -1 作哨兵，展示层显示「不限」。
		return -1, used, -1, true, nil
	}
	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	return limit, used, remaining, false, nil
}

func firstEnterpriseNumber(raw map[string]json.RawMessage, keys ...string) (float64, bool, error) {
	var value float64
	var present bool
	for _, key := range keys {
		val, ok := raw[key]
		if !ok || string(bytes.TrimSpace(val)) == "null" {
			continue
		}
		num, err := parseEnterpriseJSONNumber(val)
		if err != nil {
			return 0, true, fmt.Errorf("企业额度字段 %s 无法解析", key)
		}
		if present && value != num {
			return 0, true, fmt.Errorf("企业额度同义字段数值冲突")
		}
		value, present = num, true
	}
	return value, present, nil
}

func parseEnterpriseJSONNumber(raw json.RawMessage) (float64, error) {
	raw = bytes.TrimSpace(raw)
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil && !math.IsNaN(num) && !math.IsInf(num, 0) {
		return num, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		num, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err == nil && !math.IsNaN(num) && !math.IsInf(num, 0) {
			return num, nil
		}
	}
	return 0, fmt.Errorf("not a number")
}

// applyQuotaObservation 写入额度并在余额恢复或不限量时解除模型额度阻断。
// 调用方可以持有账号锁；这里只取账号池锁。
func applyQuotaObservation(acc *Account, total, used, remaining float64, unlimited, paid bool, plan string, planOK bool, now time.Time) (wasExhausted, exhausted bool, display string) {
	accountMu.Lock()
	defer accountMu.Unlock()
	wasExhausted = acc.QuotaExhausted
	acc.QuotaTotal = total
	acc.QuotaUsed = used
	acc.QuotaRemaining = remaining
	acc.QuotaUnlimited = unlimited
	acc.IsPaidUser = paid
	if planOK {
		acc.PlanLabel = plan
		acc.PlanCheckedAt = now.Unix()
		acc.PlanStale = false
	} else {
		if acc.PlanCheckedAt == 0 {
			acc.PlanLabel = planUnknown
		}
		acc.PlanStale = true
	}
	display = planDisplayLabel(acc.PlanLabel, acc.PlanStale)
	acc.QuotaKnown = true
	exhausted = !unlimited && remaining <= 0
	acc.QuotaExhausted = exhausted
	if unlimited || remaining > 0 {
		for _, state := range acc.ModelStates {
			if state == nil {
				continue
			}
			state.QuotaBlocked = false
			state.NextProbeAt = time.Time{}
		}
	}
	return wasExhausted, exhausted, display
}

func formatQuota(value float64) string {
	if value > -0.005 && value < 0.005 {
		value = 0
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func formatQuotaAmount(unlimited bool, kind string, value float64) string {
	if unlimited && (kind == "total" || kind == "remaining") {
		return "不限"
	}
	return formatQuota(value)
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

func usageNumber(usage map[string]any, key string) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	switch v := usage[key].(type) {
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n, true
		}
		f, err := v.Float64()
		return int64(f), err == nil
	case string:
		s := strings.TrimSpace(v)
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(s, 64)
		return int64(f), err == nil
	default:
		return 0, false
	}
}

func usageTotalTokens(usage map[string]any) (int64, bool) {
	if n, ok := usageNumber(usage, "total_tokens"); ok {
		return n, true
	}
	prompt, hasPrompt := usageNumber(usage, "prompt_tokens")
	completion, hasCompletion := usageNumber(usage, "completion_tokens")
	if hasPrompt || hasCompletion {
		return prompt + completion, true
	}
	return 0, false
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
	site := acc.Profile().Key
	accountMu.Unlock()
	recordModelCostClass(model, site, credit <= 0)

	if credit <= 0 {
		if oldClass != modelCostFree {
			log.Printf("[ModelCost] requestId=%d 账号 %s 站点=%s 模型 %s 实测 usage.credit=0，已学习为免费模型", reqID, path, site, model)
		}
		if quotaExhausted {
			log.Printf("[FreeModel] requestId=%d 站点=%s 请求的是免费模型 %s，已明确使用付费余额耗尽账号 %s 完成请求，usage.credit=0", reqID, site, model, path)
		}
	} else if oldClass != modelCostPaid {
		log.Printf("[ModelCost] requestId=%d 账号 %s 站点=%s 模型 %s 实测 usage.credit=%s，已学习为收费模型", reqID, path, site, model, formatQuota(credit))
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

	traceID := newTraceID()
	enterpriseID := strings.TrimSpace(auth.Account.EnterpriseID)
	if enterpriseID != "" {
		return refreshEnterpriseAccountQuota(ctx, acc, auth, path, prof, traceID, enterpriseID)
	}
	log.Printf("[Quota] traceId=%s 账号 %s 开始查询额度，站点=%s，接口=%s", traceID, path, prof.Label, prof.quotaSummaryURL())
	// 该接口由客户端主进程经 axios（RestOperations）调用：
	//   post(`/billing/meter/get-user-resource-summary`, {}, {headers:{"Accept-Language": locale}})
	// 故走控制面头集合（非模型请求头集合），身份头由 AuthenticationHttpInterceptor 补。
	// 旧实现混用了模型请求头集合并自造 `X-Client-Platform: web`（客户端全量安装目录 0 命中）。
	headers := func(r *http.Request) {
		controlHeaders(r, prof)
		setHeaderExact(r.Header, "Authorization", "Bearer "+auth.Auth.AccessToken)
		if auth.Account.UID != "" {
			setHeaderExact(r.Header, "X-User-Id", auth.Account.UID)
		}
		if auth.Auth.Domain != "" {
			setHeaderExact(r.Header, "X-Domain", auth.Auth.Domain)
		} else {
			setHeaderExact(r.Header, "X-Domain", prof.domain())
		}
		if auth.Account.EnterpriseID != "" {
			setHeaderExact(r.Header, "X-Enterprise-Id", auth.Account.EnterpriseID)
			setHeaderExact(r.Header, "X-Tenant-Id", auth.Account.EnterpriseID)
		}
		setControlTraceHeadersWithID(r, traceID, newSpanID())
	}
	data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.quotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s 账号 %s 查询额度失败，HTTP=%d，保留上一次额度数据，套餐标记待刷新", traceID, path, status)
		return err
	}
	total, used, remaining, paid, plan, err := parseQuotaSummary(data)
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s 账号 %s 查询额度响应无法解析，保留上一次额度数据，套餐标记待刷新", traceID, path)
		return err
	}
	var summary quotaSummaryData
	_ = json.Unmarshal(data, &summary) // parseQuotaSummary 已校验。
	now := time.Now()
	resources, planErr := fetchPlanResources(ctx, prof, headers, traceID, path, now)
	if planErr == nil {
		plan, planErr = identifyPlan(prof.Key, summary, resources, now)
	}
	wasExhausted, exhausted, plan := applyQuotaObservation(acc, total, used, remaining, false, paid, plan, planErr == nil, now)
	if planErr != nil {
		log.Printf("[套餐判断] traceId=%s 账号=%s 结果=待确认 原因=%v 显示=%s 业务影响=额度仍更新，不把接口失败误报为免费或试用", traceID, path, planErr, plan)
	} else {
		log.Printf("[套餐判断] traceId=%s 账号=%s 有效期校验时间=%s 资源数=%d 显示=%s 结果=完整权益校验成功", traceID, path, now.Format(time.RFC3339), len(resources), plan)
	}
	switch {
	case !wasExhausted && exhausted:
		log.Printf("[Quota] traceId=%s 账号 %s 额度查询成功，总额度=%s，已用=%s，剩余=%s，付费用户=%t，套餐=%s；账号已冻结调度，等待额度恢复", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	case wasExhausted && !exhausted:
		log.Printf("[Quota] traceId=%s 账号 %s 额度已恢复，总额度=%s，已用=%s，剩余=%s，付费用户=%t，套餐=%s；账号已自动解除冻结并恢复调度", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	default:
		log.Printf("[Quota] traceId=%s 账号 %s 额度查询成功，总额度=%s，已用=%s，剩余=%s，付费用户=%t，套餐=%s", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	}
	return nil
}

// refreshEnterpriseAccountQuota 只查询企业成员被分配的额度。
// 外部项目实测个人 get-user-resource* 对企业号为空，不能据此把账号判成剩余 0。
// 调用方已持有 acc.lock。
func refreshEnterpriseAccountQuota(ctx context.Context, acc *Account, auth StoredAuth, path string, prof *upstreamProfile, traceID, enterpriseID string) error {
	endpoint := prof.enterpriseUsageURL()
	log.Printf("[Quota] traceId=%s 账号 %s 开始查询企业分配额度，站点=%s，接口=%s，企业身份=已识别，说明=不请求个人资源包 get-user-resource，避免空包被当成剩余 0", traceID, path, prof.Label, endpoint)
	headers := func(r *http.Request) {
		controlHeaders(r, prof)
		setHeaderExact(r.Header, "Authorization", "Bearer "+auth.Auth.AccessToken)
		setControlTraceHeadersWithID(r, traceID, newSpanID())
		setHeaderExact(r.Header, "X-Enterprise-Id", enterpriseID)
		setHeaderExact(r.Header, "X-Tenant-Id", enterpriseID)
		if auth.Account.UID != "" {
			setHeaderExact(r.Header, "X-User-Id", strings.TrimSpace(auth.Account.UID))
		}
		if auth.Auth.Domain != "" {
			setHeaderExact(r.Header, "X-Domain", auth.Auth.Domain)
		} else {
			setHeaderExact(r.Header, "X-Domain", prof.domain())
		}
	}
	data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, endpoint, headers, strings.NewReader("{}"))
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s 账号 %s 企业额度查询失败，HTTP=%d，保留上一次额度，业务影响=不把失败当成剩余 0，本轮不解除也不新增强制阻断", traceID, path, status)
		return err
	}
	total, used, remaining, unlimited, err := parseEnterpriseUsage(data)
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s 账号 %s 企业额度响应无法解析，保留上一次额度，原因=%v，业务影响=不把坏数据当成剩余 0", traceID, path, err)
		return err
	}
	now := time.Now()
	wasExhausted, exhausted, plan := applyQuotaObservation(acc, total, used, remaining, unlimited, false, planEnterprise, true, now)
	log.Printf("[套餐判断] traceId=%s 账号=%s 结果=%s 说明=企业号没有个人套餐资源包，不调用 get-user-resource，显示=%s", traceID, path, planEnterprise, plan)
	totalText := formatQuotaAmount(unlimited, "total", total)
	remainText := formatQuotaAmount(unlimited, "remaining", remaining)
	switch {
	case !wasExhausted && exhausted:
		log.Printf("[Quota] traceId=%s 账号 %s 企业额度计算=limitNum %s - credit %s，剩余=%s，不限量=%t，套餐=%s；分配额度已用完，收费模型暂停调度", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
	case wasExhausted && !exhausted:
		log.Printf("[Quota] traceId=%s 账号 %s 企业额度已恢复，计算=limitNum %s - credit %s，剩余=%s，不限量=%t，套餐=%s；已解除此前因个人空包或 14018 造成的额度阻断", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
	default:
		log.Printf("[Quota] traceId=%s 账号 %s 企业额度查询成功，计算=limitNum %s - credit %s，剩余=%s，不限量=%t，套餐=%s", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
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

func refreshAllAccountQuotas(ctx context.Context, done chan<- struct{}, schedule ...time.Time) {
	defer close(done)
	next := time.Time{}
	if len(schedule) > 0 {
		next = schedule[0]
	}
	trace, started := startScheduledTask("quota_scan", next)
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	log.Printf("[Quota] 开始批量更新额度，账号数=%d", len(accs))
	processed, failed := 0, 0
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			log.Printf("[Quota] 本轮额度更新被取消，尚未扫描的账号将等待下一轮: %v", err)
			break
		}
		accountCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := refreshAccountQuota(accountCtx, acc)
		processed++
		cancel()
		if err != nil {
			failed++
			log.Printf("[Quota] 账号 %s 本轮额度更新未完成: %v", acc.Path, err)
		} else {
			accountMu.Lock()
			if acc != nil && !acc.Disabled && acc.PlanStale {
				failed++
			}
			accountMu.Unlock()
		}
	}
	snapshotErr := writeStatusSnapshot()
	log.Printf("[Quota] 本轮额度更新完成，状态快照已写入")
	status := "success"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if len(accs) == 0 {
		status = "skipped"
	} else if failed == len(accs) {
		status = "failed"
	} else if failed > 0 || snapshotErr != nil {
		status = "partial"
	}
	logScheduledTask("quota_scan", "completed", status, trace, started, next, fmt.Sprintf("账号=%d，已检查=%d，额度/套餐异常=%d，快照失败=%t", len(accs), processed, failed, snapshotErr != nil))
}

func backgroundQuotaRefresher() {
	const interval = 300 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	next := time.Now().Add(interval)
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
		go refreshAllAccountQuotas(ctx, done, next)
	}
	start()
	for {
		select {
		case tick := <-ticker.C:
			next = tick.Add(interval)
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
	if strings.TrimSpace(acc.Auth.Account.EnterpriseID) != "" {
		accountPath := acc.Path
		accountMu.Unlock()
		log.Printf("[Checkin] traceId=%s 账号 %s 是企业版，不发起个人签到请求；额度仍由企业分配额度接口更新", newTraceID(), accountPath)
		return "enterprise_skipped", nil
	}
	auth := *acc.Auth
	path := acc.Path
	prof := acc.Profile()
	accountMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	log.Printf("[Checkin] 账号 %s 开始每日签到，站点=%s，接口=%s", path, prof.Label, prof.dailyCheckinURL())
	// 客户端 `checkin()`（billingPrefix=/v2）同样经 axios `RestOperations` 发出：
	//   post(`/v2/billing/meter/daily-checkin`)  // 带 X-Device-Token（Turing 设备令牌）
	// 故头集合取控制面集合，而非模型请求集合；旧实现混用模型头并自造 `X-Client-Platform: web`。
	// 网关无法产出 Turing 设备令牌（需本地原生风控 SDK），此处不臆造该头。
	headers := func(r *http.Request) {
		controlHeaders(r, prof)
		setHeaderExact(r.Header, "Authorization", "Bearer "+auth.Auth.AccessToken)
		if auth.Account.UID != "" {
			setHeaderExact(r.Header, "X-User-Id", auth.Account.UID)
		}
		if auth.Account.EnterpriseID != "" {
			setHeaderExact(r.Header, "X-Enterprise-Id", auth.Account.EnterpriseID)
			setHeaderExact(r.Header, "X-Tenant-Id", auth.Account.EnterpriseID)
		}
		if auth.Auth.Domain != "" {
			setHeaderExact(r.Header, "X-Domain", auth.Auth.Domain)
		} else {
			setHeaderExact(r.Header, "X-Domain", prof.domain())
		}
		setControlTraceHeaders(r)
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
	next := nextDailyCheckin(time.Now())
	trace, started := startScheduledTask("daily_checkin", next)

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
	status := "success"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if failed > 0 && ok+already == 0 {
		status = "failed"
	} else if failed > 0 {
		status = "partial"
	} else if ok+already == 0 {
		status = "skipped"
	}
	logScheduledTask("daily_checkin", "completed", status, trace, started, next, fmt.Sprintf("成功=%d，今日已签=%d，失败=%d，跳过=%d", ok, already, failed, skipped))
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
		logScheduledTask("daily_checkin", "scheduled", "", newTraceID(), time.Time{}, next, "固定北京时间09:00；账号池变化可能提前触发")
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
	sb.WriteString(formatRenewalStatus(acc))
	sb.WriteString(fmt.Sprintf("套餐:         %s\n", planDisplayLabel(acc.PlanLabel, acc.PlanStale)))
	if acc.PlanStale {
		sb.WriteString("              * 权益查询失败，待刷新；名称可能是上次成功结果。\n")
	}
	if acc.QuotaKnown {
		sb.WriteString(fmt.Sprintf("额度:         总额度 %s / 已用 %s / 剩余 %s\n",
			formatQuotaAmount(acc.QuotaUnlimited, "total", acc.QuotaTotal),
			formatQuotaAmount(acc.QuotaUnlimited, "used", acc.QuotaUsed),
			formatQuotaAmount(acc.QuotaUnlimited, "remaining", acc.QuotaRemaining)))
	}
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
	Alerts            []accountAlertMark `json:"alerts,omitempty"`
	Path              string             `json:"path"`
	SchedulingEnabled *bool              `json:"schedulingEnabled,omitempty"`
	SchedulingReason  string             `json:"schedulingReason,omitempty"`
	Edition           string             `json:"edition,omitempty"` // 站点标识（cn/intl）
	Nickname          string             `json:"nickname"`
	UID               string             `json:"uid"`
	State             string             `json:"state"` // active | cooldown | paid_exhausted | expired | disabled
	CooldownUntil     int64              `json:"cooldownUntil,omitempty"`
	CooldownMsg       string             `json:"cooldownMsg,omitempty"`
	DisabledReason    string             `json:"disabledReason,omitempty"`
	TokenExpiresAt    int64              `json:"tokenExpiresAt,omitempty"`
	QuotaTotal        float64            `json:"quotaTotal,omitempty"`
	QuotaUsed         float64            `json:"quotaUsed,omitempty"`
	QuotaRemaining    float64            `json:"quotaRemaining"`
	IsPaidUser        bool               `json:"isPaidUser"`
	PlanLabel         string             `json:"planLabel,omitempty"` // 官方套餐名称或已知短名称
	PlanCheckedAt     int64              `json:"planCheckedAt,omitempty"`
	PlanStale         bool               `json:"planStale,omitempty"`
	// 登录续期状态：刷新令牌到期时间与最近成功续期时间，便于提前发现需要重新登录的账号。
	RefreshExpiresAt int64                         `json:"refreshExpiresAt,omitempty"`
	LastRefreshTime  int64                         `json:"lastRefreshTime,omitempty"`
	RefreshFailCount int                           `json:"refreshFailCount,omitempty"`
	QuotaKnown       bool                          `json:"quotaKnown,omitempty"`
	QuotaExhausted   bool                          `json:"quotaExhausted,omitempty"`
	QuotaUnlimited   bool                          `json:"quotaUnlimited,omitempty"`
	ModelStates      map[string]modelStateSnapshot `json:"modelStates,omitempty"`
	FreeModels       int                           `json:"freeModels,omitempty"` // 展示值：所属站点模型统计中 0.00x 的模型数，不参与调度
	ModelCooldowns   int                           `json:"modelCooldowns,omitempty"`
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
	AlertSource  string              `json:"alertSource,omitempty"`
	UpdatedAt    int64               `json:"updatedAt"`
	Accounts     []accountSnapshot   `json:"accounts"`
	Models       []modelStatSnapshot `json:"models,omitempty"`
	ModelCatalog []modelStatSnapshot `json:"modelCatalog,omitempty"` // 管理页完整模型目录，含停用项。
}

// writeStatusSnapshot 将账号池实时状态（含冷却/失效）原子写入状态快照文件。
// serve 后台周期调用；monitor 命令前台读取展示。
func writeStatusSnapshot() error {
	statusSnapshotWriteMu.Lock()
	defer statusSnapshotWriteMu.Unlock()
	accountMu.Lock()
	snap := statusSnapshot{UpdatedAt: time.Now().Unix()}
	now := time.Now()
	for _, acc := range accounts {
		as := accountSnapshot{Path: acc.Path, Edition: acc.Profile().Key}
		as.QuotaTotal = acc.QuotaTotal
		as.QuotaUsed = acc.QuotaUsed
		as.QuotaRemaining = acc.QuotaRemaining
		as.IsPaidUser = acc.IsPaidUser
		as.PlanLabel = acc.PlanLabel
		as.PlanCheckedAt = acc.PlanCheckedAt
		as.PlanStale = acc.PlanStale
		as.RefreshFailCount = acc.RefreshFailCount
		if acc.Auth != nil {
			as.RefreshExpiresAt = acc.Auth.Auth.RefreshExpiresAt
			as.LastRefreshTime = acc.Auth.Auth.LastRefreshTime
			if as.RefreshExpiresAt == 0 {
				// 旧凭据或手工导入的凭据没有该字段，从未验签的令牌声明补一个展示值。
				if exp, ok := jwtExpiry(acc.Auth.Auth.RefreshToken); ok {
					as.RefreshExpiresAt = exp.Unix()
				}
			}
		}
		as.QuotaKnown = acc.QuotaKnown
		as.QuotaExhausted = acc.QuotaExhausted
		as.QuotaUnlimited = acc.QuotaUnlimited
		if len(acc.ModelStates) > 0 {
			as.ModelStates = make(map[string]modelStateSnapshot, len(acc.ModelStates))
			for model, state := range acc.ModelStates {
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
	annotateAccountPolicies(snap.Accounts)
	snap.ModelCatalog = buildAllModelStatSnapshots(now, accounts)
	captureAccountStatsLocked()
	for _, row := range snap.ModelCatalog {
		if disabled, _ := modelDisabled(row.ID); !disabled {
			snap.Models = append(snap.Models, row)
		}
	}
	cnFree, intlFree := freeModelDisplayCounts(snap.Models)
	for i := range snap.Accounts {
		if profileForEdition(snap.Accounts[i].Edition).Key == "intl" {
			snap.Accounts[i].FreeModels = intlFree
		} else {
			snap.Accounts[i].FreeModels = cnFree
		}
	}
	if !lastFreeModelDisplay.initialized || lastFreeModelDisplay.cn != cnFree || lastFreeModelDisplay.intl != intlFree {
		lastFreeModelDisplay.initialized = true
		lastFreeModelDisplay.cn, lastFreeModelDisplay.intl = cnFree, intlFree
		log.Printf("[免费模型展示] traceId=%s 来源=模型统计站点倍率 国内=%d 国际=%d 模型行数=%d 规则=只计0.00x 业务影响=仅更新账号表展示，不改实测账本、调度或计费判断",
			newTraceID(), cnFree, intlFree, len(snap.Models))
	}
	accountMu.Unlock()

	// 告警标记与快照同一次写入，避免出现「快照已异常、日志还没扫到」的时间差。
	marks := recordAccountAlertMarks(snap.Accounts, snap.UpdatedAt)
	for i := range snap.Accounts {
		snap.Accounts[i].Alerts = marks[snap.Accounts[i].Path]
	}
	snap.AlertSource = alertSourceSnapshot

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return writeStatusSnapshotFile(data)
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
	for _, a := range accs {
		widths[9] = min(36, max(widths[9], displayWidth(planDisplayLabel(a.PlanLabel, a.PlanStale))))
	}
	headers := []string{"序号", "凭据文件", "账号", "站点", "状态", "Token 有效期", "总额度", "已用", "剩余", "套餐", "免费模型", "模型冷却"}
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
		total, used, remaining, plan := "-", "-", "-", "-"
		if a.QuotaKnown {
			total = formatQuotaAmount(a.QuotaUnlimited, "total", a.QuotaTotal)
			used = formatQuotaAmount(a.QuotaUnlimited, "used", a.QuotaUsed)
			remaining = formatQuotaAmount(a.QuotaUnlimited, "remaining", a.QuotaRemaining)
			plan = planDisplayLabel(a.PlanLabel, a.PlanStale)
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
			profileForEdition(a.Edition).Label, status, expires, total, used, remaining, plan,
			strconv.Itoa(a.FreeModels), strconv.Itoa(a.ModelCooldowns),
		}) + "\n")
	}
	b.WriteString(border())
	for _, a := range accs {
		if a.PlanStale {
			b.WriteString("\n* 套餐权益查询失败，待刷新；名称可能是上次成功结果。")
			break
		}
	}
	now := time.Now()
	for _, a := range accs {
		if a.RefreshExpiresAt <= 0 {
			continue
		}
		exp := time.Unix(a.RefreshExpiresAt, 0)
		switch {
		case !exp.After(now):
			b.WriteString(fmt.Sprintf("\n! %s 刷新令牌已过期，需要重新登录。", filepath.Base(a.Path)))
		case exp.Sub(now) < 7*24*time.Hour:
			b.WriteString(fmt.Sprintf("\n! %s 刷新令牌 %s 到期（剩余 %s），建议尽快重新登录。",
				filepath.Base(a.Path), exp.Format("2006-01-02 15:04:05"), humanDuration(time.Until(exp))))
		}
	}
	for _, a := range accs {
		if a.RefreshFailCount > 0 {
			b.WriteString(fmt.Sprintf("\n! %s 续期连续失败 %d 次，可能需要重新登录。", filepath.Base(a.Path), a.RefreshFailCount))
		}
	}
	return b.String()
}

// statusSnapshotLoop serve 后台每 3 秒刷新一次状态快照。
func statusSnapshotLoop() {
	var lastLogged time.Time
	var previousStatus string
	write := func(next time.Time) {
		started := time.Now()
		err := writeStatusSnapshot()
		status, detail := "success", "账号与模型快照已写入"
		if err != nil {
			status, detail = "failed", safeDebugError(err)
		}
		logFrequentTask("status_snapshot", status, detail, started, next, &lastLogged, &previousStatus, err != nil)
	}
	write(time.Now().Add(3 * time.Second))
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for tick := range ticker.C {
		write(tick.Add(3 * time.Second))
	}
}

// tailLines 读取文件末尾 n 行（用于 monitor 展示最近日志）。
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines, _, err := readTailLines(f, n, 0)
	return lines, err
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

		data, err := readStatusSnapshotFile()
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
	site := "cn"
	if cfg.LoginIntl {
		site = "intl"
	}
	prof := profileForEdition(site)

	fmt.Println("================ WorkBuddy 登录 ================")
	fmt.Printf("目标站点:     %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
	if cfg.LoginIntl {
		fmt.Println("国际站登录将在浏览器中完成（邮箱 / 验证码 / SSO 等），凭据由网关自动接管。")
	}
	fmt.Println("正在生成登录凭据与二维码...")

	// 与网页「添加账号」共用同一条登录流程，避免 CLI 与网页两套实现漂移。
	s, err := beginAccountLogin(site, cfg.AuthFile)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	if qr, qrErr := qrcode.New(s.AuthURL, qrcode.Medium); qrErr == nil {
		if cfg.LoginIntl {
			fmt.Println("\n请用手机扫描下方二维码，并在浏览器中完成登录（邮箱 / 验证码 / SSO 等）：")
		} else {
			fmt.Println("\n请使用 微信 或 企业微信 扫描下方二维码登录：")
		}
		fmt.Println(qr.ToSmallString(false))
	}
	fmt.Println("如无法扫码，也可在浏览器中直接打开以下链接：")
	fmt.Printf("%s\n\n", s.AuthURL)
	fmt.Println("等待登录授权完成 (按 Ctrl+C 可取消)...")

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		snap := s.snapshot()
		switch snap["status"] {
		case "done":
			fmt.Println("\n登录成功")
			fmt.Printf("站点:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
			if acct, ok := snap["account"].(map[string]any); ok {
				fmt.Printf("欢迎，%v (UID: %v)\n", acct["nickname"], acct["uid"])
				if exp, ok := acct["tokenExpiresAt"].(int64); ok && exp > 0 {
					fmt.Printf("访问令牌有效期至: %s\n", time.Unix(exp, 0).Format("2006-01-02 15:04:05"))
				}
			}
			fmt.Printf("凭据已成功保存到: %s\n", cfg.AuthFile)
			fmt.Println("\n现在可以启动网关服务：")
			fmt.Println("  workbuddy-gateway serve")
			return
		case "failed":
			fmt.Printf("\n%v\n", snap["error"])
			return
		case "expired":
			fmt.Println("\n登录已超时，请重新执行 login 命令。")
			return
		case "cancelled":
			fmt.Println("\n登录已取消。")
			return
		}
	}
}

// -----------------------------------------------------------------------------
// 本地 HTTP API 网关服务 (OpenAI 协议兼容)
// -----------------------------------------------------------------------------

func runServe() {
	// 先绑定全部所需端口；任一失败就释放已经绑定的端口，不启动半套服务。
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	configCtx, stopConfigReload := context.WithCancel(context.Background())
	defer stopConfigReload()
	loadModelsCache()
	loadOptionalStats()
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
	// 主动续期（保活）：到点刷新全部账号登录凭据，不等访问令牌临近过期
	go keepaliveLoop()
	go backgroundQuotaRefresher()
	go backgroundDailyCheckin()
	if cfg.ModelsRefresh > 0 {
		go modelsRefreshLoop(time.Duration(cfg.ModelsRefresh) * time.Minute)
	} else {
		logScheduledTask("model_catalog", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "模型目录自动刷新已关闭")
	}
	// 价格探测独立于目录刷新：即使关闭目录刷新，也仍可基于缓存探测价格。
	go modelPriceProbeLoop()

	// 启动状态快照协程（monitor 命令实时读取展示）
	go statusSnapshotLoop()
	statsCtx, stopStats := context.WithCancel(context.Background())
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		statsPersistenceLoop(statsCtx)
	}()
	defer func() {
		stopStats()
		<-statsDone
	}()

	// 启动凭据热加载协程（新增/更新/删除凭据文件免重启生效）
	if cfg.ReloadInterval > 0 {
		go accountReloaderLoop()
	} else {
		logScheduledTask("credential_reload", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "凭据自动扫描已关闭")
	}

	listenAddr := servers[0].Addr

	fmt.Println("================================================================")
	fmt.Printf("WorkBuddy 本地网关已启动\n")
	fmt.Printf("   服务监听地址:  http://%s\n", listenAddr)
	fmt.Printf("   Chat 接口地址: http://%s/v1/chat/completions\n", listenAddr)
	fmt.Printf("   Claude Code:   http://%s/v1/messages (ANTHROPIC_BASE_URL 指向本网关)\n", listenAddr)
	fmt.Printf("   Models 接口:   http://%s/v1/models\n", listenAddr)
	fmt.Printf("   模型转发策略:  【完全透传】客户端请求的任意 model 原样中继至上游\n")
	_, modelSource := mergedModelIDs()
	fmt.Printf("   模型列表来源:  %s\n", modelSourceLabel(modelSource))
	if cfg.ReloadInterval > 0 {
		fmt.Printf("   凭据热加载:    每 %ds 自动扫描，新增/更新/删除凭据免重启生效\n", cfg.ReloadInterval)
	} else {
		fmt.Printf("   凭据热加载:    已关闭 (-reload-interval 0)\n")
	}
	if currentAPIKey() != "" {
		printAPIAuthBanner(os.Stdout)
	} else {
		fmt.Printf("   API 鉴权:      未启用 (任何客户端均可直连)\n")
	}
	if cfg.WebUI {
		fmt.Printf("   网页控制台:    已启用 http://%s/ui/ (独立管理 Key)\n", servers[1].Addr)
		if currentAdminKey() == "" {
			fmt.Println("   管理初始化:    请在本机或 SSH 隧道打开网页生成管理 Key，保存成功后仅弹窗展示一次")
		}
	} else {
		fmt.Println("   网页控制台:    未启用 (-webui 可开启)")
	}
	if cfg.ProxyURL != "" {
		fmt.Printf("   上游出口代理:  %s\n", cfg.ProxyURL)
	}
	if cfg.DebugEnabled {
		fmt.Printf("   JSON 调试日志: 已开启 (%s)\n", filepath.Join(logDir, "debug-YYYY-MM-DD.jsonl"))
	} else {
		fmt.Printf("   JSON 调试日志: 已关闭 (%s 中 debug.enabled=false)\n", runtimeConfigFile)
	}
	fmt.Printf("   上游超时:      响应头等待 %v / 流空闲 %v (%s 可覆盖)\n",
		upstreamHeaderTimeout, upstreamIdleTimeout, runtimeConfigFile)
	fmt.Printf("   网络容错:      瞬时错误重试 %d 次 / 间隔 %v（仅未输出前重试，%s 可覆盖）\n",
		upstreamTransientRetries, upstreamRetryBackoff, runtimeConfigFile)
	if modelFilterConfigured() {
		blocked, allowed := modelFilterSummary()
		fmt.Printf("   模型黑白名单:  已启用 (黑名单 %d 个 / 白名单 %d 个，被禁模型已从列表隐藏并拒绝请求)\n", blocked, allowed)
	} else {
		fmt.Printf("   模型黑白名单:  未启用 (%s 中 models 段可配置)\n", runtimeConfigFile)
	}
	fmt.Printf("   模型账号名单:  %d 个模型配置了凭据文件规则（%s）\n", modelAccountRuleCount(), runtimeConfigFile)
	fmt.Println("   配置热生效:    每 1s 扫描 config.json；端口变更需重启")

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
	go runtimeConfigLoop(configCtx)

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopChan)
	serveErrors := make(chan error, len(servers))
	for i := range servers {
		go func(i int) {
			if err := servers[i].Serve(listeners[i]); err != nil && err != http.ErrServerClosed {
				serveErrors <- err
			}
		}(i)
	}
	select {
	case <-stopChan:
	case err := <-serveErrors:
		log.Printf("[服务异常] traceId=%s 原因=%v 说明=同时关闭API与管理监听，避免半可用状态", newTraceID(), err)
	}
	fmt.Println("\n正在关闭网关服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var shutdown sync.WaitGroup
	for _, server := range servers {
		shutdown.Go(func() {
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
		})
	}
	shutdown.Wait()
	fmt.Println("网关已安全停止。")
}

func printAPIAuthBanner(w io.Writer) {
	fmt.Fprintln(w, "   API 鉴权:      已启用 (密钥已隐藏)")
}

func backgroundTokenRefresher() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	logScheduledTask("token_check", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(5*time.Minute), "")
	for tick := range ticker.C {
		next := tick.Add(5 * time.Minute)
		trace, started := startScheduledTask("token_check", next)
		accountMu.Lock()
		accs := make([]*Account, len(accounts))
		copy(accs, accounts)
		accountMu.Unlock()
		failed := 0
		for _, acc := range accs {
			if err := ensureValidTokenFor(acc); err != nil {
				failed++
				log.Printf("[BackgroundAuth] 账号 %s 自动检查/续期令牌异常: %v", acc.Path, err)
			}
		}
		status := "success"
		if len(accs) == 0 {
			status = "skipped"
		} else if failed == len(accs) {
			status = "failed"
		} else if failed > 0 {
			status = "partial"
		}
		logScheduledTask("token_check", "completed", status, trace, started, next, fmt.Sprintf("检查账号=%d，异常=%d；仅临近过期时续期", len(accs), failed))
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
		// 普通落盘审计也需要稳定的 TraceID / requestID；不能依赖可选 JSON 日志。
		ensureDebugRequestContext(r, traceID, start)
		debugFullIncomingHeaders(r)
		r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, &statsAttempt{}))
		if debugLoggingEnabled() {
			debugEvent(r, "info", "request_received", map[string]any{
				"message": "请求已进入网关，准备执行中间件与路由处理",
			})
		}
		debugFullIncomingHeaders(r)
		aw := &auditResponseWriter{ResponseWriter: w}
		log.Printf("[请求到达] traceId=%s 方法=%s 路径=%s 来源=%s 说明=请求已进入网关", traceID, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(aw, r)
		if aw.status == 0 {
			aw.status = http.StatusOK
		}
		statsFinishAttempt(r, aw.status)
		debugEvent(r, "info", "response_returned", map[string]any{
			"status_code": aw.status,
			"result":      http.StatusText(aw.status),
		})
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
			debugEvent(r, "info", "cors_preflight_completed", map[string]any{
				"result":  "allowed",
				"message": "CORS 预检已直接返回，请求未进入业务逻辑",
			})
			log.Printf("[中间件-CORS] traceId=%s 结果=通过并直接响应 说明=预检请求未进入业务方法", w.Header().Get("X-Trace-ID"))
			w.WriteHeader(http.StatusOK)
			return
		}
		debugEvent(r, "debug", "cors_check_passed", map[string]any{"result": "allowed"})
		log.Printf("[中间件-CORS] traceId=%s 结果=通过", w.Header().Get("X-Trace-ID"))
		next.ServeHTTP(w, r)
	})
}

// API 端口只保留原有健康与根路径免鉴权；管理端口使用独立鉴权链。
func isAuthExemptPath(p string) bool {
	return p == "/health" || p == "/ping" || p == "/"
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := currentAPIKey()
		if apiKey != "" && !isAuthExemptPath(r.URL.Path) {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			// Anthropic 客户端（Claude Code 等）用 x-api-key 头携带密钥。
			if token == "" {
				token = r.Header.Get("x-api-key")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				debugEvent(r, "warn", "authentication_rejected", map[string]any{
					"status_code":     http.StatusUnauthorized,
					"reason":          "invalid_api_key",
					"business_impact": "请求未进入业务逻辑",
				})
				log.Printf("[请求被拦截] traceId=%s 拦截层=API鉴权 结果=拒绝 原因=未提供有效API密钥 返回状态码=401 业务影响=请求未进入业务方法", w.Header().Get("X-Trace-ID"))
				writeUpstreamError(w, r, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
				return
			}
		}
		debugEvent(r, "debug", "authentication_passed", map[string]any{"result": "allowed"})
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
	prepareConversationContext(r, nil)
	traceID := w.Header().Get("X-Trace-ID")
	log.Printf("[业务入口] traceId=%s requestId=%d 业务=上游模型调用 请求体字节数=%d", traceID, reqID, len(upstreamBytes))
	recordModelRequest(modelName)
	accountMu.Lock()
	poolSize := len(accounts)
	accountMu.Unlock()
	if poolSize == 0 {
		debugEvent(r, "warn", "request_rejected_no_account", map[string]any{
			"status_code":     http.StatusUnauthorized,
			"reason":          "no_auth",
			"business_impact": "没有可用登录凭据，未调用上游模型",
		})
		recordModelFailure(modelName, "no_auth")
		writeUpstreamError(w, r, http.StatusUnauthorized, "no_auth", "未找到有效登录凭据，请先执行 login 命令扫码登录")
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
			if errors.Is(err, errModelAccountPolicy) {
				log.Printf("[请求被拒绝] traceId=%s requestId=%d 模型=%s 原因=账号黑白名单没有匹配的凭据 返回状态码=403 业务影响=未进入上游", traceID, reqID, modelName)
				debugEvent(r, "warn", "model_account_policy_rejected", map[string]any{
					"status_code": http.StatusForbidden, "reason": "没有符合模型账号名单的凭据", "business_impact": "未进入上游调用",
				})
				recordModelFailure(modelName, "账号名单拒绝")
				writeUpstreamError(w, r, http.StatusForbidden, "model_account_disabled", fmt.Sprintf("模型 %s 没有可用的凭据文件：已被账号黑白名单禁用", modelName))
				return nil, nil, nil, false
			}
			// 所有账号均不可用（冷却或失效）
			msg := fmt.Sprintf("无可用账号: %v", err)
			if lastAuthErr != "" {
				msg += " | 最近一次授权失效: " + truncate(lastAuthErr, 200)
			}
			if lastRateErr != "" {
				msg += " | 最近一次频率限制: " + truncate(lastRateErr, 200)
			}
			log.Printf("[#%d] %s", reqID, msg)
			debugEvent(r, "warn", "request_rejected_no_available_account", map[string]any{
				"status_code":     http.StatusServiceUnavailable,
				"reason":          "no_available_account",
				"business_impact": "未调用上游模型",
			})
			recordModelFailure(modelName, "无可用账号")
			writeUpstreamError(w, r, http.StatusServiceUnavailable, "no_available_account", msg)
			return nil, nil, nil, false
		}
		attempted[acc] = true
		debugSetAccount(r, acc.Path)
		debugEvent(r, "debug", "account_selected", map[string]any{
			"selection": string(selection),
			"attempt":   attempt + 1,
		})
		if modelAccountRuleConfigured(modelName) {
			// 仅在实际命中账号的日志里附一个短标记，不再逐账号刷名单日志。
			log.Printf("[#%d] 账号名单命中: 模型=%s 凭据=%s（已按 config.json 账号黑白名单筛选）", reqID, modelName, filepath.Base(acc.Path))
			debugEvent(r, "debug", "model_account_policy_hit", map[string]any{
				"model": modelName, "account_file": filepath.Base(acc.Path),
			})
		}
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

		debugEvent(r, "info", "upstream_request_started", map[string]any{
			"upstream_host": prof.Base,
			"attempt":       attempt + 1,
			"payload_bytes": len(upstreamBytes),
		})
		// 注入 CodeBuddy 凭据与指纹 Header
		// 限制同一账号向腾讯上游的请求严格单并发串行排队，防止并发双发触发腾讯风控
		acc.lock.Lock()
		statsSelectAccount(r, acc, modelName)
		resp, upstreamCancel, err := doUpstreamRequest(r, acc, prof, upstreamBytes, reqID, traceID, sess)
		acc.lock.Unlock()
		if err != nil {
			debugEvent(r, "error", "upstream_request_failed", map[string]any{
				"upstream_host": prof.Base,
				"error_type":    debugErrorType(err),
				"error":         safeDebugError(err),
			})
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游网络调用 账号=%s 异常=%v 业务影响=本次模型请求失败 是否已处理=是", traceID, reqID, acc.Path, err)
			log.Printf("[#%d] 账号 %s [%s] 上游请求失败: %v", reqID, acc.Path, prof.Label, err)
			recordModelFailure(modelName, "网络错误")
			writeUpstreamError(w, r, http.StatusBadGateway, "upstream_network_error", fmt.Sprintf("网络转发失败: %v", err))
			return nil, nil, nil, false
		}

		// 上游非 200 响应处理
		if resp.StatusCode >= 400 {
			statsFinishAttempt(r, resp.StatusCode)
			// 错误响应同样可能按我们声明的 Accept-Encoding 压缩，故与成功路径一样先解压：
			// isQuotaExhausted / isRateLimited / isAuthFailure 的判定与日志都依赖响应正文里的
			// 关键字与 JSON，若拿压缩字节去判定会全部落空（限流/额度/鉴权会被误判成未知错误）。
			errBody, errRead := readDecodedAll(resp.Body, resp.Header.Get("Content-Encoding"))
			resp.Body.Close()
			upstreamCancel()
			if errRead != nil {
				debugEvent(r, "error", "upstream_error_body_undecodable", map[string]any{
					"error_type":      "upstream_error_body_undecodable",
					"error":           safeDebugError(errRead),
					"status_code":     resp.StatusCode,
					"business_impact": "上游错误正文无法解压，按残留字节判定错误类型并原样返回状态码",
				})
				log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游错误响应解压 账号=%s 状态码=%d 异常=%v 业务影响=按上游错误返回，但错误正文不可读", traceID, reqID, acc.Path, resp.StatusCode, errRead)
			}
			errStr := string(errBody)
			debugFullUpstreamError(r, resp, errBody)
			debugEvent(r, "warn", "upstream_response_rejected", map[string]any{
				"upstream_host": prof.Base,
				"status_code":   resp.StatusCode,
				"error_code":    modelStatusFromError(resp.StatusCode, errStr),
			})
			log.Printf("[#%d] 账号 %s [%s] 上游返回 HTTP %d: %s (耗时 %v)", reqID, acc.Path, prof.Label, resp.StatusCode, errStr, time.Since(startTime))
			log.Printf("[外部接口] traceId=%s requestId=%d 上游=%s 状态码=%d 结果=失败 账号=%s", traceID, reqID, prof.Base, resp.StatusCode, acc.Path)

			if isQuotaExhausted(resp.StatusCode, errStr) {
				markModelQuotaBlocked(acc, modelName, errStr)
				if selection == selectionProbeExhausted {
					msg := fmt.Sprintf("当前模型 %s 已在余额耗尽账号 %s 上完成受控探测并确认需要付费额度，本次不再探测其他耗尽账号", modelName, acc.Path)
					log.Printf("[#%d] %s", reqID, msg)
					recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
					writeUpstreamError(w, r, http.StatusServiceUnavailable, "model_requires_quota", msg)
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
					writeUpstreamError(w, r, http.StatusServiceUnavailable, "model_rate_limited", msg)
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
			writeUpstreamError(w, r, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream %d: %s", resp.StatusCode, errStr))
			return nil, nil, nil, false
		}

		debugEvent(r, "info", "upstream_response_received", map[string]any{
			"upstream_host": prof.Base,
			"status_code":   resp.StatusCode,
		})
		log.Printf("[外部接口] traceId=%s requestId=%d 上游=%s 状态码=%d 结果=成功 账号=%s", traceID, reqID, prof.Base, resp.StatusCode, acc.Path)
		recordModelSuccess(modelName)
		// 用空闲看门狗包装响应体：持续有数据就永不超时，超过 upstreamIdleTimeout 无数据才中断。
		// 关闭响应体时会一并取消上游 context，完成清理。
		resp.Body = newIdleReadCloser(resp.Body, currentIdleTimeout(), upstreamCancel)

		// 上游可能按我们声明的 Accept-Encoding 压缩响应（br / gzip / deflate）。
		// 解压层包在看门狗**外侧**：卡死判定仍按原始字节的到达节奏，下游与聚合逻辑
		// 则始终读到明文 SSE。
		decodedBody, decodeErr := wrapUpstreamDecoding(resp.Body, resp.Header.Get("Content-Encoding"))
		if decodeErr != nil {
			resp.Body.Close()
			debugEvent(r, "error", "upstream_encoding_unsupported", map[string]any{
				"error_type":      "upstream_encoding_unsupported",
				"error":           safeDebugError(decodeErr),
				"business_impact": "无法解压上游响应，已按上游错误返回，未伪造内容",
			})
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游响应解压 账号=%s 异常=%v 业务影响=本次请求失败", traceID, reqID, acc.Path, decodeErr)
			writeUpstreamError(w, r, http.StatusBadGateway, "upstream_encoding_unsupported", decodeErr.Error())
			return nil, nil, nil, false
		}
		resp.Body = decodedBody
		return resp, acc, prof, true
	}

	// 理论上不可达（poolSize 次尝试后未成功即已在循环内返回）
	recordModelFailure(modelName, "all_cooldown")
	debugEvent(r, "warn", "request_rejected_all_cooldown", map[string]any{
		"status_code":     http.StatusTooManyRequests,
		"reason":          "all_accounts_cooldown",
		"business_impact": "未调用上游模型",
	})
	writeUpstreamError(w, r, http.StatusTooManyRequests, "all_accounts_cooldown", "所有账号均处于冷却状态")
	return nil, nil, nil, false
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		debugFullIncomingBody(r, bodyBytes, false, true)
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	// 用保序解析替代 map[string]any：客户端以 JSON.stringify 发送，键序即属性插入顺序，
	// 经 map 中转后重编码会按键名字典序输出（messages 跑到 model 之前），构成机器特征。
	decodeStarted := time.Now()
	reqObj, decodeErr := decodeOrderedJSON(bodyBytes)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		debugFullIncomingBody(r, bodyBytes, false, false)
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

	streamRaw, _ := reqObj.Get("stream")
	isStreamBool, _ := streamRaw.(bool)
	debugSetModelAndStream(r, modelStr, isStreamBool)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)
	debugFullIncomingBody(r, bodyBytes, true, false)
	prepareConversationContext(r, reqObj)

	// 模型黑白名单拦截：命中即拒绝，不消耗任何上游账号额度。
	if disabled, reason := modelDisabled(modelStr); disabled {
		log.Printf("[请求被拒绝] traceId=%s requestId=%d 拦截层=模型黑白名单 模型=%s 结果=拒绝 原因=%s 返回状态码=403 业务影响=请求未进入上游调用",
			w.Header().Get("X-Trace-ID"), reqID, modelStr, reason)
		debugEvent(r, "warn", "model_blocked_by_config", map[string]any{
			"status_code":     http.StatusForbidden,
			"reason":          reason,
			"business_impact": "模型被网关配置禁用，请求未进入上游调用",
		})
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	// 腾讯上游强制要求 stream 必须为 true，非流式会被拦截 (code 11101)
	reqObj.Set("stream", true)

	prepareUpstreamChatRequest(reqObj, r, reqID, modelStr)

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
		streamChatResponseResilient(w, r, resp, modelStr, reqID, acc, prof, startTime, upstreamBytes)
	} else {
		writeChatAggregate(w, r, resp, modelStr, reqID, acc, prof, startTime)
	}
}

// streamChatResponse 将上游 SSE 逐行透传为 OpenAI Chat Completions 流式响应。
func streamChatResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	streamChatResponseWithReplay(w, r, resp, modelName, reqID, acc, prof, startTime, nil)
}

// streamChatResponseResilient 在流式空闲期发送 SSE 注释，并允许在首个有效
// 数据下发前对瞬时网络中断安全重放一次。
func streamChatResponseResilient(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, upstreamBytes []byte) {
	streamChatResponseWithReplay(w, r, resp, modelName, reqID, acc, prof, startTime, upstreamBytes)
}

func streamChatResponseWithReplay(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, upstreamBytes []byte) {
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	guard := &streamWriteGuard{w: w, flusher: flusher}
	heartbeat := startStreamHeartbeat(guard, currentStreamKeepalive())
	defer heartbeat.Stop()

	var finalBody *ttftReader
	defer func() {
		if finalBody != nil {
			finishFirstToken(r, modelName, reqID, finalBody)
		}
	}()

	replayBudget := 0
	if upstreamBytes != nil && currentFirstByteRetry() {
		replayBudget = 1
	}
	replays := 0

	for attempt := 0; ; attempt++ {
		body := newTTFTReader(resp.Body, startTime)
		finalBody = body
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var usage map[string]any
		finishReason := ""
		sawDone := false
		commentLines, metadataLines := 0, 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, ":") {
				// SSE 注释是保活信号，不是 JSON。不能包装成 data: : heartbeat，
				// 否则 CPA 的 JSON/SSE 解析器会拒绝首帧并取消整个请求。
				commentLines++
				guard.writeUpstreamComment(line)
				continue
			}
			if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "{") && line != "[DONE]" {
				// event/id/retry 及未知 SSE 字段不属于 Chat Completions 的数据。
				// 保留既有裸 JSON 和重复 data: 前缀的兼容性。
				metadataLines++
				continue
			}
			cleanData := stripDataPrefix(line)
			if cleanData == "" {
				continue
			}
			if cleanData == "[DONE]" {
				// 先记住，等确认有真实 finish_reason 再转发。
				sawDone = true
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(cleanData), &chunk) == nil {
				observeFirstToken(body, chunk)
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				finishReason = noteFinishReason(finishReason, chunk)
			}
			if cleanedChunk := cleanChunkJSON(cleanData); cleanedChunk != "" {
				guard.writeData(cleanedChunk)
			}
		}
		scanErr := scanner.Err()
		if commentLines > 0 || metadataLines > 0 {
			debugEvent(r, "debug", "stream_sse_framing_checked", map[string]any{
				"comment_lines": commentLines, "metadata_lines": metadataLines,
				"business_impact": "心跳按SSE注释转发，元数据不包装为JSON数据帧，避免CPA拒绝首包",
			})
			log.Printf("[流式协议] traceId=%s requestId=%d 阶段=Chat流转发 心跳注释数=%d 忽略元数据行数=%d 结果=已按SSE字段区分 业务影响=心跳不混入JSON数据帧；不记录正文",
				debugTraceID(r), reqID, commentLines, metadataLines)
		}

		if canReplayStream(scanErr, guard.hasData(), replays < replayBudget, r.Context().Err() != nil) {
			replays++
			log.Printf("[首字节前重放] traceId=%s requestId=%d 模型=%s 账号=%s 原因=首个有效字节前上游连接中断（%v），客户端尚未收到内容，安全重放 第%d次",
				debugTraceID(r), reqID, modelName, acc.Path, safeDebugError(scanErr), replays)
			debugEvent(r, "warn", "stream_first_byte_replay", map[string]any{
				"reason":          safeDebugError(scanErr),
				"attempt":         attempt + 1,
				"business_impact": "客户端未收到任何有效内容，重放可规避边缘节点静默期误杀；会额外消耗一次上游额度",
			})
			_ = resp.Body.Close()
			resp = nil
			newResp, newAcc, newProf, ok := replayUpstreamChat(w, r, reqID, modelName, upstreamBytes, startTime, guard)
			if !ok {
				return
			}
			resp, acc, prof = newResp, newAcc, newProf
			continue
		}

		completeTokenStream(body, scanErr == nil && finishReason != "")
		if scanErr != nil {
			// 流被中断（上游卡死 / 超时 / 客户端取消）时，绝不补发 [DONE]：
			// 补发等于告诉下游「正常结束」，会让残缺的工具调用被当成完整结果执行。
			debugEvent(r, "error", "stream_response_failed", map[string]any{
				"error_type": debugErrorType(scanErr),
				"error":      safeDebugError(scanErr),
			})
			reason := "上游流式响应中断，本次回复不完整"
			if errors.Is(scanErr, context.DeadlineExceeded) {
				reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", currentIdleTimeout())
			}
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": reason,
					"type":    "upstream_stream_interrupted",
					"code":    "stream_interrupted",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流式读取 账号=%s 异常=%v 业务影响=本次响应不完整，已向客户端下发中断错误而非伪造结束", debugTraceID(r), reqID, acc.Path, scanErr)
		} else if finishReason == "" {
			// 连接是正常关掉的（没有读错误），但整段流没有非空 finish_reason。
			// 这不是成功：不能补 [DONE]，也不能记成 stream_response_completed。
			debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
				"error_type":      "stream_closed_without_finish",
				"finish_reason":   "",
				"saw_done":        sawDone,
				"business_impact": "上游干净结束但没有 finish_reason，已拒绝当成成功，未补发 [DONE]",
			})
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": errStreamClosedWithoutFinish.Error(),
					"type":    "upstream_stream_incomplete",
					"code":    "stream_closed_without_finish",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流结束 账号=%s 结果=拒绝当成成功 原因=连接正常关闭但没有非空 finish_reason 是否看到DONE=%t 业务影响=不补发 [DONE]，客户端收到 stream_closed_without_finish 是否已处理=是",
				debugTraceID(r), reqID, acc.Path, sawDone)
		} else {
			if sawDone {
				guard.writeData("[DONE]")
			}
			debugEvent(r, "info", "stream_response_completed", map[string]any{
				"status_code":   http.StatusOK,
				"finish_reason": finishReason,
				"saw_done":      sawDone,
			})
		}
		observeModelCredit(acc, modelName, usage, reqID)
		statsObserveUsage(r, usage, scanErr == nil && finishReason != "")
		recordModelTokens(modelName, usage, reqID)
		recordModelLatency(modelName, time.Since(startTime))
		if scanErr == nil && finishReason != "" {
			log.Printf("[#%d] 流式输出完成 (账号 %s [%s], 结束原因=%s, 耗时 %v, 首Token(v2) %s)", reqID, acc.Path, prof.Label, finishReason, time.Since(startTime), body.text())
		}
		return
	}
}

// writeChatAggregate 聚合上游 SSE 为完整 Chat Completions JSON 响应。
func writeChatAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionJSON)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(completionJSON),
	})
	log.Printf("[#%d] 非流式响应完成 (账号 %s [%s], 耗时 %v, 首Token(v2) %s)", reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

// -----------------------------------------------------------------------------
// 路由处理: /v1/models (模型清单由 models.go 动态同步 + 静态兜底合并)
// -----------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	modelIDs, source := mergedModelIDs()
	modelsList := make([]map[string]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		entry := map[string]any{
			"id": id, "object": "model", "owned_by": "workbuddy", "permission": []any{},
		}
		// 能力扩展字段：只透传目录里真实存在的值，未知一律省略，不做 1M 之类兜底。
		// context_length 是模型能力上限，default_context_length 是客户端默认预算，
		// 两者语义不同，必须分开给，不能合并成一个值。
		// 这里只补充描述信息，不注入上游请求参数、不截断消息，也不改变输出预算逻辑。
		if mc, ok := modelCapabilityFor(id); ok {
			if mc.Ceiling > 0 {
				entry["context_length"] = mc.Ceiling
			}
			if mc.MaxOutput > 0 {
				entry["max_output_tokens"] = mc.MaxOutput
			}
			if mc.Default > 0 {
				entry["default_context_length"] = mc.Default
			}
			if len(mc.Options) >= 2 {
				entry["context_length_options"] = mc.Options
			}
			if len(mc.BySite) > 1 {
				// 站点之间能力不一致：顶层已留空，明细在此，避免下游按较大值预留。
				entry["capabilities_by_site"] = mc.BySite
			}
		}
		modelsList = append(modelsList, entry)
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
	_, _ = fmt.Fprintf(w, "WorkBuddy Local Gateway v%s is running.\n\nEndpoints:\n- POST /v1/chat/completions\n- POST /v1/responses\n- POST /v1/messages (Anthropic)\n- POST /v1/messages/count_tokens\n- GET  /v1/models\n- GET  /health\n", version)
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
func applyThinkingRules(obj any, modelName ...string) {
	switch o := obj.(type) {
	case *jsonObject:
		hoistNestedReasoningFields(o)
		effRaw, _ := o.Get("reasoning_effort")
		eff := normalizeReasoningEffort(effRaw)
		if eff == "" {
			o.Delete("reasoning_effort")
			o.Delete("reasoning_summary")
			return
		}
		o.Set("reasoning_effort", eff)
	case map[string]any:
		// 上游简化版：仅当客户端显式传 reasoning_effort 时规范化
		effRaw, _ := o["reasoning_effort"]
		eff := normalizeReasoningEffort(effRaw)
		if eff == "" {
			delete(o, "reasoning_effort")
			delete(o, "reasoning_summary")
			return
		}
		o["reasoning_effort"] = eff
	}
}

func sanitizeMessages(obj any) int {
	var arr []any
	switch o := obj.(type) {
	case *jsonObject:
		raw, ok := o.Get("messages")
		if !ok {
			return 0
		}
		arr, ok = raw.([]any)
		if !ok {
			return 0
		}
	case map[string]any:
		var ok bool
		arr, ok = o["messages"].([]any)
		if !ok {
			return 0
		}
	}
	modified := 0
	for _, m := range arr {
		// OpenAI developer role is an alias for system and is rejected by the upstream.
		if roleOfMessage(m) == "developer" {
			setMessageField(m, "role", "system")
		}
		if roleOfMessage(m) == "system" {
			content, changed := sanitizeSystemMessageContent(messageField(m, "content"))
			if changed {
				setMessageField(m, "content", content)
				modified++
			}
		}
	}
	return modified
}

// ensureLeadingSystemMessageOrdered 是 ensureLeadingSystemMessage 的 *jsonObject 适配版。
// 返回值含义相同：是否注入了保底 system。
func ensureLeadingSystemMessageOrdered(obj *jsonObject) bool {
	raw, ok := obj.Get("messages")
	if !ok {
		obj.Set("messages", []any{newObject("role", "system", "content", configuredFallbackSystemPrompt())})
		return true
	}
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		obj.Set("messages", []any{newObject("role", "system", "content", configuredFallbackSystemPrompt())})
		return true
	}

	switch roleOfMessage(messages[0]) {
	case "system":
		return false
	case "developer":
		setMessageField(messages[0], "role", "system")
		return false
	}

	// 后续存在 system/developer：提升到首位，其余保持原序
	for i := 1; i < len(messages); i++ {
		switch roleOfMessage(messages[i]) {
		case "system", "developer":
			setMessageField(messages[i], "role", "system")
			reordered := make([]any, 0, len(messages))
			reordered = append(reordered, messages[i])
			reordered = append(reordered, messages[:i]...)
			reordered = append(reordered, messages[i+1:]...)
			obj.Set("messages", reordered)
			return false
		}
	}

	// 无任何 system：在最前注入保底 system（兼容国内站/国际站）
	injected := make([]any, 0, len(messages)+1)
	injected = append(injected, newObject("role", "system", "content", configuredFallbackSystemPrompt()))
	injected = append(injected, messages...)
	obj.Set("messages", injected)
	return true
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
//
// 返回值只表示本次是否注入了保底 system，用于不记录提示词正文的审计日志。
func ensureLeadingSystemMessage(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		obj["messages"] = []any{map[string]any{"role": "system", "content": configuredFallbackSystemPrompt()}}
		return true
	}

	switch roleOfMessage(messages[0]) {
	case "system":
		return false
	case "developer":
		if msg, ok := messages[0].(map[string]any); ok {
			msg["role"] = "system"
		}
		return false
	}

	// 后续存在 system/developer：提升到首位，其余保持原序
	for i := 1; i < len(messages); i++ {
		switch roleOfMessage(messages[i]) {
		case "system", "developer":
			setMessageField(messages[i], "role", "system")
			reordered := make([]any, 0, len(messages))
			reordered = append(reordered, messages[i])
			reordered = append(reordered, messages[:i]...)
			reordered = append(reordered, messages[i+1:]...)
			obj["messages"] = reordered
			return false
		}
	}

	// 无任何 system：在最前注入保底 system（兼容国内站/国际站）
	injected := make([]any, 0, len(messages)+1)
	injected = append(injected, map[string]any{"role": "system", "content": configuredFallbackSystemPrompt()})
	injected = append(injected, messages...)
	obj["messages"] = injected
	return true
}

// roleOfMessage 读取消息的 role 字段并归一化为小写去空格；非法结构返回空串。
// 同时接受保序对象与 map：入站请求是前者，工具序列修复的中间结构是后者。
func roleOfMessage(m any) string {
	role, _ := messageField(m, "role").(string)
	return strings.ToLower(strings.TrimSpace(role))
}

// messageFieldHas 判断消息字段是否存在（即使值为 nil）。
func messageFieldHas(m any, key string) bool {
	switch msg := m.(type) {
	case *jsonObject:
		if msg == nil {
			return false
		}
		_, ok := msg.Get(key)
		return ok
	case map[string]any:
		_, ok := msg[key]
		return ok
	default:
		return false
	}
}

// messageField 读取消息字段。保序对象与 map 都支持，其余类型返回 nil。
func messageField(m any, key string) any {
	switch msg := m.(type) {
	case *jsonObject:
		if msg == nil {
			return nil
		}
		v, _ := msg.Get(key)
		return v
	case map[string]any:
		return msg[key]
	default:
		return nil
	}
}

// setMessageField 写入消息字段。类型不支持时为空操作。
func setMessageField(m any, key string, v any) {
	switch msg := m.(type) {
	case *jsonObject:
		if msg != nil {
			msg.Set(key, v)
		}
	case map[string]any:
		msg[key] = v
	}
}

// deleteMessageField 删除消息字段。类型不支持时为空操作。
func deleteMessageField(m any, key string) {
	switch msg := m.(type) {
	case *jsonObject:
		if msg != nil {
			msg.Delete(key)
		}
	case map[string]any:
		delete(msg, key)
	}
}

// sanitizeBlockedTemplates 改写上游会拦截的客户端模板原文。
// 只替换固定句子，不改写其余正文。
func sanitizeBlockedTemplates(s string) string {
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	return s
}

// newTraceID 生成 32 位小写十六进制 trace id（OTel traceId 形态，无连字符）。
func newTraceID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// newSpanID 生成 16 位小写十六进制 span id（OTel spanId 形态）。
func newSpanID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
}

// newUUIDv7 生成 RFC 9562 UUID v7（48 位毫秒时间戳 + 74 位随机），带连字符小写形态。
//
// 客户端自 2.156.0 起用 uuidv7 生成会话/消息 ID（bundled CLI 依赖 uuidv7@^1.2.1，
// 源码 `nH: () => uuidv7`，X-Conversation-ID = nH()，X-Request-ID = nH().replace(/-/g,"")），
// 抓包实测会话/消息/请求 ID 的版本位恒为 7、前 48 位为请求时刻的毫秒时间戳。
// 随机 UUID v4 在该版本位与时间前缀上与之不符，属可静态识别的偏差。
func newUUIDv7() string {
	u, err := uuid.NewV7()
	if err != nil {
		// 仅在系统熵源不可用时发生；回退到随机 UUID，保持长度与形态可用。
		return uuid.New().String()
	}
	return u.String()
}

// newUUIDv7Compact 返回去掉连字符的 32 位形态（X-Request-ID / X-Conversation-Message-ID
// / X-Conversation-Request-ID 等客户端以 `.replace(/-/g,"")` 发出的字段）。
func newUUIDv7Compact() string {
	return strings.ReplaceAll(newUUIDv7(), "-", "")
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
	// 取前 16 字节（32 位 hex），与客户端 X-Conversation-Request-ID 同形态。
	return sessionScope{
		conversationID:        conversationID,
		conversationRequestID: derivedCompactID(conversationID),
	}
}

// sessionScopeSeed 是派生用的域分隔前缀，避免与其它哈希用途产生同值。
const sessionScopeSeed = "workbuddy-gateway/session/"

// derivedUUID 由会话键派生一个带连字符的 UUID 形态标识（不用于承载任何真实语义，
// 仅当下游缺失 X-Conversation-ID 时保持该头的形态恒定）。
func derivedUUID(key string) string { return dashUUID(derivedCompactID(key)) }

// derivedCompactID 由会话键派生 32 位 hex 的 UUID 紧凑形态 ID。
//
// 形态对齐：客户端 2.156.0 发出的会话/消息 ID 均为 UUID v7（版本位 7，前 48 位为
// 创建时刻的毫秒时间戳）。派生值沿用同一套位域布局 —— 时间戳取自同类 ID 自身
// （即该会话的真实创建时刻），其余位由会话键哈希填充：
//   - 请求 ID 派生自会话 ID 时，两者前 48 位同值，与客户端「同一次会话内生成」的
//     时序关系一致，且派生可复现、无状态；
//   - 模板非 v7（老客户端）时退化为 v4 位域，避免产出客户端不会生成的随机版本位。
//
// 不泄露会话键本身：输出与会话键同为 32 位 hex，无法反推。
func derivedCompactID(key string) string {
	sum := sha256.Sum256([]byte(sessionScopeSeed + key))
	var b [16]byte
	copy(b[:], sum[:16])
	if ts, ok := uuidV7Timestamp(key); ok {
		b[0] = byte(ts >> 40)
		b[1] = byte(ts >> 32)
		b[2] = byte(ts >> 24)
		b[3] = byte(ts >> 16)
		b[4] = byte(ts >> 8)
		b[5] = byte(ts)
		b[6] = 0x70 | (b[6] & 0x0f) // version 7
	} else {
		b[6] = 0x40 | (b[6] & 0x0f) // version 4
	}
	b[8] = 0x80 | (b[8] & 0x3f) // RFC 4122 variant
	return hex.EncodeToString(b[:])
}

// uuidV7Timestamp 从 UUID v7（32 位紧凑或带连字符形态）解析前 48 位毫秒时间戳。
func uuidV7Timestamp(s string) (uint64, bool) {
	compact := strings.ReplaceAll(s, "-", "")
	if !isHexLen(compact, 32) || compact[12] != '7' {
		return 0, false
	}
	ts, err := strconv.ParseUint(compact[:12], 16, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// dashUUID 把 32 位紧凑形态切回客户端 X-Conversation-ID 使用的带连字符形态。
func dashUUID(compact string) string {
	if !isHexLen(compact, 32) {
		return compact
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", compact[0:8], compact[8:12], compact[12:16], compact[16:20], compact[20:32])
}

// ok 表示会话键存在，会话级 ID 可用；否则调用方应回退到每请求新值。
func (s sessionScope) ok() bool { return s.conversationRequestID != "" }

// buildUserAgent 复刻客户端 UserAgentHttpInterceptor 的组装口径：
// `${platform}/${platformVersion} ${productName}/${productVersion}`，
// 桌面宿主再追加 `CLI/${cliVersion}`。
//
// 国内站桌面宿主（WorkbuddyUserAgentHttpInterceptor）的 platform 与
// applicationName 都取 product.json 的 applicationName（WorkBuddy），
// productName 取 product.json 的 productName（国内站也是 WorkBuddy，国际站是 WorkBuddy AI），
// 版本取 Electron app.getVersion()。国内站抓包：
// `WorkBuddy/5.5.3 WorkBuddy/5.5.3 CLI/2.137.1`；X-IDE-* 走另一路
// （clientInfo.ideType / platform = CLIENT_INFO_IDE_TYPE/PLATFORM），也是 WorkBuddy / 5.5.3。
//
// 国际站抓包：`WorkBuddy/5.7.6 WorkBuddy AI/5.7.6 CLI/2.156.0`。桌面宿主注入的值与
// bundled CLI 包版本（`cli/package.json` publishConfig.customPackage.version）相同，
// 故 UA 尾段与 X-IDE-Version 各自随宿主版本走。
func buildUserAgent(prof *upstreamProfile) string {
	name := prof.ProductName
	if name == "" {
		name = prof.PlatformName + " AI"
	}
	platform := prof.AppName
	if platform == "" {
		platform = prof.PlatformName
	}
	ua := fmt.Sprintf("%s/%s %s/%s", platform, prof.PlatformVersion, name, prof.PlatformVersion)
	if prof.CliVersion != "" {
		ua += " CLI/" + prof.CliVersion
	}
	return ua
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
		// 客户端 messageId = uuidv7().replace(/-/g,"")（源码 nH = uuidv7）。
		requestID = newUUIDv7Compact()
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
		// 客户端 X-Conversation-ID = uuidv7()（带连字符）。
		conversationID = newUUIDv7()
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

// setControlTraceHeaders 写控制面请求的链路头，复刻客户端 traceSpan.requestHeaders
// + CommonHeaderHttpInterceptor.injectRequestIdHeader 的组合。
//
// 与模型请求（setTraceHeaders）的差别，逐条对应实测与源码：
//   - 客户端 `OpenTelemetrySpan.requestHeaders` 只产出六项：
//     `{X-Trace-ID, b3, X-B3-TraceId, X-B3-ParentSpanId, X-B3-SpanId, X-B3-Sampled}`
//     —— **没有 traceparent**（那是模型请求链路单独加的），且 b3 为四段
//     `${trace}-${span}-${flags}-${parent}`，无父 span 时末段为空。
//   - `injectRequestIdHeader`（axios 请求拦截器，见 CommonHeaderHttpInterceptor）：
//     `X-Request-ID` 缺失时**取 X-Trace-ID**（取不到才随机 32 位 hex）。故控制面请求的
//     `X-Request-ID` 恒等于 `X-Trace-ID` —— 这与模型请求把两者解耦的口径相反。
//   - 控制面请求不带会话语义，无父 span：`X-B3-ParentSpanId` 为空值。
func setControlTraceHeaders(r *http.Request) {
	setControlTraceHeadersWithID(r, newTraceID(), newSpanID())
}

// setControlTraceHeadersWithID 用指定 trace/span 写控制面链路头。
//
// 客户端把两次请求放进同一逻辑操作（额度摘要 + 套餐资源列表）时，OTel 上下文里的
// traceId 相同、spanId 不同；网关据此在一次刷新周期内复用 trace、逐请求换 span，
// 既保留审计关联（plans_test 断言两请求 traceId 相同），也与客户端一致。
func setControlTraceHeadersWithID(r *http.Request, traceID, spanID string) {
	setHeaderExact(r.Header, "X-Trace-ID", traceID)
	setHeaderExact(r.Header, "X-Request-ID", traceID)
	setHeaderExact(r.Header, "b3", fmt.Sprintf("%s-%s-1-", traceID, spanID))
	setHeaderExact(r.Header, "X-B3-TraceId", traceID)
	setHeaderExact(r.Header, "X-B3-ParentSpanId", "")
	setHeaderExact(r.Header, "X-B3-SpanId", spanID)
	setHeaderExact(r.Header, "X-B3-Sampled", "1")
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
// 白名单只收「WorkBuddy 客户端族专有的身份头」：只有官方客户端会发送它们，
// 透传即等于复现官方值。两类**不收**（改为一律按 Profile 合成）：
//   - `User-Agent`：第三方下游（Claude Code 等）会带自己的 UA
//     （如 `claude-cli/2.x (external, cli)`），透传后上游会看到「WorkBuddy 的
//     IDE 头 + 别的客户端的 UA」这种官方客户端不可能产生的组合；
//   - `x-stainless-*`：同理会带对方 SDK 的包版本（Anthropic SDK ≠ openai 6.25.0）。
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
}

// stainlessFingerprint 复刻官方客户端 chat 链路的 OpenAI Node SDK（stainless）指纹族。
// 客户端 chat 请求由 bundled openai SDK 发出，因此上游始终能看到这一组头；
// 网关自身用 Go net/http 发起请求，若不合成即为可识别的缺失特征。
// 取值来自真实抓包与客户端运行时探测。`x-stainless-runtime-version` 随宿主客户端版本走，
// 故不在此表中，而由各站点 Profile 的 NodeVersion 提供（见 upstreamProfile）。
// 用系统 Node（如 v24.x）或臆造值的安装会与桌面客户端不一致。
var stainlessFingerprint = map[string]string{
	"x-stainless-arch":            "x64",
	"x-stainless-lang":            "js",
	"x-stainless-os":              "Windows",
	"x-stainless-package-version": "6.25.0",
	"x-stainless-retry-count":     "0",
	"x-stainless-runtime":         "node",
}

// backendHeaders 设置上游专用鉴权 Header 与链路追踪 Header（按站点 Profile 生成）。
//
// 与真实客户端实测流量的对齐要点：
//   - 不发送 X-Client-ID / X-Client-Version：官方客户端全库无此字段，属可静态识别的机器特征。
//   - X-Request-ID 为 32 位 hex（客户端 uuidv7().replace(/-/g,"")），
//     与 X-Trace-ID 解耦：后者是 OTel traceId，全链路同一值。
//   - 补齐会话/Agent/IDE 骨架、OTel + B3 双份传播头与 stainless 指纹族，缺失同样是可识别特征。
//   - 下游客户端自带身份头时优先透传（applyClientPassthrough），仅在缺失时合成。
//
// 链路 ID 分两个作用域（见 sessionScope）：会话级由 sess 提供，每请求级在此新生成。
// 下游若已提供其中任一项，则采用下游值，并按客户端的不变量补齐其同组伙伴。
func backendHeaders(req *http.Request, sa *StoredAuth, prof *upstreamProfile, in http.Header, sess sessionScope) {
	commonHeaders(req, prof)

	var ids linkIDs
	if prepared := conversationHeadersFor(req); prepared != nil {
		ids = prepared.links
	} else {
		ids = resolveLinkIDs(in, sess)
	}
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

	// 本地网关安全头。客户端对模型请求恒发送该头，取值恒为 "1"
	// （源码 withSecurityHeader 注入；桌面宿主 daemon-bootstrap 的模型请求构造器
	// 亦为其模型请求显式 set。抓包实测：CLI 直连与桌面宿主两条链路均携带，且
	// 目标为非回环地址时同样携带 —— 它不是仅对本地网关生效的头）。
	setHeaderExact(req.Header, "x-codebuddy-request", "1")

	// OpenAI SDK 指纹族
	for k, v := range stainlessFingerprint {
		setHeaderExact(req.Header, k, v)
	}
	// 运行该 SDK 的 Node 进程版本随宿主客户端走（两站安装的 Electron 均为 37.10.3 → v22.21.1）。
	setHeaderExact(req.Header, "x-stainless-runtime-version", prof.NodeVersion)

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
		// 客户端 AuthenticationHttpInterceptor 在账号存在 enterpriseId 时无条件补 X-Tenant-Id
		// （源码中该分支唯一的例外是 X-No-Enterprise-Id：它只拦 X-Enterprise-Id，不拦 X-Tenant-Id），
		// 取值与 X-Enterprise-Id 相同；企业账号请求缺失该头即为可识别偏差。
		setHeaderExact(req.Header, "X-Tenant-Id", sa.Account.EnterpriseID)
	} else if !authed {
		setHeaderExact(req.Header, "X-No-Enterprise-Id", "true")
		setHeaderExact(req.Header, "X-No-Department-Info", "true")
	}

	// 注意：X-Refresh-Token 只出现在 auth/token/refresh 与 account/switch 链路
	// （客户端源码 enterpriseHeaders 之外单独注入），chat 请求实测不携带，故此处不设置。

	// X-Domain 是 chat 请求的常驻头（客户端实测始终携带），无凭据时回退站点主机名。
	if sa != nil && sa.Auth.Domain != "" {
		setHeaderExact(req.Header, "X-Domain", sa.Auth.Domain)
	} else {
		setHeaderExact(req.Header, "X-Domain", prof.domain())
	}

	setHeaderExact(req.Header, "X-Product", prof.Product)

	// 压缩协商按站点 Profile 决定：国际站 CLI 2.156.0 由 Node http 适配器在传输层补
	// `Accept-Encoding: gzip, compress, deflate, br`（值来自 axios 默认头；线上字节抓包
	// 实测位于 Content-Length 之后、Host 之前）；国内站 CLI 2.137.1 不发该头（抓包实测）。
	// 发出时不代表上游压缩：网关按 Content-Encoding 流式解压响应
	// （wrapUpstreamDecoding），因此不需要「不声明以免上游压缩」的旧取舍。
	if prof.AcceptEncoding != "" {
		setHeaderExact(req.Header, "Accept-Encoding", prof.AcceptEncoding)
	}

	// 传输层头。客户端（Node http 模块）对模型请求恒发送 `Connection: keep-alive`，
	// 位置在头部块末位（见 .client_ref/capture2.jsonl 实测：Content-Length / Host /
	// Connection 恒为最后三位）。Go transport 默认不写该头，缺失即为可识别偏差；
	// 显式提供时 Go 会原样写出（Connection 不在 reqWriteExcludeHeader 内），
	// 且请求语义不变（HTTP/1.1 默认即长连接）。
	setHeaderExact(req.Header, "Connection", "keep-alive")
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

// daemonHeaders 设置桌面客户端**守护进程**（`workbuddy-server`）接口的 Header。
//
// 第三套头集合。守护进程经注入的 `serverFetch = (e,t)=>fetch(e,t)`（Electron 主进程的
// Node 全局 fetch = undici，见 `main/index.js` 的 `serverFetch` 注入）发出请求，头表来自
// `main/server.js` 的 `buildAuthHeaders`，与 axios 控制面（`controlHeaders`）、模型请求
// （`backendHeaders`）都**不同**：
//
//	{Accept: application/json, Authorization, X-User-Id, X-Enterprise-Id, X-Tenant-Id,
//	 X-Domain, X-B3-TraceId, X-B3-SpanId, X-B3-Sampled:"1", X-Trace-Id, b3:`${trace}-${span}-1`,
//	 X-Request-ID}
//
// 关键差别（逐条对应源码与实测）：
//   - 自动头来自 undici 而非 axios：`user-agent: node`、`accept-encoding: gzip, deflate`、
//     `accept-language: *`、`sec-fetch-mode: cors`、`connection: keep-alive`
//     （客户端运行时 Electron 37.10.3 / Node 22.21.1 / undici 6.22.0 实测逐字）。
//   - 链路头键名是 `X-Trace-Id`（非 `X-Trace-ID`），`b3` 只有三段且无尾空段
//     （axios 侧是四段 `${trace}-${span}-1-`），`X-Request-ID` 取 `X-Trace-Id`。
//   - **没有** `X-IDE-*` / `X-Agent-*` / `x-stainless-*` / `X-Product` / `traceparent`。
//   - GET 不设 `Content-Type`（`includeJsonContentType:false`），也无 `X-Domain` 缺省回退。
func daemonHeaders(req *http.Request, sa *StoredAuth) {
	setHeaderExact(req.Header, "Accept", "application/json")
	// undici 自动头（客户端运行时实测，大小写逐字）。
	// User-Agent 需要两步：Go 的 transport 只在 `Header` 里存在**规范化键** `User-Agent`
	// 时才用它的值（否则回退 `Go-http-client/1.1`），而客户端发的是全小写 `user-agent`。
	// 故把规范化键置空（transport 视为「显式不发送」并跳过），再写入小写键由 writeSubset 原样输出。
	setHeaderExact(req.Header, "User-Agent", "")
	setHeaderExact(req.Header, "user-agent", "node")
	setHeaderExact(req.Header, "accept-encoding", "gzip, deflate")
	setHeaderExact(req.Header, "accept-language", "*")
	setHeaderExact(req.Header, "sec-fetch-mode", "cors")
	setHeaderExact(req.Header, "Connection", "keep-alive")
	if sa == nil {
		return
	}
	if sa.Auth.AccessToken != "" {
		setHeaderExact(req.Header, "Authorization", "Bearer "+sa.Auth.AccessToken)
	}
	if sa.Account.UID != "" {
		setHeaderExact(req.Header, "X-User-Id", sa.Account.UID)
	}
	if sa.Account.EnterpriseID != "" {
		setHeaderExact(req.Header, "X-Enterprise-Id", sa.Account.EnterpriseID)
		setHeaderExact(req.Header, "X-Tenant-Id", sa.Account.EnterpriseID)
	}
	if sa.Auth.Domain != "" {
		setHeaderExact(req.Header, "X-Domain", sa.Auth.Domain)
	}
	traceID, spanID := newTraceID(), newSpanID()
	setHeaderExact(req.Header, "X-B3-TraceId", traceID)
	setHeaderExact(req.Header, "X-B3-SpanId", spanID)
	setHeaderExact(req.Header, "X-B3-Sampled", "1")
	setHeaderExact(req.Header, "X-Trace-Id", traceID)
	setHeaderExact(req.Header, "b3", traceID+"-"+spanID+"-1")
	setHeaderExact(req.Header, "X-Request-ID", traceID)
}

// controlHeaders 设置控制面请求（登录、令牌轮询/续期、账号快照）的 Header。
//
// 这类请求由客户端 axios 实例（RestOperations）发出，与模型请求（openai SDK + 客户端
// 自建头表）的头部集合**不同**，实测（CLI 2.156.0 直连非回环地址抓包）：
//   - Accept 为 axios 默认的 `application/json, text/plain, */*`（模型请求为单一 application/json）
//   - `X-Requested-With` 为规范化大小写（axios 侧经规范化；模型请求是小写 x-requested-with）
//   - 不携带 X-IDE-* / X-Agent-* / X-Conversation-* / x-stainless-*（这些只出现在模型请求）
//   - X-Product 由 ProductEndpointHttpInterceptor 对**每个**请求注入（值取 deploymentType，回退 SaaS）
//   - Content-Type 只在**带 body** 时出现：axios 仅在 `post(url, data)` 时补该头，
//     `get(url)` 不带（实测 CLI 2.156.0 的 `GET /v2/plugin/accounts`：Accept /
//     X-Requested-With / X-Domain / Authorization / 链路头 / X-Product / User-Agent /
//     X-User-Id / Accept-Encoding，**无** Content-Type、无 Content-Length）。
//     故这里以「请求是否带 body」判定，与客户端的 axios 语义一致：
//     auth/state、auth/token/refresh、额度/签到等 POST 带 Content-Type，
//     auth/token 轮询、login/account、模型目录等 GET 不带。
//
// 照搬模型请求的头表会同时产生「多出官方控制面请求没有的头」与「缺失官方控制面请求
// 必带的头」两类偏差，故控制面必须走这一套。
func controlHeaders(req *http.Request, prof *upstreamProfile) {
	if req.Body != nil {
		setHeaderExact(req.Header, "Content-Type", "application/json")
	}
	setHeaderExact(req.Header, "Accept", "application/json, text/plain, */*")
	setHeaderExact(req.Header, "X-Requested-With", "XMLHttpRequest")
	setHeaderExact(req.Header, "User-Agent", buildUserAgent(prof))
	setHeaderExact(req.Header, "X-Product", prof.Product)
	// 控制面压缩协商同模型请求一样按站点走：国际站 CLI 2.156.0 带完整的
	// `gzip, compress, deflate, br`，国内站 CLI 2.137.1 不带（抓包实测，两族一致）。
	// 响应解压由 doJSONContext 显式处理（transport 已禁用自动压缩）。
	if prof.AcceptEncoding != "" {
		setHeaderExact(req.Header, "Accept-Encoding", prof.AcceptEncoding)
	}
}

// loginStateHeaders 复刻客户端 fetchAuthState 的请求头集合（login 链路第一步）。
//
// 源码逐字（cli-external-link / account-scoped 两套实现同形）：
//
//	post(`/v2${prefixPath}/auth/state?platform=${authentication?.attributes?.platform ?? platform}`, {},
//	     {headers:{...this.enterpriseHeaders(), ...traceSpan.requestHeaders,
//	               "X-No-Authorization":"true","X-No-User-Id":"true",
//	               "X-No-Enterprise-Id":"true","X-No-Department-Info":"true"}})
//
// enterpriseHeaders() 无参时 X-Domain 取 configuration.authentication.endpoint || endpoint
// 的 authority，即站点主机名（www.workbuddy.ai）。
func loginStateHeaders(prof *upstreamProfile) func(*http.Request) {
	domain := prof.domain()
	return func(r *http.Request) {
		controlHeaders(r, prof)
		controlNoAuth(r, true)
		setHeaderExact(r.Header, "X-Domain", domain)
		setControlTraceHeaders(r)
	}
}

// loginPollHeaders 复刻客户端 loopGetToken 的请求头集合（auth/token 轮询）。
//
// 源码：`get(`/v2${prefixPath}/auth/token?state=${state}`, {headers:{...traceSpan.requestHeaders,
// "X-No-Authorization":"true","X-No-User-Id":"true","X-No-Enterprise-Id":"true","X-No-Department-Info":"true"}})`
//
// 与 auth/state 的唯一差别：**不带** X-Domain（该步不调 enterpriseHeaders）。
func loginPollHeaders(prof *upstreamProfile) func(*http.Request) {
	return func(r *http.Request) {
		controlHeaders(r, prof)
		controlNoAuth(r, true)
		setControlTraceHeaders(r)
	}
}

// loginAccountHeaders 复刻客户端 loopGetAccount 的请求头集合（登录成功后取账号快照）。
//
// 源码：`get(`/v2${prefixPath}/login/account?state=${state}`, {headers:{...this.enterpriseHeaders(auth),
// Authorization:`Bearer ${auth.accessToken}`, ...traceSpan.requestHeaders,
// "X-No-User-Id":"true","X-No-Enterprise-Id":"true","X-No-Department-Info":"true"}})`
//
// 该步必带 Authorization，故**不声明** X-No-Authorization；X-Domain 取 token 的 domain
// （enterpriseHeaders(auth) = auth.domain || URI.parse(endpoint).authority）。
func loginAccountHeaders(prof *upstreamProfile, accessToken, domain string) func(*http.Request) {
	if domain == "" {
		domain = prof.domain()
	}
	return func(r *http.Request) {
		controlHeaders(r, prof)
		controlNoAuth(r, false)
		setHeaderExact(r.Header, "Authorization", "Bearer "+accessToken)
		setHeaderExact(r.Header, "X-Domain", domain)
		setControlTraceHeaders(r)
	}
}

// controlNoAuth 声明控制面请求「不带身份类凭据」，复刻客户端 AuthenticationHttpInterceptor
// 的跳过语义：任一 X-No-* 置 "true" 即跳过对应头的自动注入。
//
// 客户端在各登录步骤里成组声明：
//   - auth/state：X-No-Authorization / X-No-User-Id / X-No-Enterprise-Id / X-No-Department-Info
//   - auth/token 轮询：同上四项
//   - login/account：X-No-User-Id / X-No-Enterprise-Id / X-No-Department-Info（该步必带 Authorization）
func controlNoAuth(req *http.Request, noAuthorization bool) {
	if noAuthorization {
		setHeaderExact(req.Header, "X-No-Authorization", "true")
	}
	setHeaderExact(req.Header, "X-No-User-Id", "true")
	setHeaderExact(req.Header, "X-No-Enterprise-Id", "true")
	setHeaderExact(req.Header, "X-No-Department-Info", "true")
}

// wrapUpstreamDecoding 按上游响应的 Content-Encoding 包装响应体，使调用方只看到明文。
//
// 网关向各上游端点都声明了压缩协商（模型请求 `gzip, compress, deflate, br`，控制面与
// 守护进程各一套，见 backendHeaders / controlHeaders / daemonHeaders），且 transport
// 关闭了自动压缩（DisableCompression），故响应解压必须由网关自己做，且必须与声明一致：
// gzip / deflate 用标准库，br 用纯 Go 的 brotli 解码器（无 CGO）。
//
// Content-Encoding 可能是逗号分隔的多段（RFC 9110：按声明顺序依次施加），故按逆序解码。
// 未支持的编码显式报错，绝不把压缩字节当明文交给下游或 JSON 解析。
//
// 失败时**不**关闭 body：所有权始终在调用方（上游响应体的关闭与连接归还由调用点负责）。
func wrapUpstreamDecoding(body io.ReadCloser, encoding string) (io.ReadCloser, error) {
	names := strings.Split(strings.ToLower(strings.TrimSpace(encoding)), ",")
	for i := len(names) - 1; i >= 0; i-- {
		switch name := strings.TrimSpace(names[i]); name {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			zr, err := gzip.NewReader(body)
			if err != nil {
				return nil, fmt.Errorf("解压上游 gzip 响应失败: %w", err)
			}
			body = &decodedBody{Reader: zr, inner: body, decoder: zr}
		case "deflate":
			zr, err := zlib.NewReader(body)
			if err != nil {
				return nil, fmt.Errorf("解压上游 deflate 响应失败: %w", err)
			}
			body = &decodedBody{Reader: zr, inner: body, decoder: zr}
		case "br":
			body = &decodedBody{Reader: brotli.NewReader(body), inner: body}
		default:
			return nil, fmt.Errorf("上游响应使用了网关不支持的压缩编码 %q", name)
		}
	}
	return body, nil
}

// decodedBody 把解压读取器与上游响应体配对。
//
// gzip/zlib 的 Close 只校验校验和、不关闭底层流，故 Close 必须两层都关，
// 否则上游连接不会归还连接池。
type decodedBody struct {
	io.Reader
	inner   io.Closer
	decoder io.Closer
}

func (b *decodedBody) Close() error {
	if b.decoder != nil {
		_ = b.decoder.Close()
	}
	return b.inner.Close()
}

// readDecodedAll 读完响应体并按 Content-Encoding 解压。
//
// 成功路径（模型响应流）不经过这里 —— 它需要流式读取；本函数用于把响应体整体读入内存的
// 场合：控制面 JSON 响应，以及模型请求的**错误响应**（其正文要参与 isQuotaExhausted /
// isRateLimited / isAuthFailure 的关键字判定与日志）。
func readDecodedAll(body io.ReadCloser, encoding string) ([]byte, error) {
	decoded, err := wrapUpstreamDecoding(body, encoding)
	if err != nil {
		return nil, err
	}
	defer decoded.Close()
	return io.ReadAll(decoded)
}

// readControlBody 读取控制面响应体并按 Content-Encoding 解压。
//
// 上游对控制面响应通常不压缩（241 字节的 auth/state 实测无 Content-Encoding），
// 但客户端声明了 br，故这里对未知编码显式报错，避免把压缩字节当 JSON 解析。
func readControlBody(resp *http.Response) ([]byte, error) {
	return readDecodedAll(resp.Body, resp.Header.Get("Content-Encoding"))
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	// 控制类请求（令牌刷新、额度查询、模型目录等）必须有明确上限，避免卡死；
	// 上游对话流不经过这里，因此不会误伤长流。
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	return doJSONContext(ctx, client, method, fullURL, headers, body)
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
	// 控制面请求显式声明了 Accept-Encoding（见 controlHeaders），transport 又禁用了自动压缩，
	// 故此处自行解压：gzip / deflate 由标准库覆盖，其余（如 br）明确报错而非静默解析失败。
	raw, err := readControlBody(resp)
	if err != nil {
		return nil, resp.StatusCode, err
	}
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

// logAggregateFailure 记录非流式聚合失败。没有 finish_reason 的干净结束单独说明，
// 避免和网络中断混在同一句「聚合失败」里。
func logAggregateFailure(r *http.Request, reqID uint64, err error) {
	if errors.Is(err, errStreamClosedWithoutFinish) {
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=聚合上游流 结果=拒绝当成成功 原因=上游连接正常结束但没有 finish_reason 返回状态码=500 业务影响=不把残缺输出伪装成完整回复 是否已处理=是",
			debugTraceID(r), reqID)
		return
	}
	log.Printf("[#%d] 聚合响应失败: %v", reqID, err)
}

// noteFinishReason 记下分片里的真实结束原因。
// 空串必须忽略：上游会在每一个中间分片上都带 finish_reason:""，那不是结束。
func noteFinishReason(current string, chunk map[string]any) string {
	if chunk == nil {
		return current
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := choice["finish_reason"].(string); ok && v != "" {
			return v
		}
	}
	return current
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
		observeFirstToken(r, chunk)
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
			finish = noteFinishReason(finish, map[string]any{"choices": []any{choice}})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取上游流失败: %w", err)
	}
	if finish == "" {
		return nil, errStreamClosedWithoutFinish
	}
	completeTokenStream(r, true)

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
			"finish_reason": finish,
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

// accSiteLocked 返回账号所属站点 key；调用方需持有 accountMu。
func accSiteLocked(acc *Account) string {
	if acc == nil {
		return ""
	}
	if acc.Auth != nil {
		return profileForEdition(acc.Auth.Edition).Key
	}
	return profileForEdition(acc.Edition).Key
}
