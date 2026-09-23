package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestConvertedResponsesDeliveryFailurePreservesPartialUsage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowWrites int
	}{
		{"text_delta", 1},
		{"stop_chunk", 2},
		{"usage_chunk", 3},
		{"done_marker", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: tc.allowWrites}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{IsStream: true, ShouldIncludeUsage: true,
				RelayFormat: types.RelayFormatOpenAI,
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
			body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
			usage, err := OaiResponsesToChatStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
			if err == nil || err.StatusCode != 499 || !types.IsSkipRetryError(err) || types.IsChannelPenaltyAllowed(err) {
				t.Fatalf("delivery failure must not blame upstream: %v", err)
			}
			if usage == nil || info.PartialStreamError != err {
				t.Fatalf("partial usage lost: usage=%v marker=%v err=%v", usage, info.PartialStreamError, err)
			}
			if w.writes != tc.allowWrites+1 {
				t.Fatalf("wrote after delivery failure: %d writes", w.writes)
			}
			if info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
				t.Fatalf("delivery failure retained successful stream terminal: %s", info.StreamStatus.EndReason)
			}
		})
	}
}
