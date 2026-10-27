package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupSaaSGiftTest(t *testing.T) {
	t.Helper()
	setupSaaSCreditOperationTest(t)
	require.NoError(t, DB.AutoMigrate(&SaaSGiftAccount{}, &SaaSGiftOperation{}, &Ability{}))
	t.Setenv("SAAS_GIFT_GROUP", "双十一")
	old := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"双十一":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(old)) })
	require.NoError(t, DB.Create(&Ability{Group: "双十一", Model: SaaSGiftModel, ChannelId: 1, Enabled: true}).Error)
}

func giftEnsure(ref string) SaaSGiftEnsureRequest {
	return SaaSGiftEnsureRequest{OperationID: "ensure:" + ref, CampaignID: SaaSGiftCampaign, BeneficiaryRef: ref, PolicyVersion: SaaSGiftPolicyVersion}
}

func createGift(t *testing.T, ref string) *SaaSGiftAccount {
	t.Helper()
	a, err := EnsureSaaSGiftAccount(context.Background(), giftEnsure(ref))
	require.NoError(t, err)
	require.NotNil(t, a.UserID)
	require.NotNil(t, a.TokenID)
	return a
}

func giftCredit(account *SaaSGiftAccount, id string, amount int) SaaSGiftOperationRequest {
	return SaaSGiftOperationRequest{OperationID: id, AccountID: account.ID, Kind: "credit", Amount: amount}
}

func giftBalance(t *testing.T, account *SaaSGiftAccount) (int, int) {
	t.Helper()
	u, err := GetUserById(*account.UserID, true)
	require.NoError(t, err)
	k, err := GetTokenById(*account.TokenID)
	require.NoError(t, err)
	return u.Quota, k.RemainQuota
}

func TestSaaSGiftEnsureAtomicReplayAndIsolation(t *testing.T) {
	setupSaaSGiftTest(t)
	oldSignup := common.QuotaForNewUser
	common.QuotaForNewUser = 999
	t.Cleanup(func() { common.QuotaForNewUser = oldSignup })
	var wg sync.WaitGroup
	results := make(chan *SaaSGiftAccount, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := EnsureSaaSGiftAccount(context.Background(), giftEnsure("wallet-a"))
			results <- a
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var account *SaaSGiftAccount
	created := 0
	for a := range results {
		if a.Created {
			created++
		}
		if account != nil {
			assert.Equal(t, account.ID, a.ID)
			assert.Equal(t, *account.TokenID, *a.TokenID)
		}
		account = a
	}
	assert.Equal(t, 1, created)
	u, err := GetUserById(*account.UserID, true)
	require.NoError(t, err)
	k, err := GetTokenById(*account.TokenID)
	require.NoError(t, err)
	assert.Equal(t, 0, u.Quota)
	assert.Equal(t, 0, k.RemainQuota)
	assert.Equal(t, common.RoleCommonUser, u.Role)
	assert.Empty(t, u.Password)
	assert.Empty(t, u.Email)
	assert.Equal(t, "双十一", k.Group)
	assert.Equal(t, SaaSGiftModel, k.ModelLimits)
	assert.True(t, k.ModelLimitsEnabled)
	assert.False(t, k.UnlimitedQuota)
	assert.False(t, k.CrossGroupRetry)
	encoded, err := common.Marshal(account)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), k.Key)
	other := createGift(t, "wallet-b")
	assert.NotEqual(t, *account.UserID, *other.UserID, "gift customers must not share the user-level rate limit")
	var n int64
	require.NoError(t, DB.Model(&SaaSGiftAccount{}).Count(&n).Error)
	assert.EqualValues(t, 2, n)
}

func TestSaaSGiftEnsureFailuresLeaveNoResources(t *testing.T) {
	setupSaaSGiftTest(t)
	require.NoError(t, DB.Create(&Ability{Group: "双十一", Model: "unrestricted-model", ChannelId: 1, Enabled: true}).Error)
	_, err := EnsureSaaSGiftAccount(context.Background(), giftEnsure("bad-policy"))
	require.ErrorIs(t, err, ErrSaaSGiftPolicy)
	var n int64
	require.NoError(t, DB.Model(&User{}).Where("saas_gift_account_id <> ?", "").Count(&n).Error)
	assert.Zero(t, n)
	require.NoError(t, DB.Model(&SaaSGiftAccount{}).Count(&n).Error)
	assert.Zero(t, n)
	require.NoError(t, DB.Where("model = ?", "unrestricted-model").Delete(&Ability{}).Error)
	const callback = "gift:fail-token"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			tx.AddError(errors.New("injected token failure"))
		}
	}))
	_, err = EnsureSaaSGiftAccount(context.Background(), giftEnsure("failed-create"))
	require.Error(t, err)
	require.NoError(t, DB.Callback().Create().Remove(callback))
	require.NoError(t, DB.Model(&User{}).Where("saas_gift_account_id <> ?", "").Count(&n).Error)
	assert.Zero(t, n)
	require.NoError(t, DB.Model(&SaaSGiftAccount{}).Count(&n).Error)
	assert.Zero(t, n)
}

func TestSaaSGiftCreditReplayConflictAndOrdinaryGrantDenied(t *testing.T) {
	setupSaaSGiftTest(t)
	a := createGift(t, "wallet-credit")
	q := giftCredit(a, "grant:order1", 120000000)
	for i := 0; i < 2; i++ {
		op, err := ApplySaaSGiftOperation(context.Background(), q)
		require.NoError(t, err)
		assert.Equal(t, "APPLIED", op.State)
		require.NotNil(t, op.Receipt)
		assert.Equal(t, i == 1, op.Duplicated)
	}
	u, k := giftBalance(t, a)
	assert.Equal(t, q.Amount, u)
	assert.Equal(t, q.Amount, k)
	q.Amount++
	_, err := ApplySaaSGiftOperation(context.Background(), q)
	assert.ErrorIs(t, err, ErrSaaSGiftConflict)
	_, err = ApplySaaSCreditOperation(context.Background(), SaaSCreditOperationRequest{OperationID: "normal-wallet", UserID: *a.UserID, Amount: 1, CreditUserQuota: true})
	assert.ErrorIs(t, err, ErrSaaSCreditTargetNotFound)
	_, err = ApplySaaSCreditOperation(context.Background(), SaaSCreditOperationRequest{OperationID: "normal-token", UserID: *a.UserID, TokenID: a.TokenID, Amount: 1, CreditUserQuota: false})
	assert.ErrorIs(t, err, ErrSaaSCreditTargetNotFound)
	token, err := GetTokenById(*a.TokenID)
	require.NoError(t, err)
	assert.ErrorIs(t, GrantTokenRemainQuota(token.Id, token.Key, 1), ErrSaaSGiftRestricted)
	token.ModelLimitsEnabled = false
	assert.ErrorIs(t, token.Update(), ErrSaaSGiftRestricted)
	assert.ErrorIs(t, (&Token{UserId: *a.UserID, Key: "other"}).Insert(), ErrSaaSGiftRestricted)
	assert.ErrorIs(t, token.Delete(), ErrSaaSGiftRestricted)
	_, err = BatchDeleteTokensByIds([]int{token.Id})
	assert.ErrorIs(t, err, ErrSaaSGiftRestricted)
	user, err := GetUserById(*a.UserID, true)
	require.NoError(t, err)
	user.Role = common.RoleAdminUser
	assert.ErrorIs(t, user.Update(false), ErrSaaSGiftRestricted)
	password, err := common.Password2Hash("gift-test-password")
	require.NoError(t, err)
	access := "gift-test-access"
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{"password": password, "access_token": access}).Error)
	login := User{Username: user.Username, Password: "gift-test-password"}
	assert.ErrorIs(t, login.ValidateAndFill(), ErrInvalidCredentials)
	interactive, err := ValidateAccessToken(access)
	require.NoError(t, err)
	assert.Nil(t, interactive)
	op, err := GetSaaSGiftOperation(context.Background(), "grant:order1")
	require.NoError(t, err)
	require.NotNil(t, op.Receipt)
	assert.Equal(t, 120000000, op.Receipt.Amount)
}

func TestSaaSGiftFreezeBeforeEnsureAndCancelBeforeCredit(t *testing.T) {
	setupSaaSGiftTest(t)
	freeze := SaaSGiftOperationRequest{OperationID: "refund:freeze", CampaignID: SaaSGiftCampaign, BeneficiaryRef: "wallet-frozen", Kind: "freeze"}
	_, err := ApplySaaSGiftOperation(context.Background(), freeze)
	require.NoError(t, err)
	a, err := EnsureSaaSGiftAccount(context.Background(), giftEnsure("wallet-frozen"))
	require.NoError(t, err)
	assert.Equal(t, SaaSGiftFrozen, a.State)
	assert.Nil(t, a.UserID)
	assert.Nil(t, a.TokenID)
	cancel := SaaSGiftOperationRequest{OperationID: "refund:cancel", CampaignID: SaaSGiftCampaign, BeneficiaryRef: "wallet-canceled", Kind: "cancel_credit", TargetOperationID: "grant:late"}
	op, err := ApplySaaSGiftOperation(context.Background(), cancel)
	require.NoError(t, err)
	assert.Equal(t, "CANCELED", op.State)
	a = createGift(t, "wallet-canceled")
	for i := 0; i < 2; i++ {
		op, err = ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:late", 500))
		require.NoError(t, err)
		assert.Equal(t, "CANCELED", op.State)
		assert.Nil(t, op.Receipt)
	}
	u, k := giftBalance(t, a)
	assert.Zero(t, u)
	assert.Zero(t, k)
	op, err = GetSaaSGiftOperation(context.Background(), "grant:late")
	require.NoError(t, err)
	assert.Equal(t, "CANCELED", op.State)
}

func TestSaaSGiftConcurrentCancelAndCredit(t *testing.T) {
	setupSaaSGiftTest(t)
	for i := 0; i < 8; i++ {
		a := createGift(t, fmt.Sprintf("race-%d", i))
		grant := fmt.Sprintf("grant:race-%d", i)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, grant, 50))
			errs <- err
		}()
		go func() {
			<-start
			_, err := ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: fmt.Sprintf("cancel:race-%d", i), AccountID: a.ID, Kind: "cancel_credit", TargetOperationID: grant})
			errs <- err
		}()
		close(start)
		require.NoError(t, <-errs)
		require.NoError(t, <-errs)
		op, err := GetSaaSGiftOperation(context.Background(), grant)
		require.NoError(t, err)
		u, k := giftBalance(t, a)
		assert.Equal(t, u, k)
		if op.State == "APPLIED" {
			assert.Equal(t, 50, u)
		} else {
			assert.Equal(t, "CANCELED", op.State)
			assert.Zero(t, u)
		}
		_, err = ApplySaaSGiftOperation(context.Background(), giftCredit(a, grant, 50))
		require.NoError(t, err)
		u2, k2 := giftBalance(t, a)
		assert.Equal(t, u, u2)
		assert.Equal(t, k, k2)
	}
}

func TestSaaSGiftFreezeBlocksNewUsageButAllowsAcceptedSettlement(t *testing.T) {
	setupSaaSGiftTest(t)
	a := createGift(t, "wallet-usage")
	_, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:usage", 100))
	require.NoError(t, err)
	require.NoError(t, ReserveUserQuota(*a.UserID, 10))
	token, err := GetTokenById(*a.TokenID)
	require.NoError(t, err)
	require.NoError(t, ValidateSaaSGiftToken(context.Background(), token))
	_, err = ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: "freeze:usage", AccountID: a.ID, Kind: "freeze"})
	require.NoError(t, err)
	assert.Error(t, ReserveUserQuota(*a.UserID, 1))
	assert.ErrorIs(t, ReserveUserQuota(*a.UserID, 0), ErrSaaSGiftFrozen)
	require.NoError(t, ReserveUserQuotaForSettlement(*a.UserID, 5))
	require.NoError(t, IncreaseUserQuota(*a.UserID, 10, true))
	assert.ErrorIs(t, ValidateSaaSGiftToken(context.Background(), token), ErrSaaSGiftFrozen)
	_, err = ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:new-after-freeze", 50))
	assert.ErrorIs(t, err, ErrSaaSGiftFrozen)
	old, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:usage", 100))
	require.NoError(t, err)
	assert.Equal(t, "APPLIED", old.State)
	u, err := GetUserById(*a.UserID, true)
	require.NoError(t, err)
	assert.Equal(t, 95, u.Quota)
	_, err = ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: "cancel:applied", AccountID: a.ID, Kind: "cancel_credit", TargetOperationID: "grant:usage"})
	require.NoError(t, err)
	cancel, err := GetSaaSGiftOperation(context.Background(), "cancel:applied")
	require.NoError(t, err)
	assert.Equal(t, "APPLIED", cancel.State)
	require.NotNil(t, cancel.Receipt)
}

func TestSaaSGiftCachedUsageCreditAndFreeze(t *testing.T) {
	setupSaaSGiftTest(t)
	a := createGift(t, "cached-wallet")
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	common.RDB, common.RedisEnabled = client, true
	resetUserCacheEpochInitializationForTest()
	t.Cleanup(func() {
		drainCreditCacheWorkers(t)
		require.NoError(t, client.Close())
		common.RDB = oldClient
		resetUserCacheEpochInitializationForTest()
	})
	token, err := GetTokenById(*a.TokenID)
	require.NoError(t, err)
	require.NoError(t, cacheSetToken(*token))
	_, err = ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:cached", 100))
	require.NoError(t, err)
	require.NoError(t, cacheDecrTokenQuota(token.Key, 20))
	_, err = ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:cached", 100))
	require.NoError(t, err)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 80, cached.RemainQuota)
	assert.Equal(t, a.ID, cached.SaaSGiftAccountID)
	common.RDB = nil
	_, err = ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: "freeze:cache-offline", AccountID: a.ID, Kind: "freeze"})
	require.NoError(t, err)
	assert.ErrorIs(t, ValidateSaaSGiftToken(context.Background(), cached), ErrSaaSGiftFrozen)
	assert.Error(t, ReserveUserQuota(*a.UserID, 1))
	common.RDB = client
	old, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:cached", 100))
	require.NoError(t, err)
	assert.True(t, old.Duplicated)
	assert.ErrorIs(t, ValidateSaaSGiftToken(context.Background(), cached), ErrSaaSGiftFrozen)
}
