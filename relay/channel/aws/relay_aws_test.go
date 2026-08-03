package aws

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrockruntimeTypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type awsFailAfterWriteWriter struct {
	gin.ResponseWriter
	err error
}

func (w *awsFailAfterWriteWriter) Write(data []byte) (int, error) {
	n, _ := w.ResponseWriter.Write(data)
	return n, w.err
}

func (w *awsFailAfterWriteWriter) WriteString(data string) (int, error) {
	n, _ := w.ResponseWriter.WriteString(data)
	return n, w.err
}

type fakeAwsResponseStream struct {
	events <-chan bedrockruntimeTypes.ResponseStream
	err    error
	closed atomic.Bool
}

func (s *fakeAwsResponseStream) Events() <-chan bedrockruntimeTypes.ResponseStream { return s.events }
func (s *fakeAwsResponseStream) Err() error                                        { return s.err }
func (s *fakeAwsResponseStream) Close() error {
	s.closed.Store(true)
	return nil
}

func newAwsStreamTestContext() (*gin.Context, *httptest.ResponseRecorder, *relaycommon.RelayInfo, *claude.ClaudeResponseInfo) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat:  types.RelayFormatOpenAI,
		StreamStatus: relaycommon.NewStreamStatus(),
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-test",
		},
	}
	claudeInfo := &claude.ClaudeResponseInfo{
		Model: "claude-test",
		Usage: &dto.Usage{},
	}
	return c, recorder, info, claudeInfo
}

func TestNewAwsInvokeContextFollowsInboundCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRelayTimeout := common.RelayTimeout
	common.RelayTimeout = 30
	t.Cleanup(func() {
		common.RelayTimeout = previousRelayTimeout
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancelRequest := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(requestContext)

	invokeContext, cancelInvoke := newAwsInvokeContext(c)
	defer cancelInvoke()
	_, hasDeadline := invokeContext.Deadline()
	require.False(t, hasDeadline, "legacy RELAY_TIMEOUT must not cap the whole Bedrock response")
	cancelRequest()

	select {
	case <-invokeContext.Done():
		require.ErrorIs(t, invokeContext.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("AWS invoke context did not follow inbound cancellation")
	}
}

func TestDoAwsClientRequest_AppliesRuntimeHeaderOverrideToAnthropicBeta(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName:           "claude-3-5-sonnet-20240620",
		IsStream:                  false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"anthropic-beta": "computer-use-2025-01-24",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            "access-key|secret-key|us-east-1",
			UpstreamModelName: "claude-3-5-sonnet-20240620",
		},
	}

	requestBody := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":128}`)
	adaptor := &Adaptor{}

	_, err := doAwsClientRequest(ctx, info, adaptor, requestBody)
	require.NoError(t, err)
	require.Equal(t, 1, adaptor.AwsClient.Options().RetryMaxAttempts,
		"Bedrock generation retries must be decided by NewAPI only after proving the request was not sent")

	awsReq, ok := adaptor.AwsReq.(*bedrockruntime.InvokeModelInput)
	require.True(t, ok)

	var payload map[string]any
	require.NoError(t, common.Unmarshal(awsReq.Body, &payload))

	anthropicBeta, exists := payload["anthropic_beta"]
	require.True(t, exists)

	values, ok := anthropicBeta.([]any)
	require.True(t, ok)
	require.Equal(t, []any{"computer-use-2025-01-24"}, values)
}

func TestConsumeAwsResponseStreamFirstEventTimeout(t *testing.T) {
	oldFirstEventTimeout := constant.RelayFirstEventTimeout
	constant.RelayFirstEventTimeout = 1
	t.Cleanup(func() { constant.RelayFirstEventTimeout = oldFirstEventTimeout })

	events := make(chan bedrockruntimeTypes.ResponseStream)
	stream := &fakeAwsResponseStream{events: events}
	c, _, info, claudeInfo := newAwsStreamTestContext()

	streamErr, _ := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusGatewayTimeout, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.Equal(t, relaycommon.StreamEndReasonFirstEventTimeout, info.StreamStatus.EndReason)
	require.True(t, stream.closed.Load())
}

func TestConsumeAwsResponseStreamUsesRemainingRequestWideBudget(t *testing.T) {
	oldFirstEventTimeout := constant.RelayFirstEventTimeout
	constant.RelayFirstEventTimeout = 5
	t.Cleanup(func() { constant.RelayFirstEventTimeout = oldFirstEventTimeout })

	events := make(chan bedrockruntimeTypes.ResponseStream)
	stream := &fakeAwsResponseStream{events: events}
	c, _, info, claudeInfo := newAwsStreamTestContext()
	info.SetFirstValidEventDeadline(time.Now().Add(100 * time.Millisecond))

	started := time.Now()
	streamErr, _ := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.NotNil(t, streamErr)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.Less(t, time.Since(started), time.Second)
	require.True(t, stream.closed.Load())
}

func TestConsumeAwsResponseStreamRejectsCleanEOFMissingMessageStop(t *testing.T) {
	events := make(chan bedrockruntimeTypes.ResponseStream)
	close(events)
	stream := &fakeAwsResponseStream{events: events}
	c, recorder, info, claudeInfo := newAwsStreamTestContext()

	streamErr, _ := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamStreamIncomplete, streamErr.GetErrorCode())
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
}

func TestConsumeAwsResponseStreamCompletesOnlyOnMessageStop(t *testing.T) {
	events := make(chan bedrockruntimeTypes.ResponseStream, 1)
	events <- &bedrockruntimeTypes.ResponseStreamMemberChunk{
		Value: bedrockruntimeTypes.PayloadPart{Bytes: []byte(`{"type":"message_stop"}`)},
	}
	close(events)
	stream := &fakeAwsResponseStream{events: events}
	c, recorder, info, claudeInfo := newAwsStreamTestContext()

	streamErr, usage := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Contains(t, recorder.Body.String(), "[DONE]")
}

func TestConsumeAwsResponseStreamPartialEOFEstimatesUsageWithoutDone(t *testing.T) {
	events := make(chan bedrockruntimeTypes.ResponseStream, 2)
	events <- &bedrockruntimeTypes.ResponseStreamMemberChunk{
		Value: bedrockruntimeTypes.PayloadPart{Bytes: []byte(`{"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":1}}}`)},
	}
	events <- &bedrockruntimeTypes.ResponseStreamMemberChunk{
		Value: bedrockruntimeTypes.PayloadPart{Bytes: []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello from a partial stream"}}`)},
	}
	close(events)
	stream := &fakeAwsResponseStream{events: events}
	c, recorder, info, claudeInfo := newAwsStreamTestContext()

	streamErr, usage := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.Nil(t, streamErr, "after output is committed the handler must preserve the partial stream instead of appending JSON")
	require.NotNil(t, usage)
	require.Greater(t, usage.CompletionTokens, 0, "partial output must not settle as zero completion tokens")
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.NotEmpty(t, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "[DONE]", "clean EOF without message_stop must never be presented as successful")
}

func TestConsumeAwsResponseStreamReturnsWriteFailureAfterSSECommitted(t *testing.T) {
	events := make(chan bedrockruntimeTypes.ResponseStream, 1)
	events <- &bedrockruntimeTypes.ResponseStreamMemberChunk{
		Value: bedrockruntimeTypes.PayloadPart{Bytes: []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)},
	}
	close(events)
	stream := &fakeAwsResponseStream{events: events}
	c, recorder, info, claudeInfo := newAwsStreamTestContext()
	c.Writer = &awsFailAfterWriteWriter{ResponseWriter: c.Writer, err: errors.New("downstream write failed")}

	streamErr, _ := consumeAwsResponseStream(c, info, c.Request.Context(), stream, claudeInfo)

	require.NotNil(t, streamErr, "a committed SSE response must still fail settlement when the downstream write fails")
	require.True(t, types.IsSkipRetryError(streamErr))
	require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	require.NotEmpty(t, recorder.Body.String())
}
