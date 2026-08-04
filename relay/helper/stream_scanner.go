package helper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize    = 64 << 10 // 64KB (64*1024)
	DefaultMaxScannerBufferSize = 64 << 20 // 64MB (64*1024*1024) default SSE buffer size
	DefaultStreamingTimeout     = 300 * time.Second
	DefaultPingInterval         = 10 * time.Second
	// streamWriteTimeout bounds a single blocked write to a slow client so the
	// unconditional wg.Wait() in cleanup can always finish. Without it, a slow
	// but connected client (full TCP buffer, no server WriteTimeout) could hang
	// the handler forever.
	streamWriteTimeout = 30 * time.Second
)

func NormalizeSSEPayload(data string) (payload string, done bool) {
	payload = strings.TrimSpace(data)
	for strings.HasPrefix(payload, "data:") {
		payload = strings.TrimSpace(payload[len("data:"):])
	}
	if payload == "[DONE]" {
		return "[DONE]", true
	}
	return payload, false
}

func getScannerBufferSize() int {
	if constant.StreamScannerMaxBufferMB > 0 {
		return constant.StreamScannerMaxBufferMB << 20
	}
	return DefaultMaxScannerBufferSize
}

func getStreamingTimeout() time.Duration {
	if constant.StreamingTimeout > 0 {
		return time.Duration(constant.StreamingTimeout) * time.Second
	}
	return DefaultStreamingTimeout
}

func getFirstEventTimeout() time.Duration {
	if constant.RelayFirstEventTimeout > 0 {
		return time.Duration(constant.RelayFirstEventTimeout) * time.Second
	}
	return getStreamingTimeout()
}

// StreamFirstEventTimeout exposes the shared first-valid-event budget to
// streaming adapters that do not use the line-oriented SSE scanner (for
// example AWS Bedrock's SDK event stream).
func StreamFirstEventTimeout() time.Duration {
	return getFirstEventTimeout()
}

// StreamIdleTimeout exposes the shared inter-event idle budget to non-SSE
// streaming adapters.
func StreamIdleTimeout() time.Duration {
	return getStreamingTimeout()
}

// PreOutputStreamError converts a stream that failed before its first valid
// event into a controller-visible error. Callers must check this immediately
// after StreamScannerHandler and before synthesizing usage, stop chunks, or
// [DONE]. It is non-retryable because upstream response headers prove the
// request may already have been accepted and replay could duplicate work.
func PreOutputStreamError(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if info == nil || info.StreamStatus == nil {
		return nil
	}
	if info.ReceivedResponseCount != 0 && (c == nil || c.Writer == nil || c.Writer.Written() || info.StreamStatus.EndReason == relaycommon.StreamEndReasonDone) {
		return nil
	}

	statusCode := http.StatusBadGateway
	errorCode := types.ErrorCodeUpstreamStreamIncomplete
	message := "upstream stream ended before the first valid event"
	allowChannelPenalty := true

	switch info.StreamStatus.EndReason {
	case relaycommon.StreamEndReasonFirstEventTimeout:
		statusCode = http.StatusGatewayTimeout
		errorCode = types.ErrorCodeUpstreamFirstEventTimeout
		message = "upstream returned headers but no valid stream event before the first-event timeout"
	case relaycommon.StreamEndReasonScannerErr:
		message = "upstream stream failed before the first valid event"
	case relaycommon.StreamEndReasonEOF:
		message = "upstream stream closed without a valid event or terminal marker"
	case relaycommon.StreamEndReasonPanic:
		statusCode = http.StatusInternalServerError
		message = "stream processing failed before the first valid event"
	case relaycommon.StreamEndReasonHandlerStop:
		message = "upstream stream was rejected before a valid response could be delivered"
	case relaycommon.StreamEndReasonTimeout:
		statusCode = http.StatusGatewayTimeout
		message = "upstream stream became idle before a terminal event"
	case relaycommon.StreamEndReasonPingFail:
		message = "downstream stream heartbeat failed before a response could be delivered"
	case relaycommon.StreamEndReasonClientGone:
		statusCode = 499
		message = "request canceled by client before the first valid event"
		allowChannelPenalty = false
	case relaycommon.StreamEndReasonDone:
		// An intentionally empty response is valid only with an explicit marker.
		return nil
	default:
		return nil
	}

	options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
	if allowChannelPenalty {
		options = append(options, types.ErrOptionWithChannelPenalty())
	}
	return types.NewErrorWithStatusCode(fmt.Errorf("%s", message), errorCode, statusCode, options...)
}

// ShouldFinalizeStream prevents adapters from manufacturing a successful
// terminal chunk after a timeout, cancellation, scanner failure, ping failure,
// panic, handler error, or clean EOF without the adapter's protocol-specific
// terminal frame.
func ShouldFinalizeStream(info *relaycommon.RelayInfo) bool {
	if info == nil || info.StreamStatus == nil {
		return true
	}
	switch info.StreamStatus.EndReason {
	case relaycommon.StreamEndReasonDone:
		return true
	default:
		return false
	}
}

func NewStreamScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, InitialScannerBufferSize), getScannerBufferSize())
	return scanner
}

// StreamFrame is a protocol-neutral unit emitted by a line decoder. Data is
// the payload consumed by the provider adapter; Kind/Event preserve protocol
// metadata for NDJSON and event-aware SSE variants.
type StreamFrame struct {
	Kind  string
	Event string
	Data  string
}

// StreamLineDecoder turns physical lines into zero or more logical protocol
// frames. Flush is called after a clean scanner EOF so event-aware decoders can
// emit a final frame that is not followed by a blank line.
type StreamLineDecoder interface {
	Feed(line string) ([]StreamFrame, error)
	Flush() ([]StreamFrame, error)
}

type sseDataLineDecoder struct{}

func (sseDataLineDecoder) Feed(line string) ([]StreamFrame, error) {
	trimmedLine := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmedLine, "data:") && !strings.HasPrefix(trimmedLine, "[DONE]") {
		return nil, nil
	}
	payload, _ := NormalizeSSEPayload(trimmedLine)
	if payload == "" {
		return nil, nil
	}
	return []StreamFrame{{Kind: "data", Data: payload}}, nil
}

func (sseDataLineDecoder) Flush() ([]StreamFrame, error) { return nil, nil }

// ExtendWriteDeadline pushes the connection write deadline forward before each
// stream write. Best-effort: writers that don't support deadlines (e.g.
// httptest recorders) are silently ignored.
func ExtendWriteDeadline(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
}

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult)) {
	if dataHandler == nil {
		return
	}
	StreamScannerHandlerWithDecoder(c, resp, info, sseDataLineDecoder{}, func(frame StreamFrame, sr *StreamResult) {
		dataHandler(frame.Data, sr)
	})
}

// StreamScannerHandlerWithDecoder applies the same cancellation, first-event,
// idle-timeout, heartbeat, write-serialization, and cleanup state machine to
// protocols that are not plain data-only SSE (for example NDJSON or SSE event
// records). Only frames explicitly accepted by the adapter advance timers.
func StreamScannerHandlerWithDecoder(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, decoder StreamLineDecoder, dataHandler func(frame StreamFrame, sr *StreamResult)) {
	if resp == nil || decoder == nil || dataHandler == nil {
		return
	}
	if resp.Body == nil {
		return
	}

	previousStreamStatus := info.StreamStatus
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.CopyErrorsFrom(previousStreamStatus)

	ctx, cancel := context.WithCancel(c.Request.Context())

	streamingTimeout := getStreamingTimeout()
	firstEventTimeout := info.BoundFirstValidEventWait(getFirstEventTimeout())

	var (
		stopChan            = make(chan bool, 3) // 增加缓冲区避免阻塞
		scanner             = NewStreamScanner(resp.Body)
		streamTimer         = time.NewTimer(firstEventTimeout)
		writeMutex          sync.Mutex     // Mutex to protect concurrent writes
		wg                  sync.WaitGroup // 用于等待所有 goroutine 退出
		cleanupOnce         sync.Once
		stopOnce            sync.Once
		firstEventReady     = make(chan struct{})
		firstEventReadyOnce sync.Once
		firstEventSeen      atomic.Bool
		acceptedEvent       = make(chan struct{}, 1)
	)

	stop := func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
	}
	ctx = context.WithValue(ctx, "stop_chan", stopChan)

	generalSettings := operation_setting.GetGeneralSetting()
	pingEnabled := generalSettings.PingIntervalEnabled && !info.DisablePing
	pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if common.DebugEnabled {
		// print timeout and ping interval for debugging
		println("relay timeout seconds:", common.RelayTimeout)
		println("relay max idle conns:", common.RelayMaxIdleConns)
		println("relay max idle conns per host:", common.RelayMaxIdleConnsPerHost)
		println("streaming timeout seconds:", int64(streamingTimeout.Seconds()))
		println("first event timeout seconds:", int64(firstEventTimeout.Seconds()))
		println("ping interval seconds:", int64(pingInterval.Seconds()))
	}

	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			stop()
			if resp.Body != nil {
				_ = resp.Body.Close()
			}

			streamTimer.Stop()
			wg.Wait()
		})
	}
	// Ensure gin.Context is not returned to Gin's pool while any stream goroutine can still use it.
	defer cleanup()

	scanner.Split(bufio.ScanLines)

	// Handle ping data sending with improved error handling
	if pingEnabled {
		wg.Add(1)
		gopool.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					logger.LogError(c, fmt.Sprintf("ping goroutine panic: %v", r))
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("ping panic: %v", r))
					stop()
				}
				logger.LogDebug(c, "ping goroutine exited")
				wg.Done()
			}()

			// Do not emit a gateway-generated heartbeat before the first valid
			// upstream data event. A pre-event ping would commit a downstream 200
			// response and make a subsequent first-event timeout impossible to
			// surface as a normal relay error/retry decision.
			select {
			case <-firstEventReady:
			case <-ctx.Done():
				return
			case <-stopChan:
				return
			case <-c.Request.Context().Done():
				return
			}

			pingTicker := time.NewTicker(pingInterval)
			defer pingTicker.Stop()

			for {
				select {
				case <-pingTicker.C:
					var err error
					func() {
						writeMutex.Lock()
						defer writeMutex.Unlock()
						ExtendWriteDeadline(c)
						err = PingData(c)
					}()
					if err != nil {
						logger.LogError(c, "ping data error: "+err.Error())
						info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPingFail, err)
						stop()
						return
					}
					logger.LogDebug(c, "ping data sent")
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				case <-c.Request.Context().Done():
					// 监听客户端断开连接
					return
				}
			}
		})
	}

	// Keep the downstream connection alive while response headers have arrived
	// but the provider has not produced its first valid protocol event yet. The
	// pre-response phase uses the same interval in relay/channel; this goroutine
	// takes over after client.Do returns and exits before the first data write.
	preFirstEventHeartbeatInterval := common.RelayPreFirstEventHeartbeatInterval
	if !info.DisablePing && preFirstEventHeartbeatInterval > 0 {
		wg.Add(1)
		gopool.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					logger.LogError(c, fmt.Sprintf("pre-first-event heartbeat panic: %v", r))
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("pre-first-event heartbeat panic: %v", r))
					stop()
				}
				wg.Done()
			}()

			ticker := time.NewTicker(preFirstEventHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					var err error
					func() {
						writeMutex.Lock()
						defer writeMutex.Unlock()
						ExtendWriteDeadline(c)
						err = PingData(c)
					}()
					if err != nil {
						logger.LogDebug(c, "pre-first-event heartbeat stopped: "+err.Error())
						return
					}
				case <-firstEventReady:
					return
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				case <-c.Request.Context().Done():
					return
				}
			}
		})
	}

	dataChan := make(chan StreamFrame, 10)
	scannerEndReason := relaycommon.StreamEndReasonEOF
	var scannerEndErr error

	markAccepted := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-stopChan:
			return false
		default:
		}

		if firstEventSeen.CompareAndSwap(false, true) {
			info.SetFirstResponseTime()
			firstEventReadyOnce.Do(func() {
				close(firstEventReady)
			})
		}
		info.ReceivedResponseCount++
		select {
		case acceptedEvent <- struct{}{}:
		default:
		}
		return true
	}

	wg.Add(1)
	gopool.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, fmt.Sprintf("data handler goroutine panic: %v", r))
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("handler panic: %v", r))
			}
			stop()
			wg.Done()
		}()
		sr := newStreamResult(info.StreamStatus, markAccepted)
		for {
			var frame StreamFrame
			var ok bool
			select {
			case <-ctx.Done():
				return
			case frame, ok = <-dataChan:
				if !ok {
					info.StreamStatus.SetEndReason(scannerEndReason, scannerEndErr)
					return
				}
			}
			sr.reset()
			func() {
				writeMutex.Lock()
				defer writeMutex.Unlock()
				ExtendWriteDeadline(c)
				dataHandler(frame, sr)
			}()
			if sr.IsStopped() {
				return
			}
		}
	})

	// Scanner goroutine with improved error handling
	wg.Add(1)
	common.RelayCtxGo(ctx, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, fmt.Sprintf("scanner goroutine panic: %v", r))
				scannerEndReason = relaycommon.StreamEndReasonPanic
				scannerEndErr = fmt.Errorf("scanner panic: %v", r)
			}
			close(dataChan)
			logger.LogDebug(c, "scanner goroutine exited")
			wg.Done()
		}()

		emitFrames := func(frames []StreamFrame) bool {
			for _, frame := range frames {
				select {
				case dataChan <- frame:
				case <-ctx.Done():
					if err := c.Request.Context().Err(); err != nil {
						info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
					}
					return false
				case <-stopChan:
					return false
				}
			}
			return true
		}

		for scanner.Scan() {
			// 检查是否需要停止
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				if err := c.Request.Context().Err(); err != nil {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
				}
				return
			default:
			}

			line := scanner.Text()
			if common.DebugEnabled {
				println(line)
			}
			frames, decodeErr := decoder.Feed(line)
			if decodeErr != nil {
				scannerEndReason = relaycommon.StreamEndReasonHandlerStop
				scannerEndErr = decodeErr
				return
			}
			if !emitFrames(frames) {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if err != io.EOF {
				if isExpectedStreamCloseError(err) && ctx.Err() != nil {
					return
				}
				logger.LogError(c, "scanner error: "+err.Error())
				scannerEndReason = relaycommon.StreamEndReasonScannerErr
				scannerEndErr = err
			}
			return
		}
		frames, decodeErr := decoder.Flush()
		if decodeErr != nil {
			scannerEndReason = relaycommon.StreamEndReasonHandlerStop
			scannerEndErr = decodeErr
			return
		}
		_ = emitFrames(frames)
	})

	resetStreamTimer := func(timeout time.Duration) {
		if !streamTimer.Stop() {
			select {
			case <-streamTimer.C:
			default:
			}
		}
		streamTimer.Reset(timeout)
	}

	finished := false
	for !finished {
		select {
		case <-acceptedEvent:
			resetStreamTimer(streamingTimeout)
		case <-streamTimer.C:
			// An accepted event and the old timer can become ready together. The
			// coordinator owns the timer and gives the accepted event precedence,
			// preventing a valid boundary event from being mislabeled as timeout.
			select {
			case <-acceptedEvent:
				resetStreamTimer(streamingTimeout)
				continue
			default:
			}

			writeMutex.Lock()
			select {
			case <-acceptedEvent:
				writeMutex.Unlock()
				resetStreamTimer(streamingTimeout)
				continue
			default:
			}
			if firstEventSeen.Load() {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
			} else {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonFirstEventTimeout, nil)
			}
			stop()
			writeMutex.Unlock()
			finished = true
		case <-stopChan:
			if err := c.Request.Context().Err(); err != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
			}
			finished = true
		case <-c.Request.Context().Done():
			// 客户端断开：立即关闭上游 resp.Body，解除 scanner 阻塞并让上游停止生成。
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, c.Request.Context().Err())
			stop()
			finished = true
		}
	}

	cleanup()
	if info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
		logger.LogInfo(c, fmt.Sprintf("stream ended: %s", info.StreamStatus.Summary()))
	} else {
		logger.LogError(c, fmt.Sprintf("stream ended: %s, received=%d", info.StreamStatus.Summary(), info.ReceivedResponseCount))
	}
}

func isExpectedStreamCloseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "read on closed response body")
}
