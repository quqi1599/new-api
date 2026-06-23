package controller

import (
	"compress/gzip"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func seedTokenLogsForGetLogByKey(t *testing.T, tokenId int) {
	t.Helper()

	logs := []model.Log{
		{UserId: 1, TokenId: tokenId, CreatedAt: 10, Type: model.LogTypeConsume, Content: "older"},
		{UserId: 1, TokenId: tokenId, CreatedAt: 20, Type: model.LogTypeConsume, Content: "middle"},
		{UserId: 1, TokenId: tokenId + 1, CreatedAt: 30, Type: model.LogTypeConsume, Content: "other-token"},
		{UserId: 1, TokenId: tokenId, CreatedAt: 30, Type: model.LogTypeConsume, Content: "newer"},
		{UserId: 1, TokenId: tokenId, CreatedAt: 40, Type: model.LogTypeConsume, Content: "newest"},
	}
	if err := model.LOG_DB.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed token logs: %v", err)
	}
}

func performGetLogByKey(t *testing.T, target string, tokenId int) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	c.Set("token_id", tokenId)
	GetLogByKey(c)
	return w
}

func performExportLogByKey(t *testing.T, target string, tokenId int) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	c.Set("token_id", tokenId)
	ExportLogByKey(c)
	return w
}

func performCreateTokenLogExportJob(t *testing.T, target string, tokenId int) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, target, nil)
	c.Set("token_id", tokenId)
	c.Set("id", 1)
	CreateTokenLogExportJob(c)
	return w
}

func performGetTokenLogExportJob(t *testing.T, jobId string, tokenId int) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/log/token/export-jobs/"+jobId, nil)
	c.Params = gin.Params{{Key: "id", Value: jobId}}
	c.Set("token_id", tokenId)
	GetTokenLogExportJob(c)
	return w
}

func performDownloadTokenLogExportJob(t *testing.T, jobId string, downloadToken string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/log/token/export-jobs/"+jobId+"/download?download_token="+downloadToken, nil)
	c.Params = gin.Params{{Key: "id", Value: jobId}}
	DownloadTokenLogExportJob(c)
	return w
}

func waitForLogExportJobStatus(t *testing.T, jobId string, status string) *model.LogExportJob {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := model.GetLogExportJobById(jobId)
		if err != nil {
			t.Fatalf("failed to load export job: %v", err)
		}
		if job.Status == status {
			return job
		}
		if job.Status == model.LogExportJobStatusFailed {
			t.Fatalf("export job failed: %s", job.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := model.GetLogExportJobById(jobId)
	t.Fatalf("timed out waiting for export job %s, last job: %+v", status, job)
	return nil
}

func TestGetLogByKeyKeepsLegacyArrayResponse(t *testing.T) {
	setupTokenControllerTestDB(t)
	seedTokenLogsForGetLogByKey(t, 7)

	w := performGetLogByKey(t, "/api/log/token", 7)
	if w.Code != http.StatusOK {
		t.Fatalf("expected http 200, got %d", w.Code)
	}

	var response struct {
		Success bool        `json:"success"`
		Data    []model.Log `json:"data"`
	}
	if err := common.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("expected success response")
	}
	if len(response.Data) != 4 {
		t.Fatalf("expected 4 logs, got %d", len(response.Data))
	}
	if response.Data[0].Content != "newest" || response.Data[0].Id != 1 {
		t.Fatalf("unexpected first legacy log: %+v", response.Data[0])
	}
}

func TestGetLogByKeySupportsPagedRangeResponse(t *testing.T) {
	setupTokenControllerTestDB(t)
	seedTokenLogsForGetLogByKey(t, 7)

	w := performGetLogByKey(t, "/api/log/token?p=2&page_size=2&start_timestamp=20&end_timestamp=40", 7)
	if w.Code != http.StatusOK {
		t.Fatalf("expected http 200, got %d", w.Code)
	}

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Page     int         `json:"page"`
			PageSize int         `json:"page_size"`
			Total    int         `json:"total"`
			Items    []model.Log `json:"items"`
		} `json:"data"`
	}
	if err := common.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("expected success response")
	}
	if response.Data.Page != 2 || response.Data.PageSize != 2 || response.Data.Total != 3 {
		t.Fatalf("unexpected page metadata: %+v", response.Data)
	}
	if len(response.Data.Items) != 1 {
		t.Fatalf("expected 1 log on second page, got %d", len(response.Data.Items))
	}
	if response.Data.Items[0].Content != "middle" || response.Data.Items[0].Id != 3 {
		t.Fatalf("unexpected paged log: %+v", response.Data.Items[0])
	}
}

func TestExportLogByKeyStreamsCSVForMatchingRange(t *testing.T) {
	setupTokenControllerTestDB(t)
	seedTokenLogsForGetLogByKey(t, 7)

	w := performExportLogByKey(t, "/api/log/token/export?start_timestamp=20&end_timestamp=40", 7)
	if w.Code != http.StatusOK {
		t.Fatalf("expected http 200, got %d", w.Code)
	}
	if contentType := w.Header().Get("Content-Type"); !strings.Contains(contentType, "text/csv") {
		t.Fatalf("expected csv content type, got %q", contentType)
	}

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("failed to read csv response: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected header plus 3 rows, got %d rows: %#v", len(rows), rows)
	}
	if rows[0][0] != "id" || rows[0][3] != "content" {
		t.Fatalf("unexpected csv header: %#v", rows[0])
	}
	if rows[1][3] != "newest" || rows[2][3] != "newer" || rows[3][3] != "middle" {
		t.Fatalf("unexpected csv content order: %#v", rows)
	}
	for _, row := range rows[1:] {
		if row[3] == "other-token" {
			t.Fatalf("csv response included another token's log: %#v", rows)
		}
	}
}

func TestExportLogByKeyRejectsTooLargeRange(t *testing.T) {
	w := performExportLogByKey(t, "/api/log/token/export?start_timestamp=1&end_timestamp=2678402", 7)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected http 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "导出时间范围不能超过31天") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestExportLogByKeyRejectsWhenExportSlotsAreFull(t *testing.T) {
	for i := 0; i < tokenLogExportMaxConcurrent; i++ {
		if !acquireTokenLogExportSlot() {
			t.Fatalf("failed to fill export slot %d", i)
		}
		defer releaseTokenLogExportSlot()
	}

	w := performExportLogByKey(t, "/api/log/token/export?start_timestamp=20&end_timestamp=40", 7)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected http 429, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "当前导出任务较多") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestCreateTokenLogExportJobWritesGzipCSV(t *testing.T) {
	setupTokenControllerTestDB(t)
	t.Setenv("LOG_EXPORT_DIR", t.TempDir())
	seedTokenLogsForGetLogByKey(t, 7)

	w := performCreateTokenLogExportJob(t, "/api/log/token/export-jobs?start_timestamp=20&end_timestamp=40", 7)
	if w.Code != http.StatusOK {
		t.Fatalf("expected http 200, got %d: %s", w.Code, w.Body.String())
	}

	var response struct {
		Success bool                      `json:"success"`
		Data    tokenLogExportJobResponse `json:"data"`
	}
	if err := common.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Success || response.Data.Id == "" || response.Data.Status != model.LogExportJobStatusQueued {
		t.Fatalf("unexpected create response: %+v", response)
	}

	job := waitForLogExportJobStatus(t, response.Data.Id, model.LogExportJobStatusSucceeded)
	if job.Total != 3 || job.Exported != 3 {
		t.Fatalf("unexpected job counts: %+v", job)
	}
	if !strings.HasSuffix(job.FileName, ".csv.gz") {
		t.Fatalf("expected gzip csv filename, got %q", job.FileName)
	}

	file, err := os.Open(job.FilePath)
	if err != nil {
		t.Fatalf("failed to open export file: %v", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("failed to open gzip reader: %v", err)
	}
	defer gzipReader.Close()
	rows, err := csv.NewReader(gzipReader).ReadAll()
	if err != nil {
		t.Fatalf("failed to read gzip csv: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected header plus 3 rows, got %d rows: %#v", len(rows), rows)
	}
	if rows[1][3] != "newest" || rows[2][3] != "newer" || rows[3][3] != "middle" {
		t.Fatalf("unexpected csv content order: %#v", rows)
	}
}

func TestTokenLogExportJobStatusAndDownloadAreTokenScoped(t *testing.T) {
	setupTokenControllerTestDB(t)
	t.Setenv("LOG_EXPORT_DIR", t.TempDir())
	seedTokenLogsForGetLogByKey(t, 7)

	w := performCreateTokenLogExportJob(t, "/api/log/token/export-jobs?start_timestamp=20&end_timestamp=40", 7)
	var response struct {
		Success bool                      `json:"success"`
		Data    tokenLogExportJobResponse `json:"data"`
	}
	if err := common.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	job := waitForLogExportJobStatus(t, response.Data.Id, model.LogExportJobStatusSucceeded)

	statusResp := performGetTokenLogExportJob(t, job.Id, 7)
	if statusResp.Code != http.StatusOK || !strings.Contains(statusResp.Body.String(), model.LogExportJobStatusSucceeded) {
		t.Fatalf("unexpected status response: code=%d body=%s", statusResp.Code, statusResp.Body.String())
	}

	downloadResp := performDownloadTokenLogExportJob(t, job.Id, job.DownloadToken)
	if downloadResp.Code != http.StatusOK {
		t.Fatalf("expected download http 200, got %d: %s", downloadResp.Code, downloadResp.Body.String())
	}
	if contentType := downloadResp.Header().Get("Content-Type"); !strings.Contains(contentType, "application/gzip") {
		t.Fatalf("expected gzip content type, got %q", contentType)
	}

	wrongDownloadTokenResp := performDownloadTokenLogExportJob(t, job.Id, "wrong-token")
	if !strings.Contains(wrongDownloadTokenResp.Body.String(), "导出任务不存在") {
		t.Fatalf("expected download token denial, got %s", wrongDownloadTokenResp.Body.String())
	}
}
