package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupGiftController(t *testing.T) *gin.Engine {
	t.Helper()
	router, _ := setupSaaSCreditController(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SaaSGiftAccount{}, &model.SaaSGiftOperation{}, &model.Ability{}))
	t.Setenv("SAAS_GIFT_GROUP", "双十一")
	oldUsable := setting.UserUsableGroups2JSONString()
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
	t.Cleanup(func() { require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(oldUsable)) })
	old := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"双十一":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(old)) })
	require.NoError(t, model.DB.Create(&model.Ability{Group: "双十一", Model: model.SaaSGiftModel, ChannelId: 1, Enabled: true}).Error)
	group := router.Group("/api/token/admin/saas-gifts", middleware.AdminAuth(), middleware.DisableCache())
	group.POST("/accounts/ensure", EnsureSaaSGiftAccount)
	group.POST("/operations", CreateSaaSGiftOperation)
	group.GET("/operations/:operationId", GetSaaSGiftOperation)
	return router
}

func TestSaaSGiftHTTPContractAndRestriction(t *testing.T) {
	router := setupGiftController(t)
	ensure := map[string]any{"operationId": "ensure:http", "campaignId": model.SaaSGiftCampaign, "beneficiaryRef": "http-wallet", "policyVersion": model.SaaSGiftPolicyVersion}
	const base = "/api/token/admin/saas-gifts"
	unauthorized := callSaaSCreditController(t, router, http.MethodPost, base+"/accounts/ensure", ensure, false)
	assert.False(t, decodeAPIResponse(t, unauthorized).Success)
	rr := callSaaSCreditController(t, router, http.MethodPost, base+"/accounts/ensure", ensure, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var account model.SaaSGiftAccount
	require.NoError(t, common.Unmarshal(decodeAPIResponse(t, rr).Data, &account))
	require.NotNil(t, account.TokenID)
	token, err := model.GetTokenById(*account.TokenID)
	require.NoError(t, err)
	assert.NotContains(t, rr.Body.String(), token.Key)
	assert.NotContains(t, rr.Body.String(), "http-wallet")
	credit := map[string]any{"operationId": "grant:http", "accountId": account.ID, "kind": "credit", "amount": 50}
	for i := 0; i < 2; i++ {
		rr = callSaaSCreditController(t, router, http.MethodPost, base+"/operations", credit, true)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var op model.SaaSGiftOperation
		require.NoError(t, common.Unmarshal(decodeAPIResponse(t, rr).Data, &op))
		assert.Equal(t, "APPLIED", op.State)
		require.NotNil(t, op.Receipt)
		assert.Equal(t, 50, op.Receipt.Amount)
		assert.Equal(t, i == 1, op.Duplicated)
		assert.NotContains(t, rr.Body.String(), token.Key)
	}
	rr = callSaaSCreditController(t, router, http.MethodGet, base+"/operations/grant:http", nil, true)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, decodeAPIResponse(t, rr).Success)
	credit["amount"] = -1
	rr = callSaaSCreditController(t, router, http.MethodPost, base+"/operations", credit, true)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	credit["amount"] = 100
	rr = callSaaSCreditController(t, router, http.MethodPost, base+"/operations", credit, true)
	assert.Equal(t, http.StatusConflict, rr.Code)
	eligible, err := isSaaSTopupTokenEligible(token)
	require.NoError(t, err)
	assert.False(t, eligible)
	router.POST("/manage-gift-check", middleware.AdminAuth(), ManageUser)
	for _, mode := range []string{"add", "subtract", "override"} {
		rr = callSaaSCreditController(t, router, http.MethodPost, "/manage-gift-check", map[string]any{"id": *account.UserID, "action": "add_quota", "mode": mode, "value": 100}, true)
		assert.False(t, decodeAPIResponse(t, rr).Success)
	}
	user, err := model.GetUserById(*account.UserID, true)
	require.NoError(t, err)
	assert.Equal(t, 50, user.Quota)
	setup := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"model": c.GetString("saas_gift_model")}) }
	router.POST("/auth-check", middleware.TokenAuth(), setup)
	req := httptest.NewRequest(http.MethodPost, "/auth-check", nil)
	req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), model.SaaSGiftModel)
	for _, key := range []string{token.Key + "-1"} {
		req = httptest.NewRequest(http.MethodPost, "/auth-check", nil)
		req.Header.Set("Authorization", "Bearer sk-"+key)
		rr = httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	}
	router.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), setup)
	router.POST("/pg/chat/completions", middleware.TokenAuth(), middleware.Distribute(), setup)
	for _, test := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"gpt-other"}`},
		{"/pg/chat/completions", `{"model":"claude-sonnet-4-6","group":"default"}`},
	} {
		req = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-"+token.Key)
		rr = httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	}
	_, err = model.ApplySaaSGiftOperation(context.Background(), model.SaaSGiftOperationRequest{OperationID: "freeze:http", AccountID: account.ID, Kind: "freeze"})
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/auth-check", nil)
	req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestSaaSGiftHTTPFreezeBeforeProvisionAndCancel(t *testing.T) {
	router := setupGiftController(t)
	const base = "/api/token/admin/saas-gifts"
	rr := callSaaSCreditController(t, router, http.MethodPost, base+"/operations", map[string]any{"operationId": "freeze:before", "kind": "freeze", "campaignId": model.SaaSGiftCampaign, "beneficiaryRef": "before"}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = callSaaSCreditController(t, router, http.MethodPost, base+"/accounts/ensure", map[string]any{"operationId": "ensure:before", "campaignId": model.SaaSGiftCampaign, "beneficiaryRef": "before", "policyVersion": model.SaaSGiftPolicyVersion}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var data map[string]any
	require.NoError(t, common.Unmarshal(decodeAPIResponse(t, rr).Data, &data))
	assert.Equal(t, model.SaaSGiftFrozen, data["state"])
	assert.Nil(t, data["userId"])
	assert.Nil(t, data["tokenId"])
	rr = callSaaSCreditController(t, router, http.MethodPost, base+"/operations", map[string]any{"operationId": "cancel:before", "kind": "cancel_credit", "targetOperationId": "grant:before", "campaignId": model.SaaSGiftCampaign, "beneficiaryRef": "before"}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "CANCELED")
	rr = callSaaSCreditController(t, router, http.MethodGet, base+"/operations/grant:before", nil, true)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "CANCELED")
}
