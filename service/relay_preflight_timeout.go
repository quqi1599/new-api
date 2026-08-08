package service

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// RelayPreflight bounds provider calls that happen before the primary relay
// request (for example media uploads and access-token exchanges). Its local
// deadline is derived from the inbound request start, so preflight work consumes
// the same non-stream or first-event budget as the model request that follows.
//
// unsafeToReplay must be true for side-effecting uploads. Once such a request
// may have crossed the wire, ambiguous failures stop channel replay while still
// remaining eligible for channel-health accounting. Semantically safe token
// exchanges use false and may be retried while request-wide budget remains.
type RelayPreflight struct {
	ginCtx                *gin.Context
	info                  *relaycommon.RelayInfo
	caller                context.Context
	ctx                   context.Context
	cancel                context.CancelFunc
	unsafeToReplay        bool
	channelHealthEligible bool
	localDeadline         time.Time
	timeoutPhase          string
	timeoutSeconds        int
	timeoutErrorCode      types.ErrorCode

	connected              atomic.Bool
	requestMayHaveBeenSent atomic.Bool
	writeTimedOut          atomic.Bool
	tlsHandshakeTimedOut   atomic.Bool
}

// NewRelayPreflight creates a request-scoped preflight guard. caller may be nil;
// in that case the inbound Gin request context is used, then Background as a
// final fallback for maintenance callers.
func NewRelayPreflight(c *gin.Context, caller context.Context, info *relaycommon.RelayInfo, unsafeToReplay bool) *RelayPreflight {
	return NewRelayPreflightWithChannelHealth(c, caller, info, unsafeToReplay, true)
}

// NewRelayPreflightWithChannelHealth separates replay safety from channel
// health attribution. Provider-owned preflights should pass true; downloads of
// user/provider asset URLs pass false so CDN/object-storage failures do not
// penalize the selected model channel.
func NewRelayPreflightWithChannelHealth(c *gin.Context, caller context.Context, info *relaycommon.RelayInfo, unsafeToReplay bool, channelHealthEligible bool) *RelayPreflight {
	common.ResetRelayAttemptTimeoutContext(c)
	if caller == nil && c != nil && c.Request != nil {
		caller = c.Request.Context()
	}
	if caller == nil {
		caller = context.Background()
	}

	p := &RelayPreflight{
		ginCtx:                c,
		info:                  info,
		caller:                caller,
		ctx:                   caller,
		unsafeToReplay:        unsafeToReplay,
		channelHealthEligible: channelHealthEligible,
	}

	startTime := time.Now()
	if info != nil && !info.StartTime.IsZero() {
		startTime = info.StartTime
	}

	if info != nil && info.IsStream {
		p.timeoutPhase = "first_valid_event_total"
		p.timeoutSeconds = common.RelayFirstEventTotalTimeout
		p.timeoutErrorCode = types.ErrorCodeUpstreamFirstEventTimeout
		if p.timeoutSeconds > 0 {
			info.EnsureFirstValidEventDeadline(startTime, time.Duration(p.timeoutSeconds)*time.Second)
			if remaining, limited := info.RemainingFirstValidEventBudget(); limited {
				p.localDeadline = time.Now().Add(remaining)
			}
		}
	} else {
		p.timeoutPhase = "non_stream_total"
		p.timeoutSeconds = common.RelayNonStreamTimeout
		p.timeoutErrorCode = types.ErrorCodeUpstreamNonStreamTimeout
		if p.timeoutSeconds > 0 {
			infoTimeout := time.Duration(p.timeoutSeconds) * time.Second
			if info != nil {
				info.EnsureNonStreamDeadline(startTime, infoTimeout)
				if remaining, limited := info.RemainingNonStreamBudget(); limited {
					p.localDeadline = time.Now().Add(remaining)
				}
			} else {
				p.localDeadline = startTime.Add(infoTimeout)
			}
		}
	}

	if !p.localDeadline.IsZero() {
		p.ctx, p.cancel = context.WithDeadline(caller, p.localDeadline)
	}
	return p
}

func (p *RelayPreflight) Context() context.Context {
	if p == nil || p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *RelayPreflight) Close() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

func (p *RelayPreflight) setContextKey(key constant.ContextKey, value any) {
	if p != nil && p.ginCtx != nil {
		common.SetContextKey(p.ginCtx, key, value)
	}
}

func (p *RelayPreflight) bindRequest(req *http.Request) *http.Request {
	if p == nil || req == nil {
		return req
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			p.connected.Store(true)
			p.setContextKey(constant.ContextKeyRelayConnectedUpstream, true)
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			var netErr net.Error
			if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()) {
				p.tlsHandshakeTimedOut.Store(true)
				p.setContextKey(constant.ContextKeyRelayTimeoutPhase, "tls_handshake")
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			p.requestMayHaveBeenSent.Store(true)
			if info.Err == nil {
				p.setContextKey(constant.ContextKeyRelayRequestWritten, true)
			} else {
				var netErr net.Error
				if errors.Is(info.Err, context.DeadlineExceeded) || errors.As(info.Err, &netErr) && netErr.Timeout() {
					p.writeTimedOut.Store(true)
					p.setContextKey(constant.ContextKeyRelayTimeoutPhase, "request_write")
				}
			}
			if p.unsafeToReplay && p.info != nil {
				p.info.MarkUpstreamRequestMayHaveBeenAccepted()
			}
		},
	}
	ctx := httptrace.WithClientTrace(p.Context(), trace)
	return req.WithContext(ctx)
}

// Do sends a preflight request with trace-based replay-safety bookkeeping.
// The response body remains bound to the preflight context and must be fully
// consumed before Close is called.
func (p *RelayPreflight) Do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(p.bindRequest(req))
	if resp != nil {
		p.requestMayHaveBeenSent.Store(true)
		p.setContextKey(constant.ContextKeyRelayResponseHeaders, true)
		if p != nil && p.unsafeToReplay && p.info != nil {
			p.info.MarkUpstreamRequestMayHaveBeenAccepted()
		}
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return resp, p.ClassifyError(err)
	}
	return resp, nil
}

func (p *RelayPreflight) replaySafetyOptions(skipRetry bool) []types.NewAPIErrorOptions {
	options := make([]types.NewAPIErrorOptions, 0, 2)
	if skipRetry {
		options = append(options, types.ErrOptionWithSkipRetry())
	}
	if p != nil && p.channelHealthEligible && p.requestMayHaveBeenSent.Load() {
		// A total-budget failure after any upstream write is a channel-health
		// signal. For unsafe uploads, ambiguous transport/body failures also
		// stop replay because the upload may already exist upstream.
		if skipRetry || p.unsafeToReplay {
			options = append(options, types.ErrOptionWithChannelPenalty())
		}
	}
	return options
}

func (p *RelayPreflight) localBudgetExpired() bool {
	if p == nil || p.localDeadline.IsZero() || p.caller == nil || p.caller.Err() != nil {
		return false
	}
	return errors.Is(p.Context().Err(), context.DeadlineExceeded) || !time.Now().Before(p.localDeadline)
}

// ClassifyError preserves inbound cancellation, maps request-wide or transport
// timeouts to typed 504s, and closes the replay gate only after an unsafe POST
// may have reached the provider.
func (p *RelayPreflight) ClassifyError(err error) *types.NewAPIError {
	if err == nil {
		return nil
	}
	var typedErr *types.NewAPIError
	if errors.As(err, &typedErr) {
		return typedErr
	}

	if p != nil && p.caller != nil {
		switch callerErr := p.caller.Err(); {
		case errors.Is(callerErr, context.Canceled):
			p.setContextKey(constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginDownstreamDisconnected)
			return types.NewErrorWithStatusCode(
				errors.New("downstream connection closed during upstream preflight"),
				types.ErrorCodeDoRequestFailed,
				499,
				types.ErrOptionWithSkipRetry(),
			)
		case errors.Is(callerErr, context.DeadlineExceeded):
			p.setContextKey(constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginGatewayDeadline)
			return types.NewErrorWithStatusCode(
				errors.New("request deadline exceeded during upstream preflight"),
				types.ErrorCodeDoRequestFailed,
				http.StatusGatewayTimeout,
				types.ErrOptionWithSkipRetry(),
			)
		}
	}

	if p != nil && p.localBudgetExpired() {
		p.setContextKey(constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginGatewayDeadline)
		p.setContextKey(constant.ContextKeyRelayTimeoutPhase, p.timeoutPhase)
		p.setContextKey(constant.ContextKeyRelayTimeoutSeconds, p.timeoutSeconds)
		message := "upstream preflight exhausted the request-wide non-stream budget"
		if p.timeoutErrorCode == types.ErrorCodeUpstreamFirstEventTimeout {
			message = "upstream preflight exhausted the request-wide first valid event budget"
		}
		return types.NewErrorWithStatusCode(
			errors.New(message),
			p.timeoutErrorCode,
			http.StatusGatewayTimeout,
			p.replaySafetyOptions(true)...,
		)
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		p.setContextKey(constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginUpstreamTimeout)
		phase := "response_headers"
		timeoutSeconds := common.RelayResponseHeaderTimeout
		errorCode := types.ErrorCodeUpstreamResponseHeaderTimeout
		message := "upstream preflight response headers timed out"
		if p != nil && p.writeTimedOut.Load() {
			phase = "request_write"
			errorCode = types.ErrorCodeUpstreamRequestWriteTimeout
			message = "upstream preflight request write timed out after data may have been sent"
		} else if p != nil && p.tlsHandshakeTimedOut.Load() {
			phase = "tls_handshake"
			timeoutSeconds = common.RelayTLSHandshakeTimeout
			errorCode = types.ErrorCodeUpstreamTLSHandshakeTimeout
			message = "upstream preflight TLS handshake timed out"
		} else if p != nil && !p.connected.Load() {
			phase = "connect"
			timeoutSeconds = common.RelayDialTimeout
			errorCode = types.ErrorCodeUpstreamConnectionTimeout
			message = "upstream preflight connection timed out"
		}
		p.setContextKey(constant.ContextKeyRelayTimeoutPhase, phase)
		p.setContextKey(constant.ContextKeyRelayTimeoutSeconds, timeoutSeconds)
		skipRetry := p != nil && (!p.channelHealthEligible || p.unsafeToReplay && p.requestMayHaveBeenSent.Load())
		return types.NewErrorWithStatusCode(
			errors.New(message),
			errorCode,
			http.StatusGatewayTimeout,
			p.replaySafetyOptions(skipRetry)...,
		)
	}

	if errors.Is(err, context.Canceled) {
		p.setContextKey(constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginInternalAbort)
	}
	skipRetry := p != nil && (!p.channelHealthEligible || p.unsafeToReplay && p.requestMayHaveBeenSent.Load())
	return types.NewError(err, types.ErrorCodeDoRequestFailed, p.replaySafetyOptions(skipRetry)...)
}
