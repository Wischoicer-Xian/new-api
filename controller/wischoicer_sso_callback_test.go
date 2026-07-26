package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupWischoicerSsoCallbackTestDB 立 in-memory SQLite（auth_flows + users + user_sessions）+ user(id=69)，
// 配 SessionSecret + SsoPublicOrigin（callback_url 用），cleanup 恢复。
func setupWischoicerSsoCallbackTestDB(t *testing.T) (*gorm.DB, *model.User) {
	t.Helper()
	prevDB := model.DB
	prevRedis := common.RedisEnabled
	prevSecret := common.SessionSecret
	prevOrigin := common.WischoicerSsoPublicOrigin
	// 文件 sqlite（t.TempDir 唯一、自动清理）：所有连接共享同一文件 DB——避免 :memory: 每连接独立 DB
	// 导致 CreateLoginSession 多连接查询撞空 DB（"no such table"）；也避免 SetMaxOpenConns(1) 与嵌套 tx 死锁。
	dbFile := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbFile), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}, &model.User{}, &model.UserSession{}))
	db.Exec("PRAGMA busy_timeout = 5000") // P2-2: 并发测试——让第二个写事务等待而非立即报 "database is locked"
	model.DB = db
	common.RedisEnabled = false
	common.SessionSecret = "wischoicer-sso-callback-test-secret"
	common.WischoicerSsoPublicOrigin = "https://test.wischoicer.com"
	user := &model.User{Username: "sso-cb-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1}
	require.NoError(t, db.Create(user).Error)
	t.Cleanup(func() {
		model.DB = prevDB
		common.RedisEnabled = prevRedis
		common.SessionSecret = prevSecret
		common.WischoicerSsoPublicOrigin = prevOrigin
	})
	return db, user
}

func makeWischoicerSsoFlow(t *testing.T, userID int, bsid string) string {
	t.Helper()
	tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSOCode, // F2（记星 9c4b5fee P1）
		Intent:    model.AuthFlowIntentLogin,
		UserId:    userID,
		Payload:   wischoicerSsoBsidHash(bsid),
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)
	return tok
}

func callCallback(t *testing.T, code, cookieBsid string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.GET("/callback", SsoCallback)
	req := httptest.NewRequest(http.MethodGet, "/callback?code="+code, nil)
	if cookieBsid != "" && cookieBsid != "__none__" {
		req.AddCookie(&http.Cookie{Name: wischoicerSsoStateCookie, Value: cookieBsid})
	}
	r.ServeHTTP(w, req)
	return w
}

func sessionCount(t *testing.T, db *gorm.DB, userID int) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&model.UserSession{}).Where("user_id = ?", userID).Count(&n).Error)
	return n
}

// 记星 edec1c4b C2 契约：消费 F1 + 同事务 mint F2，F1.payload 原样复制进 F2.payload。
func TestSsoAuthorize_MintsF2WithF1PayloadVerbatim(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	bsid := "c2-test-bsid"
	bsidHash := wischoicerSsoBsidHash(bsid)
	f1Tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose: model.AuthFlowPurposeWischoicerSSOStart, Intent: model.AuthFlowIntentLogin, // F1（记星 9c4b5fee P1）
		Payload: bsidHash, ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.POST("/authorize", SsoAuthorize)
	body := `{"flow_token":"` + f1Tok + `","new_api_user_id":` + strconv.Itoa(user.Id) + `}`
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "C2 body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["callback_url"].(string), "/api/sso/wischoicer/callback?code=")

	var flows []model.AuthFlow
	require.NoError(t, db.Where("purpose IN ?", []string{model.AuthFlowPurposeWischoicerSSOStart, model.AuthFlowPurposeWischoicerSSOCode}).Find(&flows).Error)
	require.Len(t, flows, 2, "F1 + F2 should both exist")
	var f2 *model.AuthFlow
	for i := range flows {
		if flows[i].UserId == user.Id {
			f2 = &flows[i]
		}
	}
	require.NotNil(t, f2, "F2 (UserId=%d) must exist", user.Id)
	assert.Equal(t, bsidHash, f2.Payload, "F2.payload must equal F1.payload VERBATIM (记星 edec1c4b)")
	assert.Nil(t, f2.ConsumedAt, "F2 not consumed by C2 (callback consumes it)")
}

// happy path：正确 bsid → 建 session + refresh cookie + 302 /dashboard。
func TestSsoCallback_HappyPath_CreatesSession(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	bsid := "happy-bsid"
	f2Tok := makeWischoicerSsoFlow(t, user.Id, bsid)

	w := callCallback(t, f2Tok, bsid)
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "/dashboard", w.Header().Get("Location"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"), "P2-2: referrer-policy on success")
	assert.Equal(t, int64(1), sessionCount(t, db, user.Id), "one session created")
}

// 记星 edec1c4b 契约①：错 bsid 也提交消费 F2（burn），统一失败（/login），不建 session。
func TestSsoCallback_WrongBsid_BurnsF2AndFailsUnified(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	f2Tok := makeWischoicerSsoFlow(t, user.Id, "correct-bsid")

	w := callCallback(t, f2Tok, "wrong-bsid")
	assert.Equal(t, http.StatusFound, w.Code, "unified failure redirects (302)")
	assert.Equal(t, "/login", w.Header().Get("Location"), "unified failure → /login")
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"), "P2-2: referrer-policy on failure too")
	// F2 burned despite wrong bsid
	var f2 model.AuthFlow
	require.NoError(t, db.Where("purpose = ? AND user_id = ?", model.AuthFlowPurposeWischoicerSSOCode, user.Id).First(&f2).Error)
	assert.NotNil(t, f2.ConsumedAt, "F2 MUST be burned even on wrong bsid (记星: 失败也提交)")
	assert.Equal(t, int64(0), sessionCount(t, db, user.Id), "no session on wrong bsid")
}

// 记星 edec1c4b 契约②：cookie 缺失 → 先提交消费 F2 + 统一失败，不建 session。
func TestSsoCallback_MissingCookie_BurnsF2AndFailsUnified(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	f2Tok := makeWischoicerSsoFlow(t, user.Id, "any-bsid")

	w := callCallback(t, f2Tok, "__none__") // no cookie
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
	var f2 model.AuthFlow
	require.NoError(t, db.Where("purpose = ? AND user_id = ?", model.AuthFlowPurposeWischoicerSSOCode, user.Id).First(&f2).Error)
	assert.NotNil(t, f2.ConsumedAt, "F2 MUST be burned even when cookie missing")
	assert.Equal(t, int64(0), sessionCount(t, db, user.Id), "no session when cookie missing")
}

// F2 不可重放：第二次 callback（同 code）→ F2 已消费 → 统一失败。
func TestSsoCallback_F2NotReplayable(t *testing.T) {
	_, user := setupWischoicerSsoCallbackTestDB(t)
	bsid := "replay-bsid"
	f2Tok := makeWischoicerSsoFlow(t, user.Id, bsid)

	w1 := callCallback(t, f2Tok, bsid)
	assert.Equal(t, "/dashboard", w1.Header().Get("Location"))
	w2 := callCallback(t, f2Tok, bsid) // replay
	assert.Equal(t, "/login", w2.Header().Get("Location"), "replayed F2 → unified failure (not replayable)")
}

// 记星 9c4b5fee P1: callback 只消费 F2(Code)，收到 F1(Start) 拒绝且不消费。
func TestSsoCallback_RejectsF1_DoesNotConsume(t *testing.T) {
	db, _ := setupWischoicerSsoCallbackTestDB(t)
	f1Tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSOStart,
		Intent:    model.AuthFlowIntentLogin,
		Payload:   wischoicerSsoBsidHash("x"),
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)
	w := callCallback(t, f1Tok, "x")
	assert.Equal(t, "/login", w.Header().Get("Location"), "callback(F1) → unified failure (purpose mismatch)")
	var f1 model.AuthFlow
	require.NoError(t, db.Where("purpose = ?", model.AuthFlowPurposeWischoicerSSOStart).First(&f1).Error)
	assert.Nil(t, f1.ConsumedAt, "F1 must NOT be consumed by callback (purpose mismatch)")
}

// 记星 9c4b5fee P1: C2 只消费 F1(Start)，收到 F2(Code) 拒绝且不消费。
func TestSsoAuthorize_RejectsF2_DoesNotConsume(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	f2Tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSOCode,
		Intent:    model.AuthFlowIntentLogin,
		UserId:    user.Id,
		Payload:   wischoicerSsoBsidHash("x"),
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.POST("/authorize", SsoAuthorize)
	body := `{"flow_token":"` + f2Tok + `","new_api_user_id":` + strconv.Itoa(user.Id) + `}`
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, "C2(F2) → 400 (purpose mismatch)")
	var f2 model.AuthFlow
	require.NoError(t, db.Where("purpose = ?", model.AuthFlowPurposeWischoicerSSOCode).First(&f2).Error)
	assert.Nil(t, f2.ConsumedAt, "F2 must NOT be consumed by C2 (purpose mismatch)")
}

// ---- 记星 9c4b5fee P2-2: 并发回归 ----

// ---- 记星 de34eafd P2-1: 并发回归（barrier + model 层证明）----

// model 层：两 goroutine 并发消费同一 F2 → 恰一次成功、一次非成功（SQLite busy 或
// consumed——两者都 fail-closed、不签 session）。安全契约是「至多一次副作用 + loser
// fail-closed」，不是「loser 必为 consumed」（SQLite 单写锁下 loser 可能拿 SQLITE_BUSY
// 而非 ErrAuthFlowConsumed——这是测试 artifact；生产 MySQL/PostgreSQL 行锁下 loser 天然
// = consumed，exact 契约挂 integration test）。
func TestConsumeAuthFlowWithAction_ConcurrentF2_ExactlyOneSucceeds(t *testing.T) {
	_, _ = setupWischoicerSsoCallbackTestDB(t)
	f2Tok := makeWischoicerSsoFlow(t, 0, "model-concurrent-bsid")

	allReady := sync.WaitGroup{}
	allReady.Add(2)
	start := make(chan struct{})
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		successes    int
		nonSuccesses int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allReady.Done() // signal ready
			<-start         // barrier: wait for simultaneous release
			_, err := model.ConsumeAuthFlowWithAction(f2Tok, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeWischoicerSSOCode}, func(tx *gorm.DB, f2 *model.AuthFlow) error {
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				nonSuccesses++ // busy OR consumed — both fail-closed
			}
		}()
	}
	allReady.Wait() // both parked on <-start
	close(start)    // simultaneous release
	wg.Wait()

	assert.Equal(t, 1, successes, "exactly one consume succeeds (at-most-one side effect)")
	assert.Equal(t, 1, nonSuccesses, "exactly one non-success (loser fail-closed: busy or consumed)")
}

// HTTP 层：一 F2 并发双 callback → 恰一 /dashboard + 一非 /dashboard + 库内恰一 session。
// 带 ready/start barrier（防串行化掩盖真并发语义，记星 de34eafd P2-1）。
func TestSsoCallback_ConcurrentDoubleCallback_ExactlyOneSession(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	bsid := "concurrent-bsid"
	f2Tok := makeWischoicerSsoFlow(t, user.Id, bsid)

	gin.SetMode(gin.TestMode)
	allReady := sync.WaitGroup{}
	allReady.Add(2)
	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []string
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allReady.Done() // signal ready
			<-start         // barrier: wait for simultaneous release
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.GET("/callback", SsoCallback)
			req := httptest.NewRequest(http.MethodGet, "/callback?code="+f2Tok, nil)
			req.AddCookie(&http.Cookie{Name: wischoicerSsoStateCookie, Value: bsid})
			r.ServeHTTP(w, req)
			mu.Lock()
			results = append(results, w.Header().Get("Location"))
			mu.Unlock()
		}()
	}
	allReady.Wait() // both parked on <-start
	close(start)    // simultaneous release
	wg.Wait()

	dashboards, others := 0, 0
	for _, loc := range results {
		if loc == "/dashboard" {
			dashboards++
		} else {
			others++
		}
	}
	assert.Equal(t, 1, dashboards, "exactly one /dashboard (one session)")
	assert.Equal(t, 1, others, "exactly one non-/dashboard (F2 consumed by the other)")
	assert.Equal(t, int64(1), sessionCount(t, db, user.Id), "exactly one session in DB")
}

// CreateLoginSession 失败 → F2 仍 consumed、无 refresh cookie。
func TestSsoCallback_CreateLoginSessionFails_F2ConsumedNoCookie(t *testing.T) {
	db, _ := setupWischoicerSsoCallbackTestDB(t)
	bsid := "fail-bsid"
	f2Tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSOCode,
		Intent:    model.AuthFlowIntentLogin,
		UserId:    999999, // 不存在的 user → CreateLoginSession 失败
		Payload:   wischoicerSsoBsidHash(bsid),
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)
	w := callCallback(t, f2Tok, bsid)
	assert.Equal(t, "/login", w.Header().Get("Location"), "CreateLoginSession failed → unified failure")
	var f2 model.AuthFlow
	require.NoError(t, db.Where("purpose = ? AND user_id = ?", model.AuthFlowPurposeWischoicerSSOCode, 999999).First(&f2).Error)
	assert.NotNil(t, f2.ConsumedAt, "F2 MUST be consumed even when CreateLoginSession fails")
	for _, ck := range w.Result().Cookies() {
		assert.NotContains(t, ck.Name, "refresh", "no refresh cookie on CreateLoginSession failure")
	}
}

// clear cookie 的 Path/Secure/HttpOnly/SameSite/MaxAge 逐项对称（与 /start set 对称）。
func TestSsoCallback_ClearCookieSymmetric(t *testing.T) {
	_, user := setupWischoicerSsoCallbackTestDB(t)
	f2Tok := makeWischoicerSsoFlow(t, user.Id, "clear-test-bsid")
	w := callCallback(t, f2Tok, "clear-test-bsid")
	var cleared *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == wischoicerSsoStateCookie {
			cleared = ck
		}
	}
	require.NotNil(t, cleared, "state cookie clear must be in response")
	assert.Equal(t, wischoicerSsoStateCookiePath, cleared.Path, "clear Path == set Path")
	assert.True(t, cleared.Secure, "clear Secure == set Secure")
	assert.True(t, cleared.HttpOnly, "clear HttpOnly == set HttpOnly")
	assert.Equal(t, http.SameSiteLaxMode, cleared.SameSite, "clear SameSite == set SameSite")
	assert.True(t, cleared.MaxAge < 0, "clear MaxAge < 0 (delete)")
}

// 记星 9c4b5fee / de34eafd P2-3: C2 重放同一 F1 → 首次 200、二次 400、恰一 F2、F1 已消费。
func TestSsoAuthorize_ReplayF1_SecondRejectsOnlyOneF2(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	f1Tok, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSOStart,
		Intent:    model.AuthFlowIntentLogin,
		Payload:   wischoicerSsoBsidHash("replay-f1-bsid"),
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	body := `{"flow_token":"` + f1Tok + `","new_api_user_id":` + strconv.Itoa(user.Id) + `}`

	// First C2 → 200
	w1 := httptest.NewRecorder()
	_, r1 := gin.CreateTestContext(w1)
	r1.POST("/authorize", SsoAuthorize)
	req1 := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	r1.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code, "first C2(F1) → 200")

	// Replay F1 → 400 (F1 already consumed)
	w2 := httptest.NewRecorder()
	_, r2 := gin.CreateTestContext(w2)
	r2.POST("/authorize", SsoAuthorize)
	req2 := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusBadRequest, w2.Code, "replay F1 → 400")

	// Exactly one F2 (Code)
	var f2Count int64
	require.NoError(t, db.Model(&model.AuthFlow{}).Where("purpose = ?", model.AuthFlowPurposeWischoicerSSOCode).Count(&f2Count).Error)
	assert.Equal(t, int64(1), f2Count, "exactly one F2 (replay produces no extra)")

	// F1 consumed
	var f1 model.AuthFlow
	require.NoError(t, db.Where("purpose = ?", model.AuthFlowPurposeWischoicerSSOStart).First(&f1).Error)
	assert.NotNil(t, f1.ConsumedAt, "F1 consumed after first C2")
}
