package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

func setSaaSTopupExcludedUsersForTest(t *testing.T, userIDs ...int) {
	t.Helper()
	original := constant.SaaSTopupExcludedUserIDs
	constant.SaaSTopupExcludedUserIDs = make(map[int]struct{}, len(userIDs))
	for _, userID := range userIDs {
		constant.SaaSTopupExcludedUserIDs[userID] = struct{}{}
	}
	t.Cleanup(func() {
		constant.SaaSTopupExcludedUserIDs = original
	})
}

func TestResolveSaaSTopupTokenHidesExcludedOwner(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 9, common.RoleCommonUser, "default", "agent-access-token")
	seedInternalLookupToken(t, db, 9, "agent-key", "agent-key-123", "default", false)
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/resolve", map[string]any{
		"key": "sk-agent-key-123",
	}, 1)
	ResolveSaaSTopupToken(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for excluded owner, got %d", recorder.Code)
	}
	response := decodeAPIResponse(t, recorder)
	if response.Success || response.Message != "token not found" {
		t.Fatalf("expected generic token-not-found response, got %+v", response)
	}
}

func TestResolveSaaSTopupTokenReturnsAllowedOwnerAndQuota(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 10, common.RoleCommonUser, "vip", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "customer-key-123", 700, 300, "", -1, false)
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/resolve", map[string]any{
		"key": "sk-customer-key-123",
	}, 1)
	ResolveSaaSTopupToken(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected allowed token to resolve: %+v", response)
	}
	var resolved ResolveSaaSTopupTokenResponse
	if err := common.Unmarshal(response.Data, &resolved); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resolved.TokenID != token.Id || resolved.UserID != 10 {
		t.Fatalf("unexpected resolved identity: %+v", resolved)
	}
	if resolved.Group != "vip" || resolved.RemainQuota != 700 || resolved.UsedQuota != 300 || resolved.TotalGranted != 1000 {
		t.Fatalf("unexpected resolved quota metadata: %+v", resolved)
	}
}

func TestResolveSaaSTopupTokenHidesDisabledToken(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "disabled-key-123", 700, 300, "default", -1, false)
	if err := db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/resolve", map[string]any{
		"key": "sk-disabled-key-123",
	}, 1)
	ResolveSaaSTopupToken(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token, got %d", recorder.Code)
	}
}

func TestResolveSaaSTopupTokenHidesDisabledOwner(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	user := seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	seedGrantQuotaToken(t, db, 10, "disabled-owner-key-123", 700, 300, "default", -1, false)
	if err := db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token owner: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/resolve", map[string]any{
		"key": "sk-disabled-owner-key-123",
	}, 1)
	ResolveSaaSTopupToken(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token owner, got %d", recorder.Code)
	}
}

func TestGrantSaaSTopupTokenQuotaRejectsExcludedOwnerWithoutMutation(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 9, common.RoleCommonUser, "default", "agent-access-token")
	token := seedGrantQuotaToken(t, db, 9, "agent-grant-key", 100, 50, "default", -1, false)
	setSaaSTopupExcludedUsersForTest(t, 9)

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  9,
		"amount":  1000,
		"note":    "Topup order must be rejected",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/grant-quota", body, 1)
	GrantSaaSTopupTokenQuota(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for excluded owner, got %d", recorder.Code)
	}
	updated, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updated.RemainQuota != 100 {
		t.Fatalf("excluded token quota changed: got %d", updated.RemainQuota)
	}
}

func TestValidateSaaSTopupTokenTargetRejectsExcludedOwner(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 9, common.RoleCommonUser, "default", "agent-access-token")
	token := seedGrantQuotaToken(t, db, 9, "agent-validate-key", 100, 50, "default", -1, false)
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/validate", map[string]any{
		"tokenId": token.Id,
		"userId":  9,
	}, 1)
	ValidateSaaSTopupTokenTarget(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for excluded owner, got %d", recorder.Code)
	}
}

func TestValidateSaaSTopupTokenTargetRejectsDisabledOwner(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	user := seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "disabled-owner-validate-key", 100, 50, "default", -1, false)
	if err := db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token owner: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/validate", map[string]any{
		"tokenId": token.Id,
		"userId":  10,
	}, 1)
	ValidateSaaSTopupTokenTarget(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token owner, got %d", recorder.Code)
	}
}

func TestValidateSaaSTopupTokenTargetRejectsDisabledToken(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "disabled-token-validate-key", 100, 50, "default", -1, false)
	if err := db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/validate", map[string]any{
		"tokenId": token.Id,
		"userId":  10,
	}, 1)
	ValidateSaaSTopupTokenTarget(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token, got %d", recorder.Code)
	}
}

func TestGrantSaaSTopupTokenQuotaRejectsDisabledTokenWithoutMutation(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "disabled-grant-key", 100, 50, "default", -1, false)
	if err := db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  10,
		"amount":  1000,
		"note":    "Disabled token must be rejected",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/grant-quota", body, 1)
	GrantSaaSTopupTokenQuota(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token, got %d", recorder.Code)
	}
	updated, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updated.RemainQuota != 100 {
		t.Fatalf("disabled token quota changed: got %d", updated.RemainQuota)
	}
}

func TestGrantSaaSTopupTokenQuotaRejectsDisabledOwnerWithoutMutation(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	user := seedInternalUser(t, db, 10, common.RoleCommonUser, "default", "customer-access-token")
	token := seedGrantQuotaToken(t, db, 10, "disabled-owner-grant-key", 100, 50, "default", -1, false)
	if err := db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error; err != nil {
		t.Fatalf("failed to disable token owner: %v", err)
	}
	setSaaSTopupExcludedUsersForTest(t, 9)

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  10,
		"amount":  1000,
		"note":    "Disabled token owner must be rejected",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/saas-topup/grant-quota", body, 1)
	GrantSaaSTopupTokenQuota(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled token owner, got %d", recorder.Code)
	}
	updated, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updated.RemainQuota != 100 {
		t.Fatalf("disabled owner's token quota changed: got %d", updated.RemainQuota)
	}
}

func TestGenericAdminGrantRemainsAvailableForExcludedOwner(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 9, common.RoleCommonUser, "default", "agent-access-token")
	token := seedGrantQuotaToken(t, db, 9, "manual-grant-key", 100, 50, "default", -1, false)
	setSaaSTopupExcludedUsersForTest(t, 9)

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  9,
		"amount":  25,
		"note":    "Manual admin adjustment",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected generic admin grant to remain available: %+v", response)
	}
	updated, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updated.RemainQuota != 125 {
		t.Fatalf("expected manual grant to update quota, got %d", updated.RemainQuota)
	}
}
