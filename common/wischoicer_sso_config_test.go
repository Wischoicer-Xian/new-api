package common

import (
	"strings"
	"testing"
)

// RFC v4 §1.4 N3 + §2 N1: WISCHOICER_SSO_PUBLIC_ORIGIN 纯 origin 校验。
func TestValidateWischoicerSsoPublicOrigin(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		wantErr bool
	}{
		{"valid no trailing slash", "https://test.wischoicer.com", false},
		{"valid trailing slash", "https://test.wischoicer.com/", false},
		{"valid with port", "https://test.wischoicer.com:8443", false},
		{"empty", "", true},
		{"http rejected", "http://test.wischoicer.com", true},
		{"path rejected", "https://test.wischoicer.com/sub", true},
		{"query rejected", "https://test.wischoicer.com?x=1", true},
		{"fragment rejected", "https://test.wischoicer.com#frag", true},
		{"userinfo rejected", "https://user:pass@test.wischoicer.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWischoicerSsoPublicOrigin(tc.origin)
			if tc.wantErr && err == nil {
				t.Fatalf("origin %q: want error, got nil", tc.origin)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("origin %q: want no error, got %v", tc.origin, err)
			}
		})
	}
}

// RFC v4 §2 N1: WISCHOICER_SSO_AUTHORIZE_URL HTTPS + 固定 path /bff/gateway/user/sso/authorize。
func TestValidateWischoicerSsoAuthorizeURL(t *testing.T) {
	const ok = "https://test.wischoicer.com/bff/gateway/user/sso/authorize"
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid fixed path", ok, false},
		{"empty", "", true},
		{"http rejected", "http://test.wischoicer.com/bff/gateway/user/sso/authorize", true},
		{"wrong path rejected", "https://test.wischoicer.com/api/user/sso/authorize", true},
		{"trailing slash rejected", "https://test.wischoicer.com/bff/gateway/user/sso/authorize/", true},
		{"query rejected", ok + "?x=1", true},
		{"host only no path rejected", "https://test.wischoicer.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWischoicerSsoAuthorizeURL(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("url %q: want error, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("url %q: want no error, got %v", tc.url, err)
			}
		})
	}
}

// 双 gate 互不推导 + WischoicerSsoEnabled=true 时 origin/authorize-url/SessionCookieSecure 必须 valid（否则 boot reject）。
func TestInitWischoicerSsoConfig_GateTruthTable(t *testing.T) {
	const (
		okOrigin = "https://test.wischoicer.com"
		okAuth   = "https://test.wischoicer.com/bff/gateway/user/sso/authorize"
	)
	reset := func() {
		WischoicerSsoEnabled = false
		LegacySsoPatDisabled = false
		WischoicerSsoPublicOrigin = ""
		WischoicerSsoAuthorizeURL = ""
	}
	secure := func(on bool) func() {
		prev := SessionCookieSecure
		SessionCookieSecure = on
		return func() { SessionCookieSecure = prev }
	}

	t.Run("disabled skips validation", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "false")
		t.Setenv(EnvWischoicerSsoPublicOrigin, "http://invalid") // would fail if enabled
		reset()
		if err := initWischoicerSsoConfig(); err != nil {
			t.Fatalf("disabled: want nil, got %v", err)
		}
		if WischoicerSsoEnabled {
			t.Fatalf("disabled: WischoicerSsoEnabled want false")
		}
	})

	t.Run("enabled with valid origin+authorize+secure", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "true")
		t.Setenv(EnvWischoicerSsoPublicOrigin, okOrigin)
		t.Setenv(EnvWischoicerSsoAuthorizeURL, okAuth)
		t.Cleanup(secure(true))
		reset()
		if err := initWischoicerSsoConfig(); err != nil {
			t.Fatalf("enabled valid: want nil, got %v", err)
		}
		if !WischoicerSsoEnabled {
			t.Fatalf("enabled valid: WischoicerSsoEnabled want true")
		}
	})

	// 记星 P1-2：enabled 但 !SessionCookieSecure → 拒启动（callback refresh cookie 必须 Secure）。
	t.Run("enabled but !SessionCookieSecure boot-rejects", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "true")
		t.Setenv(EnvWischoicerSsoPublicOrigin, okOrigin)
		t.Setenv(EnvWischoicerSsoAuthorizeURL, okAuth)
		t.Cleanup(secure(false))
		reset()
		if err := initWischoicerSsoConfig(); err == nil {
			t.Fatal("enabled !Secure: want error, got nil")
		}
	})

	t.Run("enabled with invalid origin boot-rejects", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "true")
		t.Setenv(EnvWischoicerSsoPublicOrigin, "http://invalid")
		t.Setenv(EnvWischoicerSsoAuthorizeURL, okAuth)
		t.Cleanup(secure(true))
		reset()
		if err := initWischoicerSsoConfig(); err == nil {
			t.Fatal("enabled bad origin: want error, got nil")
		}
	})

	t.Run("enabled with invalid authorize boot-rejects", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "true")
		t.Setenv(EnvWischoicerSsoPublicOrigin, okOrigin)
		t.Setenv(EnvWischoicerSsoAuthorizeURL, "https://x/wrong/path")
		t.Cleanup(secure(true))
		reset()
		if err := initWischoicerSsoConfig(); err == nil {
			t.Fatal("enabled bad authorize: want error, got nil")
		}
	})

	// 记星 P1-2：wischoicerBoolEnv 仅接受 空/false/true，其它非空值 → 报变量名 + 拒启动（防 gate 拼错静默失效）。
	t.Run("bad bool value boot-rejects", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "yes") // not true|false
		t.Cleanup(secure(true))
		reset()
		if err := initWischoicerSsoConfig(); err == nil {
			t.Fatal("bad bool: want error, got nil")
		}
	})

	// 互不推导：LegacySsoPatDisabled 独立于 WischoicerSsoEnabled（关旧不依赖开新）。
	t.Run("legacy gate independent of sso enabled", func(t *testing.T) {
		t.Setenv(EnvWischoicerSsoEnabled, "false")
		t.Setenv(EnvWischoicerLegacySsoPatDisabled, "true")
		reset()
		if err := initWischoicerSsoConfig(); err != nil {
			t.Fatalf("legacy independent: want nil, got %v", err)
		}
		if !LegacySsoPatDisabled {
			t.Fatal("legacy independent: LegacySsoPatDisabled want true")
		}
		if WischoicerSsoEnabled {
			t.Fatal("legacy independent: WischoicerSsoEnabled want false")
		}
	})
}

// callback URL builder：纯 origin + 固定 path + escape code。
func TestWischoicerSsoCallbackURL(t *testing.T) {
	orig := WischoicerSsoPublicOrigin
	WischoicerSsoPublicOrigin = "https://test.wischoicer.com/"
	t.Cleanup(func() { WischoicerSsoPublicOrigin = orig })

	got := WischoicerSsoCallbackURL("ab c&d")
	want := "https://test.wischoicer.com/api/sso/wischoicer/callback?code=ab+c%26d"
	if got != want {
		t.Fatalf("callback URL = %q, want %q", got, want)
	}
	if strings.Contains(got, "//api") {
		t.Fatalf("callback URL double-slash from origin trailing slash: %q", got)
	}
}
