package helper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesSyntheticFailureKeepsIdentityAndDoesNotDuplicateTerminal(t *testing.T) {
	for _, terminal := range []string{"", "response.completed", "response.incomplete", "response.failed"} {
		t.Run(terminal, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			require.NoError(t, ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.created"}, `{"type":"response.created","sequence_number":11,"response":{"id":"resp_stable"}}`))
			if terminal != "" {
				require.NoError(t, ResponseChunkData(c, dto.ResponsesStreamResponse{Type: terminal}, `{"type":"`+terminal+`","sequence_number":12,"response":{"id":"resp_stable"}}`))
			}
			before := recorder.Body.String()
			err := types.NewErrorWithStatusCode(errors.New("upstream disconnected"), types.ErrorCodeUpstreamStreamIncomplete, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			require.NoError(t, SendInBandStreamError(c, types.RelayFormatOpenAIResponses, err))
			if terminal != "" {
				require.Equal(t, before, recorder.Body.String())
				return
			}
			after := recorder.Body.String()
			require.Contains(t, after, "event: response.failed\n")
			for _, line := range strings.Split(after, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				root := gjson.Parse(strings.TrimPrefix(line, "data: "))
				if root.Get("type").String() != "response.failed" {
					continue
				}
				require.Equal(t, "resp_stable", root.Get("response.id").String())
				require.EqualValues(t, 12, root.Get("sequence_number").Int())
			}
			require.NoError(t, SendInBandStreamError(c, types.RelayFormatOpenAIResponses, err))
			require.Equal(t, after, recorder.Body.String())
		})
	}
}
