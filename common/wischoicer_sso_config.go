package common

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Wischoicer SSO v2 配置（RFC v4 §1.4 N3 + §2 N1/N5）。InitEnv 一次性读取，不支持热更新。
//
// WischoicerSsoEnabled 与 LegacySsoPatDisabled 是**互不推导**的两个独立 gate（RFC v4 §2 N5）：
// 允许「新流程未开但先关旧」「新旧并存」「新开旧仍开」等各阶段，部署按 pre/dual/cutover/fallback 推进。
var (
	// WischoicerSsoEnabled 标记 SSO v2 入口（GET /api/sso/wischoicer/start 等）是否挂载。
	// env WISCHOICER_SSO_ENABLED 显式开启，且必须同时满足 WISCHOICER_SSO_PUBLIC_ORIGIN
	// 纯 origin 合法 + WISCHOICER_SSO_AUTHORIZE_URL HTTPS＋固定 path 合法——任一不满足而
	// env 又开了 → boot 拒绝启动（fail-closed，不静默降级到半残）。路由注册据此条件挂载。
	WischoicerSsoEnabled = false

	// LegacySsoPatDisabled 控制 legacy POST /api/sso/login（PAT→session）的分阶段 sunset：
	// pre/dual-track 透传（false）→ cutover/fallback 才 410（true）。与 WischoicerSsoEnabled
	// 互不推导——独立 true/false。RFC v4 D1：绝不一开始静态 deny，按 gate 分阶段。
	LegacySsoPatDisabled = false

	// WischoicerSsoPublicOrigin 是 SSO callback 的公网**纯 origin**（https＋host，path 仅空或 /）。
	// 用于 callback_url 拼接（URL builder 追加固定 path，禁拼请求参数，RFC v4 §1.4）。
	WischoicerSsoPublicOrigin = ""

	// WischoicerSsoAuthorizeURL 是 C1→C3 的固定跳转目标（HTTPS + 固定 path）。
	// /start 302 到 ${WischoicerSsoAuthorizeURL}?flow_token=F1（RFC v4 §2 N1）。
	WischoicerSsoAuthorizeURL = ""
)

const (
	EnvWischoicerSsoEnabled           = "WISCHOICER_SSO_ENABLED"
	EnvWischoicerLegacySsoPatDisabled = "WISCHOICER_SSO_LEGACY_PAT_DISABLED"
	EnvWischoicerSsoPublicOrigin      = "WISCHOICER_SSO_PUBLIC_ORIGIN"
	EnvWischoicerSsoAuthorizeURL      = "WISCHOICER_SSO_AUTHORIZE_URL"

	// wischoicerSsoAuthorizeFixedPath 是 C1→C3 跳转目标的固定 path（BFF cookie-only 入口，§1.2）。
	wischoicerSsoAuthorizeFixedPath = "/bff/gateway/user/sso/authorize"
	// wischoicerSsoCallbackFixedPath 是 C2 回调 new-api 的固定 path（§1.4）。
	wischoicerSsoCallbackFixedPath = "/api/sso/wischoicer/callback"
)

// initWischoicerSsoConfig 在 InitEnv 中调用（紧跟 initWischoicerRechargeConfig）。
// WischoicerSsoEnabled=true 但 origin/authorize-url 非法 → 返回 error（main FatalLog 拒启动）。
// WischoicerSsoEnabled=false 时不校验（允许部署先配 origin 再开 gate）；两个 gate 各自独立解析。
func initWischoicerSsoConfig() error {
	WischoicerSsoEnabled = wischoicerBoolEnv(EnvWischoicerSsoEnabled)
	LegacySsoPatDisabled = wischoicerBoolEnv(EnvWischoicerLegacySsoPatDisabled)
	WischoicerSsoPublicOrigin = os.Getenv(EnvWischoicerSsoPublicOrigin)
	WischoicerSsoAuthorizeURL = os.Getenv(EnvWischoicerSsoAuthorizeURL)

	if !WischoicerSsoEnabled {
		return nil // 未启用：不校验，允许先配后开
	}
	if err := ValidateWischoicerSsoPublicOrigin(WischoicerSsoPublicOrigin); err != nil {
		return fmt.Errorf("%s invalid (WischoicerSSO enabled): %w", EnvWischoicerSsoPublicOrigin, err)
	}
	if err := ValidateWischoicerSsoAuthorizeURL(WischoicerSsoAuthorizeURL); err != nil {
		return fmt.Errorf("%s invalid (WischoicerSSO enabled): %w", EnvWischoicerSsoAuthorizeURL, err)
	}
	return nil
}

// wischoicerBoolEnv 解析 "true"/"false"（大小写不敏感、去空白）；空/非 true → false。
func wischoicerBoolEnv(name string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true")
}

// ValidateWischoicerSsoPublicOrigin 校验公网 callback 纯 origin（RFC v4 §1.4 N3）：
// scheme=https、host 非空、path 仅空或 "/"、无 query/fragment/userinfo。任一不满足即 reject。
func ValidateWischoicerSsoPublicOrigin(raw string) error {
	if raw == "" {
		return fmt.Errorf("origin must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse origin: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("path must be empty or '/', got %q", u.Path)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("query not allowed in pure origin: %q", u.RawQuery)
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment not allowed in pure origin: %q", u.Fragment)
	}
	if u.User != nil {
		return fmt.Errorf("userinfo not allowed in pure origin")
	}
	return nil
}

// ValidateWischoicerSsoAuthorizeURL 校验 C1→C3 跳转目标（RFC v4 §2 N1 + §1.2）：
// scheme=https、host 非空、path == 固定 /bff/gateway/user/sso/authorize、无 query/fragment/userinfo。
func ValidateWischoicerSsoAuthorizeURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("authorize URL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse authorize URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if u.Path != wischoicerSsoAuthorizeFixedPath {
		return fmt.Errorf("path must be %q, got %q", wischoicerSsoAuthorizeFixedPath, u.Path)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("query not allowed: %q", u.RawQuery)
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment not allowed: %q", u.Fragment)
	}
	if u.User != nil {
		return fmt.Errorf("userinfo not allowed")
	}
	return nil
}

// WischoicerSsoCallbackURL 用 URL builder 把纯 origin + 固定 callback path + code 拼成绝对
// callback URL（RFC v4 §1.4）：`{TrimRight(origin,"/")}/api/sso/wischoicer/callback?code={escape}`。
// 禁字符串拼接用户输入 / 从请求参数取 origin——origin 已 boot 校验为纯 origin。调用方需确保
// WischoicerSsoEnabled=true（即 origin 已校验）。
func WischoicerSsoCallbackURL(code string) string {
	return strings.TrimRight(WischoicerSsoPublicOrigin, "/") +
		wischoicerSsoCallbackFixedPath + "?code=" + url.QueryEscape(code)
}
