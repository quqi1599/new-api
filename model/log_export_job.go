package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	LogExportJobStatusQueued    = "queued"
	LogExportJobStatusRunning   = "running"
	LogExportJobStatusSucceeded = "succeeded"
	LogExportJobStatusFailed    = "failed"
)

type LogExportJob struct {
	Id             string `json:"id" gorm:"primaryKey;type:varchar(64)"`
	UserId         int    `json:"user_id" gorm:"index"`
	TokenId        int    `json:"token_id" gorm:"index:idx_log_export_jobs_token_status,priority:1"`
	Status         string `json:"status" gorm:"type:varchar(32);index;index:idx_log_export_jobs_token_status,priority:2"`
	StartTimestamp int64  `json:"start_timestamp" gorm:"bigint;index"`
	EndTimestamp   int64  `json:"end_timestamp" gorm:"bigint;index"`
	Total          int64  `json:"total" gorm:"default:0"`
	Exported       int64  `json:"exported" gorm:"default:0"`
	FilePath       string `json:"-" gorm:"type:text"`
	FileName       string `json:"filename" gorm:"type:varchar(255);default:''"`
	FileSize       int64  `json:"file_size" gorm:"default:0"`
	DownloadToken  string `json:"-" gorm:"type:varchar(64);index"`
	Error          string `json:"error" gorm:"type:text"`
	CreatedAt      int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt      int64  `json:"updated_at" gorm:"bigint;index"`
	ExpiresAt      int64  `json:"expires_at" gorm:"bigint;index"`
}

func CreateLogExportJob(userId int, tokenId int, startTimestamp int64, endTimestamp int64, expiresAt int64) (*LogExportJob, error) {
	now := common.GetTimestamp()
	job := &LogExportJob{
		Id:             common.GetUUID(),
		UserId:         userId,
		TokenId:        tokenId,
		Status:         LogExportJobStatusQueued,
		StartTimestamp: startTimestamp,
		EndTimestamp:   endTimestamp,
		DownloadToken:  common.GetUUID(),
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      expiresAt,
	}
	return job, LOG_DB.Create(job).Error
}

func GetLogExportJobById(id string) (*LogExportJob, error) {
	var job LogExportJob
	if err := LOG_DB.Where("id = ?", id).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func GetActiveLogExportJobByTokenId(tokenId int) (*LogExportJob, error) {
	var job LogExportJob
	err := LOG_DB.Where("token_id = ? AND status IN ?", tokenId, []string{LogExportJobStatusQueued, LogExportJobStatusRunning}).
		Order("created_at asc").
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func MarkLogExportJobRunning(id string, total int64) error {
	return LOG_DB.Model(&LogExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     LogExportJobStatusRunning,
			"total":      total,
			"updated_at": common.GetTimestamp(),
		}).Error
}

func UpdateLogExportJobProgress(id string, exported int64) error {
	return LOG_DB.Model(&LogExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"exported":   exported,
			"updated_at": common.GetTimestamp(),
		}).Error
}

func MarkLogExportJobSucceeded(id string, filePath string, fileName string, fileSize int64, exported int64) error {
	return LOG_DB.Model(&LogExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     LogExportJobStatusSucceeded,
			"file_path":  filePath,
			"file_name":  fileName,
			"file_size":  fileSize,
			"exported":   exported,
			"updated_at": common.GetTimestamp(),
		}).Error
}

func MarkLogExportJobFailed(id string, err error) error {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return LOG_DB.Model(&LogExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     LogExportJobStatusFailed,
			"error":      message,
			"updated_at": common.GetTimestamp(),
		}).Error
}

func ListExpiredLogExportJobs(now int64, limit int) ([]LogExportJob, error) {
	if limit <= 0 {
		limit = 100
	}
	var jobs []LogExportJob
	err := LOG_DB.Where("expires_at > 0 AND expires_at < ?", now).
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

func DeleteLogExportJob(id string) error {
	return LOG_DB.Where("id = ?", id).Delete(&LogExportJob{}).Error
}
