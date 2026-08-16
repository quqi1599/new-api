package helper

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestGetAndValidateAlphaSearchRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(body string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		return c
	}

	raw := "{\n  \"model\": \"gpt-5.5\", \"query\": \"weather\", \"future\": true\n}"
	request, err := GetAndValidateAlphaSearchRequest(newContext(raw))
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "gpt-5.5" {
		t.Fatalf("model = %q", request.Model)
	}
	if !bytes.Equal(request.RawBody, []byte(raw)) {
		t.Fatalf("raw body changed: got %s want %s", request.RawBody, raw)
	}

	if _, err := GetAndValidateAlphaSearchRequest(newContext(`{"query":"weather"}`)); err == nil {
		t.Fatal("expected missing model error")
	} else {
		var relayErr *types.NewAPIError
		if !errors.As(err, &relayErr) {
			t.Fatalf("missing model error type = %T, want *types.NewAPIError", err)
		}
		if relayErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("missing model status = %d, want %d", relayErr.StatusCode, http.StatusBadRequest)
		}
		if relayErr.GetErrorCode() != types.ErrorCodeInvalidRequest {
			t.Fatalf("missing model error code = %q, want %q", relayErr.GetErrorCode(), types.ErrorCodeInvalidRequest)
		}
		if !types.IsSkipRetryError(relayErr) {
			t.Fatal("missing model error must skip retry")
		}
	}
	if _, err := GetAndValidateAlphaSearchRequest(newContext(`{"model":`)); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}
