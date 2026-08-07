package controller

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type accessTokenAPIResponse struct {
	Success bool   `json:"success"`
	Data    string `json:"data"`
}

func setupAccessTokenControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	model.DB = db
	model.LOG_DB = db
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedis
	})
	return db
}

func callAccessTokenHandler(t *testing.T, handler gin.HandlerFunc, userID int, target string) accessTokenAPIResponse {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, target, nil)
	context.Set("id", userID)
	handler(context)

	var response accessTokenAPIResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}

func TestGenerateAccessTokenPersistsToken(t *testing.T) {
	db := setupAccessTokenControllerTestDB(t)
	user := &model.User{Username: "self-token-user", Password: "password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(user).Error)

	response := callAccessTokenHandler(t, GenerateAccessToken, user.Id, "/api/user/token")
	require.True(t, response.Success)
	require.NotEmpty(t, response.Data)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	require.Equal(t, response.Data, stored.GetAccessToken())
	validated, err := model.ValidateAccessToken(response.Data)
	require.NoError(t, err)
	require.Equal(t, user.Id, validated.Id)
}

func TestAdminGenerateAccessTokenPersistsTargetToken(t *testing.T) {
	db := setupAccessTokenControllerTestDB(t)
	user := &model.User{Username: "admin-token-user", Password: "password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(user).Error)

	response := callAccessTokenHandler(t, func(context *gin.Context) {
		context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(user.Id)}}
		AdminGenerateAccessToken(context)
	}, user.Id, "/api/user/"+strconv.Itoa(user.Id)+"/generate_access_token")
	require.True(t, response.Success)
	require.NotEmpty(t, response.Data)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	require.Equal(t, response.Data, stored.GetAccessToken())
	validated, err := model.ValidateAccessToken(response.Data)
	require.NoError(t, err)
	require.Equal(t, user.Id, validated.Id)
}

func TestGenerateAccessTokenPreservesConcurrentUserFields(t *testing.T) {
	db := setupAccessTokenControllerTestDB(t)
	user := &model.User{Username: "concurrent-token-user", Password: "password", DisplayName: "before", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(user).Error)

	staleUser, err := model.GetUserById(user.Id, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("display_name", "concurrent-update").Error)

	key, err := generateAndSetAccessToken(staleUser)
	require.NoError(t, err)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	require.Equal(t, key, stored.GetAccessToken())
	require.Equal(t, "concurrent-update", stored.DisplayName)
}
