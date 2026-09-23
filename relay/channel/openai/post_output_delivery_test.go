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

func TestPostOutputConvertedResponsesErrorIsDeliveredInBand(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"code\":\"review_failure\",\"message\":\"broken\"}}}\n\n"
	_, err := OaiResponsesToChatStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	if err == nil {
		t.Fatal("expected terminal failure")
	}
	if !strings.Contains(recorder.Body.String(), `"error"`) {
		t.Fatalf("terminal failure swallowed after output: %s", recorder.Body.String())
	}
}

type postOutputDisconnectWriter struct {
	*httptest.ResponseRecorder
	writes      int
	allowWrites int
}

func (w *postOutputDisconnectWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.allowWrites {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func TestPostOutputDownstreamWriteErrorDoesNotPenalizeUpstream(t *testing.T) {
	w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: 1}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	body := strings.Repeat("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n", 3)
	_, err := OaiStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	if err == nil {
		t.Fatal("expected downstream failure")
	}
	if err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) {
		t.Fatalf("downstream write incorrectly counted upstream failure: status=%d penalty=%v end=%s", err.StatusCode, types.IsChannelPenaltyAllowed(err), info.StreamStatus.EndReason)
	}
}

func TestPostOutputFinalBufferedChatWriteErrorDoesNotRestoreSuccess(t *testing.T) {
	w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	if err == nil || err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) || info.PartialStreamError != err || usage == nil {
		t.Fatalf("final buffered delivery swallowed: err=%v usage=%v", err, usage)
	}
	if w.writes != 1 {
		t.Fatalf("retried delivery after failure: writes=%d", w.writes)
	}
	if info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
		t.Fatalf("final buffered delivery retained successful terminal: %s", info.StreamStatus.EndReason)
	}
}

func TestPostOutputRawResponsesWriteErrorDoesNotRestoreSuccess(t *testing.T) {
	w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: 1}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAIResponses, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	body := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	usage, err := OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	if err == nil || err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) || info.PartialStreamError != err || usage == nil {
		t.Fatalf("Responses delivery swallowed: err=%v usage=%v", err, usage)
	}
	if w.writes != 2 {
		t.Fatalf("retried delivery after failure: writes=%d", w.writes)
	}
}

func TestPostOutputChatTerminalWriteErrorsDoNotRestoreSuccess(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		name := "done"
		if includeUsage {
			name = "usage"
		}
		t.Run(name, func(t *testing.T) {
			w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: 1}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{IsStream: true, ShouldIncludeUsage: includeUsage, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
			usage, err := OaiStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
			if err == nil || err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) || info.PartialStreamError != err || usage == nil {
				t.Fatalf("terminal delivery swallowed: err=%v usage=%v", err, usage)
			}
			if w.writes != 2 {
				t.Fatalf("retried delivery after failure: writes=%d", w.writes)
			}
			if info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
				t.Fatalf("terminal delivery retained successful terminal: %s", info.StreamStatus.EndReason)
			}
		})
	}
}

func TestPostOutputDeliveryRetainsAlreadyReceivedUsage(t *testing.T) {
	for _, responses := range []bool{false, true} {
		name := "chat_usage_frame"
		if responses {
			name = "responses_completion_frame"
		}
		t.Run(name, func(t *testing.T) {
			w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: 1}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":7,\"total_tokens\":307,\"prompt_tokens_details\":{\"cached_tokens\":90}}}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":999,\"completion_tokens\":999,\"total_tokens\":1998}}\n\n"
			wantReceived := 3
			handler := OaiStreamHandler
			if responses {
				info.RelayFormat = types.RelayFormatOpenAIResponses
				body = "data: {\"type\":\"response.created\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":300,\"output_tokens\":7,\"total_tokens\":307,\"input_tokens_details\":{\"cached_tokens\":90}}}}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":999,\"output_tokens\":999,\"total_tokens\":1998}}}\n\n"
				wantReceived = 2
				handler = OaiResponsesStreamHandler
			}
			usage, err := handler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
			if err == nil || err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) || info.PartialStreamError != err {
				t.Fatalf("delivery error=%v", err)
			}
			if usage == nil || usage.PromptTokens != 300 || usage.CompletionTokens != 7 || usage.TotalTokens != 307 || usage.PromptTokensDetails.CachedTokens != 90 {
				t.Fatalf("already received usage discarded or later frame consumed: %#v", usage)
			}
			if info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone || info.ReceivedResponseCount != wantReceived || w.writes != 2 {
				t.Fatalf("continued after failed delivery: status=%s received=%d writes=%d", info.StreamStatus.EndReason, info.ReceivedResponseCount, w.writes)
			}
		})
	}
}
