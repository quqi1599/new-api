package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	sqlitedriver "github.com/glebarez/go-sqlite"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaxSaaSCreditQuota is the exact integer boundary shared by the SaaS JSON
// client and Redis Lua. Billing's per-request int32 clamp is unrelated to a
// wallet's cumulative balance; the database quota fields use 64-bit storage.
const MaxSaaSCreditQuota int64 = 1<<53 - 1

var (
	ErrSaaSCreditInvalid         = errors.New("invalid credit operation")
	ErrSaaSCreditConflict        = errors.New("operationId already belongs to a different credit operation")
	ErrSaaSCreditTargetNotFound  = errors.New("credit target not found")
	ErrSaaSCreditOverflow        = errors.New("credit exceeds quota limit")
	saasCreditOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,159}$`)
)

type SaaSCreditOperationRequest struct {
	OperationID     string `json:"operationId"`
	UserID          int    `json:"userId"`
	TokenID         *int   `json:"tokenId"`
	Amount          int    `json:"amount"`
	CreditUserQuota bool   `json:"creditUserQuota"`
	Note            string `json:"note,omitempty"`
}

// SaaSCreditOperation is both the idempotency ledger and the immutable receipt.
// A hash primary key keeps operation IDs byte-exact across database collations.
// No raw token key is persisted; the optional note is informational only.
type SaaSCreditOperation struct {
	ID                    string `gorm:"size:64;primaryKey" json:"-"`
	OperationID           string `gorm:"size:160;not null" json:"operationId"`
	UserID                int    `gorm:"not null;index" json:"userId"`
	TokenID               *int   `json:"tokenId"`
	Amount                int    `gorm:"size:64;not null" json:"amount"`
	CreditUserQuota       bool   `gorm:"not null" json:"creditUserQuota"`
	BeforeUserQuota       int    `gorm:"size:64" json:"beforeUserQuota"`
	AfterUserQuota        int    `gorm:"size:64" json:"afterUserQuota"`
	BeforeRemainQuota     *int   `gorm:"size:64" json:"beforeRemainQuota"`
	AfterRemainQuota      *int   `gorm:"size:64" json:"afterRemainQuota"`
	AppliedAt             int64  `gorm:"not null" json:"appliedAt"`
	Note                  string `gorm:"size:500" json:"-"`
	AfterTokenCreditTotal int64  `gorm:"not null;default:0" json:"-"`
	Duplicated            bool   `gorm:"-" json:"duplicated"`
	LogTokenName          string `gorm:"-" json:"-"`
}

func (SaaSCreditOperation) TableName() string { return "saas_credit_operations" }

func ValidSaaSCreditOperationID(id string) bool {
	return saasCreditOperationIDPattern.MatchString(id)
}

func (req SaaSCreditOperationRequest) Validate() error {
	if !ValidSaaSCreditOperationID(req.OperationID) || req.UserID <= 0 || req.Amount <= 0 ||
		int64(req.Amount) > MaxSaaSCreditQuota || len(req.Note) > 500 ||
		(req.TokenID == nil && !req.CreditUserQuota) || (req.TokenID != nil && *req.TokenID <= 0) {
		return ErrSaaSCreditInvalid
	}
	return nil
}

func saasCreditOperationKey(id string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
}

func (op *SaaSCreditOperation) matches(req SaaSCreditOperationRequest) bool {
	return op.OperationID == req.OperationID && op.UserID == req.UserID && op.Amount == req.Amount &&
		op.CreditUserQuota == req.CreditUserQuota &&
		((op.TokenID == nil && req.TokenID == nil) || (op.TokenID != nil && req.TokenID != nil && *op.TokenID == *req.TokenID))
}

func GetSaaSCreditOperation(ctx context.Context, operationID string) (*SaaSCreditOperation, error) {
	return loadSaaSCreditOperation(ctx, operationID)
}

func loadSaaSCreditOperation(ctx context.Context, operationID string) (*SaaSCreditOperation, error) {
	if !ValidSaaSCreditOperationID(operationID) {
		return nil, ErrSaaSCreditInvalid
	}
	var op SaaSCreditOperation
	if err := DB.WithContext(ctx).Where("id = ?", saasCreditOperationKey(operationID)).First(&op).Error; err != nil {
		return nil, err
	}
	return &op, nil
}

// ApplySaaSCreditOperation never enters the lossy batch queue. The unique
// receipt, wallet increment, token increment and exhaustion recovery commit
// together; a failed commit can always be resolved using this operation ID.
func ApplySaaSCreditOperation(ctx context.Context, req SaaSCreditOperationRequest) (*SaaSCreditOperation, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var op *SaaSCreditOperation
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		op, err = applySaaSCreditOperationOnce(ctx, req)
		if err == nil {
			repairSaaSCreditOperationCaches(op)
			recordSaaSCreditLog(op)
			return op, nil
		}
		// Lookup occurs outside the failed transaction: PostgreSQL aborts a
		// transaction after a uniqueness violation. This also resolves a commit
		// whose acknowledgement was lost without applying the amount again.
		if existing, lookupErr := loadSaaSCreditOperation(ctx, req.OperationID); lookupErr == nil {
			if !existing.matches(req) {
				return nil, ErrSaaSCreditConflict
			}
			existing.Duplicated = true
			repairSaaSCreditOperationCaches(existing)
			return existing, nil
		}
		if !isSaaSCreditRetryableDBError(err) || attempt == 3 {
			return nil, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, err
}

func applySaaSCreditOperationOnce(ctx context.Context, req SaaSCreditOperationRequest) (*SaaSCreditOperation, error) {
	op := &SaaSCreditOperation{
		ID: saasCreditOperationKey(req.OperationID), OperationID: req.OperationID,
		UserID: req.UserID, TokenID: req.TokenID, Amount: req.Amount,
		CreditUserQuota: req.CreditUserQuota, Note: req.Note,
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Acquire the unique operation slot before any reads. SQLite therefore
		// takes its writer lock without attempting a read-to-write lock upgrade.
		if err := tx.Create(op).Error; err != nil {
			return err
		}
		if _, excluded := constant.SaaSTopupExcludedUserIDs[req.UserID]; excluded {
			return ErrSaaSCreditTargetNotFound
		}
		locked := tx
		if tx.Dialector.Name() != "sqlite" {
			locked = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Session(&gorm.Session{})
		}
		var user User
		if err := locked.Select("id", "quota", "status", "username").Where("id = ?", req.UserID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSaaSCreditTargetNotFound
			}
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrSaaSCreditTargetNotFound
		}
		if !isSafeSaaSCreditQuota(user.Quota) {
			return ErrSaaSCreditOverflow
		}
		op.BeforeUserQuota, op.AfterUserQuota = user.Quota, user.Quota
		var token Token
		if req.TokenID != nil {
			if err := locked.Where("id = ? AND user_id = ?", *req.TokenID, req.UserID).First(&token).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrSaaSCreditTargetNotFound
				}
				return err
			}
			if token.Status == common.TokenStatusDisabled {
				return ErrSaaSCreditTargetNotFound
			}
			after, err := addSaaSCreditQuota(token.RemainQuota, req.Amount)
			if err != nil {
				return err
			}
			if token.SaaSCreditedQuota < 0 || token.SaaSCreditedQuota > MaxSaaSCreditQuota-int64(req.Amount) {
				return ErrSaaSCreditOverflow
			}
			before := token.RemainQuota
			op.BeforeRemainQuota, op.AfterRemainQuota = &before, &after
			op.AfterTokenCreditTotal = token.SaaSCreditedQuota + int64(req.Amount)
		}
		if req.CreditUserQuota {
			after, err := addSaaSCreditQuota(user.Quota, req.Amount)
			if err != nil {
				return err
			}
			op.AfterUserQuota = after
			result := tx.Model(&User{}).Where("id = ?", req.UserID).UpdateColumn("quota", gorm.Expr("quota + ?", req.Amount))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrSaaSCreditTargetNotFound
			}
		}
		if req.TokenID != nil {
			updates := map[string]interface{}{
				"remain_quota":        gorm.Expr("remain_quota + ?", req.Amount),
				"saas_credited_quota": gorm.Expr("saas_credited_quota + ?", req.Amount),
			}
			if token.Status == common.TokenStatusExhausted && token.RemainQuota+req.Amount > 0 {
				updates["status"] = common.TokenStatusEnabled
			}
			result := tx.Model(&Token{}).Where("id = ?", *req.TokenID).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrSaaSCreditTargetNotFound
			}
		}
		op.AppliedAt = common.GetTimestamp()
		op.LogTokenName = token.Name

		return tx.Model(op).Select("before_user_quota", "after_user_quota", "before_remain_quota", "after_remain_quota", "applied_at", "after_token_credit_total").Updates(op).Error
	})
	return op, err
}

func isSafeSaaSCreditQuota(quota int) bool {
	return int64(quota) >= -MaxSaaSCreditQuota && int64(quota) <= MaxSaaSCreditQuota
}

func addSaaSCreditQuota(balance int, amount int) (int, error) {
	if !isSafeSaaSCreditQuota(balance) || int64(balance) > MaxSaaSCreditQuota-int64(amount) {
		return 0, ErrSaaSCreditOverflow
	}
	return balance + amount, nil
}

func isSaaSCreditRetryableDBError(err error) bool {
	var sqliteErr *sqlitedriver.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == 5 || sqliteErr.Code()&0xff == 6
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1213 || mysqlErr.Number == 1205
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

func repairSaaSCreditOperationCaches(op *SaaSCreditOperation) {
	if !common.RedisEnabled {
		return
	}
	// Cache maintenance follows commit, never determines the receipt outcome,
	// and is safe to retry independently of the money movement.
	if op.CreditUserQuota {
		MarkUserCacheDirty(op.UserID)
		if err := InvalidateUserCache(op.UserID); err != nil {
			common.SysLog("failed to invalidate SaaS credit user cache: " + err.Error())
		}
	}
	if op.TokenID != nil && common.RedisEnabled {
		var token Token
		if err := DB.Select("id", "key").Where("id = ?", *op.TokenID).First(&token).Error; err == nil {
			if err := cacheApplySaaSTokenCredit(token.Key, op.AfterTokenCreditTotal); err != nil {
				common.SysLog("failed to refresh SaaS credit token cache: " + err.Error())
			}
		}
	}
}

// Keep the existing best-effort log path. The operation ledger is the receipt;
// durable log projection is a separate change, not a dependency of granting.
func recordSaaSCreditLog(op *SaaSCreditOperation) {
	if LOG_DB == nil {
		common.SysLog("SaaS credit committed; log database unavailable")
		return
	}
	message := fmt.Sprintf("管理员增加用户额度 %s", logger.LogQuota(op.Amount))
	if op.TokenID != nil {
		message = fmt.Sprintf("管理员为令牌 %s(ID:%d) 增加额度 %s", op.LogTokenName, *op.TokenID, logger.LogQuota(op.Amount))
	}
	if note := strings.TrimSpace(op.Note); note != "" {
		message += "，备注：" + note
	}
	RecordLog(op.UserID, LogTypeTopup, message)
}
