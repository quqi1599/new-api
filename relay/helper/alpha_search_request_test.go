package helper

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

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
	}
	if _, err := GetAndValidateAlphaSearchRequest(newContext(`{"model":`)); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}
