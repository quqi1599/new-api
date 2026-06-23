package controller

import (
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const tokenLogExportFileTTLSeconds int64 = 24 * 60 * 60

var tokenLogExportCleanupSlots = make(chan struct{}, 1)

type tokenLogExportJobRequest struct {
	StartTimestamp int64 `json:"start_timestamp"`
	EndTimestamp   int64 `json:"end_timestamp"`
}

type tokenLogExportJobResponse struct {
	Id             string  `json:"id"`
	Status         string  `json:"status"`
	StartTimestamp int64   `json:"start_timestamp"`
	EndTimestamp   int64   `json:"end_timestamp"`
	Total          int64   `json:"total"`
	Exported       int64   `json:"exported"`
	Progress       float64 `json:"progress"`
	FileName       string  `json:"filename,omitempty"`
	FileSize       int64   `json:"file_size,omitempty"`
	Error          string  `json:"error,omitempty"`
	CreatedAt      int64   `json:"created_at"`
	UpdatedAt      int64   `json:"updated_at"`
	ExpiresAt      int64   `json:"expires_at"`
	DownloadURL    string  `json:"download_url,omitempty"`
}

func CreateTokenLogExportJob(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	userId := c.GetInt("id")
	if tokenId == 0 {
		common.ApiErrorMsg(c, "无效的令牌")
		return
	}

	startTimestamp, endTimestamp, err := getTokenLogExportRangeForJob(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	activeJob, err := model.GetActiveLogExportJobByTokenId(tokenId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if activeJob != nil {
		common.ApiSuccess(c, buildTokenLogExportJobResponse(activeJob))
		return
	}

	job, err := model.CreateLogExportJob(userId, tokenId, startTimestamp, endTimestamp, common.GetTimestamp()+tokenLogExportFileTTLSeconds)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	cleanupExpiredTokenLogExportJobs()
	go runTokenLogExportJob(job.Id)
	common.ApiSuccess(c, buildTokenLogExportJobResponse(job))
}

func GetTokenLogExportJob(c *gin.Context) {
	job, ok := getOwnedTokenLogExportJob(c)
	if !ok {
		return
	}
	common.ApiSuccess(c, buildTokenLogExportJobResponse(job))
}

func DownloadTokenLogExportJob(c *gin.Context) {
	jobId := strings.TrimSpace(c.Param("id"))
	downloadToken := strings.TrimSpace(c.Query("download_token"))
	if len(jobId) == 0 || len(jobId) > 64 || downloadToken == "" {
		common.ApiErrorMsg(c, "导出任务不存在")
		return
	}
	job, err := model.GetLogExportJobById(jobId)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && job.DownloadToken != downloadToken) {
		common.ApiErrorMsg(c, "导出任务不存在")
		return
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if job.Status != model.LogExportJobStatusSucceeded {
		common.ApiErrorMsg(c, "导出任务尚未完成")
		return
	}
	if job.ExpiresAt > 0 && job.ExpiresAt < common.GetTimestamp() {
		common.ApiErrorMsg(c, "导出文件已过期，请重新创建导出任务")
		return
	}
	if job.FilePath == "" || !isTokenLogExportFilePathAllowed(job.FilePath) {
		common.ApiErrorMsg(c, "导出文件路径无效")
		return
	}
	if _, err := os.Stat(job.FilePath); err != nil {
		common.ApiErrorMsg(c, "导出文件不存在，请重新创建导出任务")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, job.FileName))
	c.File(job.FilePath)
}

func runTokenLogExportJob(jobId string) {
	tokenLogExportSlots <- struct{}{}
	defer releaseTokenLogExportSlot()

	job, err := model.GetLogExportJobById(jobId)
	if err != nil {
		common.SysError("failed to load token log export job: " + err.Error())
		return
	}
	if job.Status != model.LogExportJobStatusQueued {
		return
	}
	if err := writeTokenLogExportJob(job); err != nil {
		common.SysError("failed to export token logs: " + err.Error())
		_ = model.MarkLogExportJobFailed(job.Id, err)
	}
}

func writeTokenLogExportJob(job *model.LogExportJob) error {
	total, err := model.CountLogByTokenIdRange(job.TokenId, job.StartTimestamp, job.EndTimestamp)
	if err != nil {
		return err
	}
	if err := model.MarkLogExportJobRunning(job.Id, total); err != nil {
		return err
	}

	exportDir, err := getTokenLogExportDir()
	if err != nil {
		return err
	}
	fileName := fmt.Sprintf("token-logs-%d-%d-%d.csv.gz", job.TokenId, job.StartTimestamp, job.EndTimestamp)
	tmpPath := filepath.Join(exportDir, job.Id+".csv.gz.tmp")
	finalPath := filepath.Join(exportDir, job.Id+".csv.gz")
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	defer closeFile()

	gzipWriter := gzip.NewWriter(file)
	csvWriter := csv.NewWriter(gzipWriter)
	if err := csvWriter.Write(tokenLogCsvHeader()); err != nil {
		_ = gzipWriter.Close()
		return err
	}

	var exported int64
	beforeId := 0
	for {
		logs, nextBeforeId, err := model.GetLogByTokenIdCursor(nil, job.TokenId, job.StartTimestamp, job.EndTimestamp, beforeId, tokenLogExportBatchSize, int(exported))
		if err != nil {
			_ = gzipWriter.Close()
			return err
		}
		for _, log := range logs {
			if err := csvWriter.Write(tokenLogCsvRow(log)); err != nil {
				_ = gzipWriter.Close()
				return err
			}
		}
		exported += int64(len(logs))
		csvWriter.Flush()
		if err := csvWriter.Error(); err != nil {
			_ = gzipWriter.Close()
			return err
		}
		if err := model.UpdateLogExportJobProgress(job.Id, exported); err != nil {
			_ = gzipWriter.Close()
			return err
		}
		if len(logs) < tokenLogExportBatchSize || nextBeforeId == 0 {
			break
		}
		beforeId = nextBeforeId
	}

	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		_ = gzipWriter.Close()
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	if err := closeFile(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	stat, err := os.Stat(finalPath)
	if err != nil {
		return err
	}
	if err := model.MarkLogExportJobSucceeded(job.Id, finalPath, fileName, stat.Size(), exported); err != nil {
		return err
	}
	success = true
	return nil
}

func getTokenLogExportRangeForJob(c *gin.Context) (int64, int64, error) {
	if c.Query("start_timestamp") != "" || c.Query("end_timestamp") != "" {
		return getTokenLogExportRange(c)
	}
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return getTokenLogExportRange(c)
	}

	var req tokenLogExportJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return 0, 0, fmt.Errorf("请求参数无效")
	}
	return normalizeTokenLogExportRange(req.StartTimestamp, req.EndTimestamp)
}

func getOwnedTokenLogExportJob(c *gin.Context) (*model.LogExportJob, bool) {
	tokenId := c.GetInt("token_id")
	if tokenId == 0 {
		common.ApiErrorMsg(c, "无效的令牌")
		return nil, false
	}
	jobId := strings.TrimSpace(c.Param("id"))
	if len(jobId) == 0 || len(jobId) > 64 {
		common.ApiErrorMsg(c, "导出任务不存在")
		return nil, false
	}
	job, err := model.GetLogExportJobById(jobId)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && job.TokenId != tokenId) {
		common.ApiErrorMsg(c, "导出任务不存在")
		return nil, false
	}
	if err != nil {
		common.ApiError(c, err)
		return nil, false
	}
	return job, true
}

func buildTokenLogExportJobResponse(job *model.LogExportJob) tokenLogExportJobResponse {
	progress := 0.0
	if job.Total > 0 {
		progress = float64(job.Exported) / float64(job.Total)
		if progress > 1 {
			progress = 1
		}
	} else if job.Status == model.LogExportJobStatusSucceeded {
		progress = 1
	}

	resp := tokenLogExportJobResponse{
		Id:             job.Id,
		Status:         job.Status,
		StartTimestamp: job.StartTimestamp,
		EndTimestamp:   job.EndTimestamp,
		Total:          job.Total,
		Exported:       job.Exported,
		Progress:       progress,
		FileName:       job.FileName,
		FileSize:       job.FileSize,
		Error:          job.Error,
		CreatedAt:      job.CreatedAt,
		UpdatedAt:      job.UpdatedAt,
		ExpiresAt:      job.ExpiresAt,
	}
	if job.Status == model.LogExportJobStatusSucceeded {
		resp.DownloadURL = fmt.Sprintf("/api/log/token/export-jobs/%s/download?download_token=%s", job.Id, job.DownloadToken)
	}
	return resp
}

func getTokenLogExportDir() (string, error) {
	dir := strings.TrimSpace(os.Getenv("LOG_EXPORT_DIR"))
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "new-api-log-exports")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absDir, 0750); err != nil {
		return "", err
	}
	return absDir, nil
}

func isTokenLogExportFilePathAllowed(path string) bool {
	exportDir, err := getTokenLogExportDir()
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(exportDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func cleanupExpiredTokenLogExportJobs() {
	select {
	case tokenLogExportCleanupSlots <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() {
			<-tokenLogExportCleanupSlots
		}()
		jobs, err := model.ListExpiredLogExportJobs(common.GetTimestamp(), 100)
		if err != nil {
			common.SysError("failed to list expired token log export jobs: " + err.Error())
			return
		}
		for _, job := range jobs {
			if job.FilePath != "" && isTokenLogExportFilePathAllowed(job.FilePath) {
				_ = os.Remove(job.FilePath)
			}
			if err := model.DeleteLogExportJob(job.Id); err != nil {
				common.SysError("failed to delete expired token log export job: " + err.Error())
			}
		}
	}()
}
