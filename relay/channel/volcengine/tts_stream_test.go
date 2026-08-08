package volcengine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func newVolcengineTTSTestContext() (*gin.Context, *httptest.ResponseRecorder, *relaycommon.RelayInfo) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "app-id|access-token"},
	}
	return c, recorder, info
}

func newVolcengineTTSServer(t *testing.T, afterRequest func(*websocket.Conn) error) string {
	t.Helper()
	serverErr := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			serverErr <- err
			return
		}
		serverErr <- afterRequest(conn)
	}))
	t.Cleanup(func() {
		server.Close()
		require.NoError(t, <-serverErr)
	})
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func TestVolcengineTTSRequiresExplicitNegativeSequenceTerminal(t *testing.T) {
	requestURL := newVolcengineTTSServer(t, func(conn *websocket.Conn) error {
		return conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "truncated"),
			time.Now().Add(time.Second),
		)
	})
	c, recorder, info := newVolcengineTTSTestContext()

	usage, streamErr := handleTTSWebSocketResponse(c, requestURL, VolcengineTTSRequest{}, info, "mp3")

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, types.ErrorCodeUpstreamStreamIncomplete, streamErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(streamErr))
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
}

func TestVolcengineTTSSucceedsOnlyOnNegativeSequence(t *testing.T) {
	requestURL := newVolcengineTTSServer(t, func(conn *websocket.Conn) error {
		message, err := NewMessage(MsgTypeAudioOnlyServer, MsgTypeFlagNegativeSeq)
		if err != nil {
			return err
		}
		message.Sequence = -1
		message.Payload = []byte("audio-bytes")
		frame, err := message.Marshal()
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, frame)
	})
	c, recorder, info := newVolcengineTTSTestContext()

	usage, streamErr := handleTTSWebSocketResponse(c, requestURL, VolcengineTTSRequest{}, info, "mp3")

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, "audio-bytes", recorder.Body.String())
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
}

func TestVolcengineTTSUsesSharedFirstEventDeadline(t *testing.T) {
	requestURL := newVolcengineTTSServer(t, func(*websocket.Conn) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	c, recorder, info := newVolcengineTTSTestContext()
	info.SetFirstValidEventDeadline(time.Now().Add(30 * time.Millisecond))

	usage, streamErr := handleTTSWebSocketResponse(c, requestURL, VolcengineTTSRequest{}, info, "mp3")

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusGatewayTimeout, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(streamErr))
	require.Equal(t, relaycommon.StreamEndReasonFirstEventTimeout, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
}

func TestVolcengineTTSEmptyNonTerminalFrameDoesNotSatisfyFirstEvent(t *testing.T) {
	requestURL := newVolcengineTTSServer(t, func(conn *websocket.Conn) error {
		message, err := NewMessage(MsgTypeAudioOnlyServer, 0)
		if err != nil {
			return err
		}
		message.Sequence = 1
		frame, err := message.Marshal()
		if err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	c, recorder, info := newVolcengineTTSTestContext()
	info.SetFirstValidEventDeadline(time.Now().Add(30 * time.Millisecond))

	usage, streamErr := handleTTSWebSocketResponse(c, requestURL, VolcengineTTSRequest{}, info, "mp3")

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusGatewayTimeout, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.Zero(t, info.ReceivedResponseCount)
	require.Empty(t, recorder.Body.String())
}

func TestVolcengineTTSHandshakeConsumesAbsoluteFirstEventBudget(t *testing.T) {
	previous := common.RelayFirstEventTotalTimeout
	common.RelayFirstEventTotalTimeout = 1
	t.Cleanup(func() { common.RelayFirstEventTotalTimeout = previous })

	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	c, recorder, info := newVolcengineTTSTestContext()
	info.StartTime = time.Now().Add(-750 * time.Millisecond)
	startedAt := time.Now()

	usage, streamErr := handleTTSWebSocketResponse(
		c,
		"ws"+strings.TrimPrefix(upstream.URL, "http"),
		VolcengineTTSRequest{},
		info,
		"mp3",
	)

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusGatewayTimeout, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(streamErr))
	require.False(t, types.IsChannelPenaltyAllowed(streamErr))
	require.Less(t, time.Since(startedAt), 750*time.Millisecond)
	require.Empty(t, recorder.Body.String())
}
