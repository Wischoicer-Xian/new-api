package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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
	assert.Equal(t, int64(1), sessionCount(t, db, user.Id), "one session created")
}

// 记星 edec1c4b 契约①：错 bsid 也提交消费 F2（burn），统一失败（/login），不建 session。
func TestSsoCallback_WrongBsid_BurnsF2AndFailsUnified(t *testing.T) {
	db, user := setupWischoicerSsoCallbackTestDB(t)
	f2Tok := makeWischoicerSsoFlow(t, user.Id, "correct-bsid")

	w := callCallback(t, f2Tok, "wrong-bsid")
	assert.Equal(t, http.StatusFound, w.Code, "unified failure redirects (302)")
	assert.Equal(t, "/login", w.Header().Get("Location"), "unified failure → /login")
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
