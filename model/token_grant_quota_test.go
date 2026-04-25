package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
)

func resetBatchUpdateStores() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
}

func seedGrantQuotaModelToken(t *testing.T, remainQuota int, usedQuota int) *Token {
	t.Helper()

	token := &Token{
		UserId:         1,
		Name:           "grant-model-token",
		Key:            fmt.Sprintf("grant-model-key-%s", t.Name()),
		Status:         common.TokenStatusEnabled,
		CreatedTime:    1,
		AccessedTime:   1,
		ExpiredTime:    -1,
		RemainQuota:    remainQuota,
		UsedQuota:      usedQuota,
		UnlimitedQuota: false,
		Group:          "default",
	}
	if err := DB.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

func seedGrantQuotaModelTokenWithStatus(t *testing.T, remainQuota int, usedQuota int, status int) *Token {
	t.Helper()

	token := seedGrantQuotaModelToken(t, remainQuota, usedQuota)
	token.Status = status
	if err := DB.Model(&Token{}).Where("id = ?", token.Id).Update("status", status).Error; err != nil {
		t.Fatalf("failed to update token status: %v", err)
	}
	return token
}

func TestGrantTokenRemainQuotaUpdatesRemainOnly(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores()

	originalBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() {
		common.BatchUpdateEnabled = originalBatchUpdateEnabled
		resetBatchUpdateStores()
	})

	token := seedGrantQuotaModelToken(t, 100, 40)
	beforeTotalGranted := token.RemainQuota + token.UsedQuota
	beforeAccessedTime := token.AccessedTime

	if err := GrantTokenRemainQuota(token.Id, token.Key, 25); err != nil {
		t.Fatalf("failed to grant token remain quota: %v", err)
	}

	updatedToken, err := GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updatedToken.RemainQuota != 125 {
		t.Fatalf("expected remain quota 125, got %d", updatedToken.RemainQuota)
	}
	if updatedToken.UsedQuota != 40 {
		t.Fatalf("expected used quota 40, got %d", updatedToken.UsedQuota)
	}
	if updatedToken.RemainQuota+updatedToken.UsedQuota != beforeTotalGranted+25 {
		t.Fatalf("expected total granted quota %d, got %d", beforeTotalGranted+25, updatedToken.RemainQuota+updatedToken.UsedQuota)
	}
	if updatedToken.AccessedTime < beforeAccessedTime {
		t.Fatalf("expected accessed time to be updated, before=%d after=%d", beforeAccessedTime, updatedToken.AccessedTime)
	}
}

func TestGrantTokenRemainQuotaBatchUpdatesRemainOnly(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores()

	originalBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() {
		common.BatchUpdateEnabled = originalBatchUpdateEnabled
		resetBatchUpdateStores()
	})

	token := seedGrantQuotaModelToken(t, 200, 80)
	beforeTotalGranted := token.RemainQuota + token.UsedQuota

	if err := GrantTokenRemainQuota(token.Id, token.Key, 30); err != nil {
		t.Fatalf("failed to grant token remain quota with batch update enabled: %v", err)
	}

	updatedToken, err := GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token with batch update enabled: %v", err)
	}
	if updatedToken.RemainQuota != 230 {
		t.Fatalf("expected remain quota 230, got %d", updatedToken.RemainQuota)
	}
	if updatedToken.UsedQuota != 80 {
		t.Fatalf("expected used quota 80, got %d", updatedToken.UsedQuota)
	}
	if updatedToken.RemainQuota+updatedToken.UsedQuota != beforeTotalGranted+30 {
		t.Fatalf("expected total granted quota %d, got %d", beforeTotalGranted+30, updatedToken.RemainQuota+updatedToken.UsedQuota)
	}
}

func TestGrantTokenRemainQuotaRestoresExhaustedStatus(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores()

	originalBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() {
		common.BatchUpdateEnabled = originalBatchUpdateEnabled
		resetBatchUpdateStores()
	})

	token := seedGrantQuotaModelTokenWithStatus(t, 0, 40, common.TokenStatusExhausted)

	if err := GrantTokenRemainQuota(token.Id, token.Key, 25); err != nil {
		t.Fatalf("failed to grant token remain quota: %v", err)
	}

	updatedToken, err := GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updatedToken.RemainQuota != 25 {
		t.Fatalf("expected remain quota 25, got %d", updatedToken.RemainQuota)
	}
	if updatedToken.Status != common.TokenStatusEnabled {
		t.Fatalf("expected token status %d, got %d", common.TokenStatusEnabled, updatedToken.Status)
	}
}
