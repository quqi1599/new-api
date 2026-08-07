package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWriteRelayErrorSuppressesJSONAfterSSEOutput(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.NoError(t, helper.StringData(c, `{"ok":true}`))
	before := recorder.Body.String()

	relayErr := types.NewOpenAIError(errors.New("upstream stream failed"), types.ErrorCodeBadResponse, http.StatusBadGateway)
	writeRelayError(c, nil, types.RelayFormatOpenAI, relayErr)

	require.Equal(t, before, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), `"error"`)
}

func TestWriteRelayErrorRestoresJSONHeadersBeforeFirstOutput(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	helper.SetEventStreamHeaders(c)

	relayErr := types.NewOpenAIError(errors.New("upstream stream failed"), types.ErrorCodeBadResponse, http.StatusBadGateway)
	writeRelayError(c, nil, types.RelayFormatOpenAI, relayErr)

	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.NotContains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, recorder.Body.String(), `"error"`)
}

func TestWriteRelayErrorAddsRetryAfterForAuthUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	relayErr := types.WithOpenAIError(
		types.OpenAIError{
			Message: "requested route is temporarily unavailable",
			Type:    "upstream_error",
			Code:    string(types.ErrorCodeAuthUnavailable),
		},
		http.StatusServiceUnavailable,
		types.ErrOptionWithUpstreamResponse(),
	)

	writeRelayError(c, nil, types.RelayFormatOpenAI, relayErr)

	require.Equal(t, authUnavailableRetryAfterSeconds, recorder.Header().Get("Retry-After"))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestWriteRelayErrorDoesNotAddRetryAfterForOtherErrors(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	relayErr := types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponse, http.StatusBadGateway)

	writeRelayError(c, nil, types.RelayFormatOpenAI, relayErr)

	require.Empty(t, recorder.Header().Get("Retry-After"))
}
