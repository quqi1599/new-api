package model

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	SaaSGiftCampaign      = "double11-2026"
	SaaSGiftPolicyVersion = "double11-2026-v1"
	SaaSGiftModel         = "claude-sonnet-4-6"
	SaaSGiftActive        = "ACTIVE"
	SaaSGiftFrozen        = "FROZEN_PENDING_REVIEW"
)

var (
	ErrSaaSGiftInvalid    = errors.New("invalid gift operation")
	ErrSaaSGiftConflict   = errors.New("gift operation conflicts with its stored identity")
	ErrSaaSGiftPolicy     = errors.New("gift group or model policy is not ready")
	ErrSaaSGiftFrozen     = errors.New("gift account is frozen pending review")
	ErrSaaSGiftRestricted = errors.New("gift accounts must use the gift service")
)

// The opaque identity is supplied by SaaS; never store the customer's email here.
// Frozen identities may exist before their user and token are provisioned.
type SaaSGiftAccount struct {
	ID                string `gorm:"size:64;primaryKey" json:"accountId"`
	CampaignID        string `gorm:"size:64;not null" json:"campaignId"`
	BeneficiaryRef    string `gorm:"size:160;not null" json:"-"`
	PolicyVersion     string `gorm:"size:64;not null" json:"policyVersion"`
	PolicyFingerprint string `gorm:"size:64;not null" json:"policyFingerprint"`
	Group             string `gorm:"size:64;not null" json:"group"`
	UserID            *int   `gorm:"uniqueIndex" json:"userId"`
	TokenID           *int   `gorm:"uniqueIndex" json:"tokenId"`
	State             string `gorm:"size:32;not null" json:"state"`
	CreatedAt         int64  `gorm:"not null" json:"createdAt"`
	Created           bool   `gorm:"-" json:"created"`
}

func (SaaSGiftAccount) TableName() string { return "saas_gift_accounts" }

// One credit slot can be canceled before it exists. Restoring an account never
// clears that terminal tombstone or creates a different ID for the same grant.
type SaaSGiftOperation struct {
	ID                string               `gorm:"size:64;primaryKey" json:"-"`
	OperationID       string               `gorm:"size:160;not null" json:"operationId"`
	AccountID         string               `gorm:"size:64;not null;index" json:"accountId"`
	Kind              string               `gorm:"size:24;not null" json:"kind"`
	Amount            int                  `gorm:"size:64;not null;default:0" json:"amount"`
	TargetOperationID string               `gorm:"size:160;not null;default:''" json:"targetOperationId,omitempty"`
	State             string               `gorm:"size:32;not null" json:"state"`
	AppliedAt         int64                `gorm:"not null" json:"appliedAt"`
	Receipt           *SaaSCreditOperation `gorm:"-" json:"receipt,omitempty"`
	Duplicated        bool                 `gorm:"-" json:"duplicated"`
}

func (SaaSGiftOperation) TableName() string { return "saas_gift_operations" }

type SaaSGiftEnsureRequest struct {
	OperationID    string `json:"operationId"`
	CampaignID     string `json:"campaignId"`
	BeneficiaryRef string `json:"beneficiaryRef"`
	PolicyVersion  string `json:"policyVersion"`
}

type SaaSGiftOperationRequest struct {
	OperationID       string `json:"operationId"`
	AccountID         string `json:"accountId,omitempty"`
	CampaignID        string `json:"campaignId,omitempty"`
	BeneficiaryRef    string `json:"beneficiaryRef,omitempty"`
	Kind              string `json:"kind"`
	Amount            int    `json:"amount,omitempty"`
	TargetOperationID string `json:"targetOperationId,omitempty"`
}

func saasGiftGroup() string {
	if group := strings.TrimSpace(os.Getenv("SAAS_GIFT_GROUP")); group != "" {
		return group
	}
	return "双十一"
}

func saasGiftIdentity(campaignID, beneficiary string) (string, error) {
	if campaignID != SaaSGiftCampaign || !ValidSaaSCreditOperationID(beneficiary) {
		return "", ErrSaaSGiftInvalid
	}
	return saasCreditOperationKey(campaignID + "\x00" + beneficiary), nil
}

func giftAccount(campaignID, beneficiary string) (*SaaSGiftAccount, error) {
	id, err := saasGiftIdentity(campaignID, beneficiary)
	if err != nil {
		return nil, err
	}
	group := saasGiftGroup()
	if group == "auto" || len(group) > 64 {
		return nil, ErrSaaSGiftPolicy
	}
	return &SaaSGiftAccount{ID: id, CampaignID: campaignID, BeneficiaryRef: beneficiary,
		PolicyVersion: SaaSGiftPolicyVersion, PolicyFingerprint: saasCreditOperationKey(SaaSGiftPolicyVersion + "\x00" + group + "\x00" + SaaSGiftModel),
		Group: group, State: SaaSGiftActive, CreatedAt: common.GetTimestamp()}, nil
}

func validateGiftGroup(tx *gorm.DB, account *SaaSGiftAccount) error {
	ratio := ratio_setting.GetGroupRatio(account.Group)
	if !ratio_setting.ContainsGroupRatio(account.Group) || ratio <= 0 || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		return ErrSaaSGiftPolicy
	}
	var models []string
	if err := tx.Model(&Ability{}).Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: account.Group}).Where("enabled = ?", true).Distinct("model").Pluck("model", &models).Error; err != nil {
		return err
	}
	if len(models) != 1 || models[0] != SaaSGiftModel {
		return ErrSaaSGiftPolicy
	}
	return nil
}

// Write first for SQLite; lock the same identity for ensure, credit and freeze.
func lockGiftAccount(tx *gorm.DB, candidate *SaaSGiftAccount) (*SaaSGiftAccount, error) {
	if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(candidate).Error; err != nil {
		return nil, err
	}
	var stored SaaSGiftAccount
	locked := tx
	if tx.Dialector.Name() != "sqlite" {
		locked = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Session(&gorm.Session{})
	}
	if err := locked.Where("id = ?", candidate.ID).First(&stored).Error; err != nil {
		return nil, err
	}
	if stored.CampaignID != candidate.CampaignID || stored.BeneficiaryRef != candidate.BeneficiaryRef {
		return nil, ErrSaaSGiftConflict
	}
	return &stored, nil
}

func giftTransaction(ctx context.Context, run func(*gorm.DB) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = DB.WithContext(ctx).Transaction(run)
		if err == nil || !isSaaSCreditRetryableDBError(err) || attempt == 3 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func loadGiftOperation(tx *gorm.DB, id string) (*SaaSGiftOperation, error) {
	var op SaaSGiftOperation
	if err := tx.Where("id = ?", saasCreditOperationKey(id)).First(&op).Error; err != nil {
		return nil, err
	}
	if op.OperationID != id {
		return nil, ErrSaaSGiftConflict
	}
	return &op, nil
}

func fillGiftReceipt(ctx context.Context, op *SaaSGiftOperation) error {
	if op.State != "APPLIED" || (op.Kind != "credit" && op.Kind != "cancel_credit") {
		return nil
	}
	id := op.OperationID
	if op.Kind == "cancel_credit" {
		id = op.TargetOperationID
	}
	receipt, err := loadSaaSCreditOperation(ctx, id)
	if err != nil {
		return err
	}
	op.Receipt = receipt
	return nil
}

func GetSaaSGiftOperation(ctx context.Context, operationID string) (*SaaSGiftOperation, error) {
	if !ValidSaaSCreditOperationID(operationID) {
		return nil, ErrSaaSGiftInvalid
	}
	op, err := loadGiftOperation(DB.WithContext(ctx), operationID)
	if err == nil {
		err = fillGiftReceipt(ctx, op)
	}
	return op, err
}

func EnsureSaaSGiftAccount(ctx context.Context, req SaaSGiftEnsureRequest) (*SaaSGiftAccount, error) {
	if !ValidSaaSCreditOperationID(req.OperationID) || req.PolicyVersion != SaaSGiftPolicyVersion {
		return nil, ErrSaaSGiftInvalid
	}
	candidate, err := giftAccount(req.CampaignID, req.BeneficiaryRef)
	if err != nil {
		return nil, err
	}
	var result *SaaSGiftAccount
	err = giftTransaction(ctx, func(tx *gorm.DB) error {
		account, err := lockGiftAccount(tx, candidate)
		if err != nil {
			return err
		}
		result = account
		existing, err := loadGiftOperation(tx, req.OperationID)
		if err == nil {
			if existing.Kind != "ensure" || existing.AccountID != account.ID {
				return ErrSaaSGiftConflict
			}
			if account.State == SaaSGiftActive {
				if account.PolicyFingerprint != candidate.PolicyFingerprint {
					return ErrSaaSGiftPolicy
				}
				if err := validateGiftBinding(tx, account); err != nil {
					return err
				}
				return validateGiftGroup(tx, account)
			}
			if account.State != SaaSGiftFrozen {
				return ErrSaaSGiftPolicy
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if account.PolicyFingerprint != candidate.PolicyFingerprint {
			return ErrSaaSGiftPolicy
		}
		if account.State != SaaSGiftFrozen {
			if err := validateGiftGroup(tx, account); err != nil {
				return err
			}
			if account.UserID == nil && account.TokenID == nil {
				key, err := common.GenerateKey()
				if err != nil {
					return err
				}
				user := User{Username: "gift" + account.ID[:16], DisplayName: "双十一赠送", Password: "", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: account.Group, AffCode: account.ID[:32], SaaSGiftAccountID: account.ID, Quota: 0}
				// Explicit zero balances: regular registration can grant signup quota.
				if err := tx.Create(&user).Error; err != nil {
					return err
				}
				token := Token{UserId: user.Id, Key: key, Name: "双十一赠送", Status: common.TokenStatusEnabled, CreatedTime: common.GetTimestamp(), ExpiredTime: -1, Group: account.Group, ModelLimitsEnabled: true, ModelLimits: SaaSGiftModel, SaaSGiftAccountID: account.ID}
				if err := tx.Create(&token).Error; err != nil {
					return err
				}
				account.UserID, account.TokenID = &user.Id, &token.Id
				if err := tx.Model(account).Updates(map[string]any{"user_id": user.Id, "token_id": token.Id}).Error; err != nil {
					return err
				}
				account.Created = true
			} else if err := validateGiftBinding(tx, account); err != nil {
				return err
			}
		}
		return tx.Create(&SaaSGiftOperation{ID: saasCreditOperationKey(req.OperationID), OperationID: req.OperationID, Kind: "ensure", AccountID: account.ID, State: account.State, AppliedAt: common.GetTimestamp()}).Error
	})
	if err != nil {
		// Same operation on a different account may race its unique insertion.
		if op, readErr := loadGiftOperation(DB.WithContext(ctx), req.OperationID); readErr == nil {
			if op.AccountID != candidate.ID || op.Kind != "ensure" {
				return nil, ErrSaaSGiftConflict
			}
			// Resolve a lost COMMIT acknowledgement from the durable identity.
			var recovered SaaSGiftAccount
			if readErr := DB.WithContext(ctx).Where("id = ?", candidate.ID).First(&recovered).Error; readErr == nil {
				if recovered.State == SaaSGiftFrozen {
					return &recovered, nil
				}
				if recovered.PolicyFingerprint != candidate.PolicyFingerprint {
					return nil, ErrSaaSGiftPolicy
				}
				if readErr := validateGiftBinding(DB.WithContext(ctx), &recovered); readErr != nil {
					return nil, readErr
				}
				if readErr := validateGiftGroup(DB.WithContext(ctx), &recovered); readErr != nil {
					return nil, readErr
				}
				return &recovered, nil
			}
		}
		return nil, err
	}
	return result, nil
}

func validateGiftBinding(tx *gorm.DB, account *SaaSGiftAccount) error {
	if account.UserID == nil || account.TokenID == nil {
		return ErrSaaSGiftPolicy
	}
	var user User
	var token Token
	if err := tx.First(&user, *account.UserID).Error; err != nil {
		return err
	}
	if err := tx.First(&token, *account.TokenID).Error; err != nil {
		return err
	}
	if user.SaaSGiftAccountID != account.ID || user.Role != common.RoleCommonUser || user.Status != common.UserStatusEnabled || user.Group != account.Group || user.SaaSGiftFrozen ||
		token.SaaSGiftAccountID != account.ID || token.UserId != user.Id || token.Group != account.Group || token.UnlimitedQuota || token.CrossGroupRetry || !token.ModelLimitsEnabled || token.ModelLimits != SaaSGiftModel || token.Status == common.TokenStatusDisabled || token.ExpiredTime != -1 {
		return ErrSaaSGiftPolicy
	}
	return nil
}

func resolveGiftAccount(ctx context.Context, req SaaSGiftOperationRequest) (*SaaSGiftAccount, error) {
	if req.CampaignID != "" || req.BeneficiaryRef != "" {
		account, err := giftAccount(req.CampaignID, req.BeneficiaryRef)
		if err != nil {
			return nil, err
		}
		if req.AccountID != "" && req.AccountID != account.ID {
			return nil, ErrSaaSGiftConflict
		}
		return account, nil
	}
	if len(req.AccountID) != 64 {
		return nil, ErrSaaSGiftInvalid
	}
	var account SaaSGiftAccount
	if err := DB.WithContext(ctx).Where("id = ?", req.AccountID).First(&account).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

func (op *SaaSGiftOperation) matchesGift(req SaaSGiftOperationRequest, accountID string) bool {
	return op.AccountID == accountID && op.Kind == req.Kind && op.TargetOperationID == req.TargetOperationID &&
		(op.Amount == req.Amount || (op.Kind == "credit" && op.State == "CANCELED"))
}

func ApplySaaSGiftOperation(ctx context.Context, req SaaSGiftOperationRequest) (*SaaSGiftOperation, error) {
	if !ValidSaaSCreditOperationID(req.OperationID) || req.Amount < 0 || int64(req.Amount) > MaxSaaSCreditQuota {
		return nil, ErrSaaSGiftInvalid
	}
	switch req.Kind {
	case "credit":
		if req.Amount <= 0 || req.TargetOperationID != "" {
			return nil, ErrSaaSGiftInvalid
		}
	case "freeze":
		if req.Amount != 0 || req.TargetOperationID != "" {
			return nil, ErrSaaSGiftInvalid
		}
	case "cancel_credit":
		if req.Amount != 0 || !ValidSaaSCreditOperationID(req.TargetOperationID) || req.TargetOperationID == req.OperationID {
			return nil, ErrSaaSGiftInvalid
		}
	default:
		return nil, ErrSaaSGiftInvalid
	}
	candidate, err := resolveGiftAccount(ctx, req)
	if err != nil {
		return nil, err
	}
	var result *SaaSGiftOperation
	var credited *SaaSCreditOperation
	var frozenUser *int
	err = giftTransaction(ctx, func(tx *gorm.DB) error {
		credited, frozenUser = nil, nil
		account, err := lockGiftAccount(tx, candidate)
		if err != nil {
			return err
		}
		existing, err := loadGiftOperation(tx, req.OperationID)
		if err == nil {
			if !existing.matchesGift(req, account.ID) {
				return ErrSaaSGiftConflict
			}
			existing.Duplicated = true
			result = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		op := &SaaSGiftOperation{ID: saasCreditOperationKey(req.OperationID), OperationID: req.OperationID, AccountID: account.ID, Kind: req.Kind, Amount: req.Amount, TargetOperationID: req.TargetOperationID, State: "APPLIED", AppliedAt: common.GetTimestamp()}
		switch req.Kind {
		case "credit":
			if account.State != SaaSGiftActive {
				return ErrSaaSGiftFrozen
			}
			currentPolicy, err := giftAccount(account.CampaignID, account.BeneficiaryRef)
			if err != nil {
				return err
			}
			if account.PolicyFingerprint != currentPolicy.PolicyFingerprint {
				return ErrSaaSGiftPolicy
			}
			if err := validateGiftBinding(tx, account); err != nil {
				return err
			}
			if err := validateGiftGroup(tx, account); err != nil {
				return err
			}
			creditReq := SaaSCreditOperationRequest{OperationID: req.OperationID, UserID: *account.UserID, TokenID: account.TokenID, Amount: req.Amount, CreditUserQuota: true, Note: "双十一领取赠送额度"}
			credited = newSaaSCreditOperation(creditReq)
			if err := applySaaSCreditOperationTx(tx, creditReq, credited, true); err != nil {
				return err
			}
		case "freeze":
			if err := tx.Model(account).Update("state", SaaSGiftFrozen).Error; err != nil {
				return err
			}
			if account.UserID != nil {
				if err := tx.Model(&User{}).Where("id = ? AND saas_gift_account_id = ?", *account.UserID, account.ID).Update("saas_gift_frozen", true).Error; err != nil {
					return err
				}
				frozenUser = account.UserID
			}
			op.State = SaaSGiftFrozen
		case "cancel_credit":
			target, err := loadGiftOperation(tx, req.TargetOperationID)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var ordinaryReceipt SaaSCreditOperation
				check := tx.Where("id = ?", saasCreditOperationKey(req.TargetOperationID)).First(&ordinaryReceipt).Error
				if check == nil {
					return ErrSaaSGiftConflict
				}
				if !errors.Is(check, gorm.ErrRecordNotFound) {
					return check
				}
				target = &SaaSGiftOperation{ID: saasCreditOperationKey(req.TargetOperationID), OperationID: req.TargetOperationID, AccountID: account.ID, Kind: "credit", State: "CANCELED", AppliedAt: op.AppliedAt}
				if err := tx.Create(target).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if target.AccountID != account.ID || target.Kind != "credit" {
				return ErrSaaSGiftConflict
			}
			op.State = target.State
		}
		if err := tx.Create(op).Error; err != nil {
			return err
		}
		result = op
		return nil
	})
	if err != nil {
		if op, readErr := loadGiftOperation(DB.WithContext(ctx), req.OperationID); readErr == nil {
			if !op.matchesGift(req, candidate.ID) {
				return nil, ErrSaaSGiftConflict
			}
			// The matching receipt wins over an uncertain acknowledgement.
			op.Duplicated = true
			result = op
			credited = nil
			frozenUser = nil
			err = nil
		}
		if err != nil && req.Kind == "credit" {
			if _, readErr := loadSaaSCreditOperation(ctx, req.OperationID); readErr == nil {
				return nil, ErrSaaSGiftConflict
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if credited != nil {
		repairSaaSCreditOperationCaches(credited)
		recordSaaSCreditLog(credited)
	}
	if frozenUser != nil {
		MarkUserCacheDirty(*frozenUser)
		_ = InvalidateUserCache(*frozenUser)
	}
	if err := fillGiftReceipt(ctx, result); err != nil {
		return nil, err
	}
	if result.Receipt != nil && result.Kind == "credit" && result.Duplicated {
		repairSaaSCreditOperationCaches(result.Receipt)
	}
	return result, nil
}

func IsSaaSGiftUser(userID int) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	var user User
	err := DB.Select("saas_gift_account_id").Where("id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return user.SaaSGiftAccountID != "", err
}

func isSaaSGiftToken(tokenID int) (bool, error) {
	var token Token
	err := DB.Select("saas_gift_account_id").Where("id = ?", tokenID).First(&token).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return token.SaaSGiftAccountID != "", err
}

// Only gifted tokens pay this extra read; regular token/cache behavior is unchanged.
func ValidateSaaSGiftToken(ctx context.Context, token *Token) error {
	if token.SaaSGiftAccountID == "" {
		return nil
	}
	var account SaaSGiftAccount
	if err := DB.WithContext(ctx).Where("id = ?", token.SaaSGiftAccountID).First(&account).Error; err != nil {
		return err
	}
	if account.State != SaaSGiftActive {
		return ErrSaaSGiftFrozen
	}
	if account.TokenID == nil || *account.TokenID != token.Id || account.UserID == nil || *account.UserID != token.UserId {
		return ErrSaaSGiftPolicy
	}
	if token.Group != account.Group || token.UnlimitedQuota || token.CrossGroupRetry || !token.ModelLimitsEnabled || token.ModelLimits != SaaSGiftModel || token.ExpiredTime != -1 {
		return ErrSaaSGiftPolicy
	}
	return validateGiftBinding(DB.WithContext(ctx), &account)
}
