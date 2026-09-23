package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestDownstreamStreamErrorRequiresDeliveryProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		recognized bool
	}{
		{"nil", nil, false},
		{"untyped closed pipe", io.ErrClosedPipe, false},
		{"lookalike parsing message", errors.New("write stream data failed: io: read/write on closed pipe"), false},
		{"typed writer failure", wrapDownstreamWriteError(io.ErrClosedPipe), true},
		{"wrapped typed writer failure", fmt.Errorf("outer: %w", wrapDownstreamWriteError(io.ErrClosedPipe)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Writer.WriteHeaderNow()
			info := &relaycommon.RelayInfo{ReceivedResponseCount: 1}
			err := DownstreamStreamError(c, info, tc.err)
			if (err != nil) != tc.recognized {
				t.Fatalf("recognized=%v want=%v", err != nil, tc.recognized)
			}
			if err == nil {
				return
			}
			if err.StatusCode != 499 || types.IsChannelPenaltyAllowed(err) || !types.IsSkipRetryError(err) || info.PartialStreamError != err {
				t.Fatalf("bad delivery classification: %#v", err)
			}
			if info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
				t.Fatal(info.StreamStatus.EndReason)
			}
		})
	}
}

func TestDownstreamStreamDeadlineNeverPenalizesUpstream(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	c.Writer.WriteHeaderNow()
	info := &relaycommon.RelayInfo{ReceivedResponseCount: 1}
	err := DownstreamStreamError(c, info, StringData(c, "partial"))
	if err == nil || err.StatusCode != http.StatusGatewayTimeout || types.IsChannelPenaltyAllowed(err) || info.PartialStreamError != err {
		t.Fatalf("bad deadline classification: %#v", err)
	}
	if info.StreamStatus.EndReason != relaycommon.StreamEndReasonRequestDeadline || common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin) != constant.RelayCancelOriginGatewayDeadline {
		t.Fatal("missing gateway deadline provenance")
	}
}

func TestDownstreamStreamEndCorrectionRequiresJoinedMatchingScanner(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		joined, stale, deadline, upstreamFailure bool
	}{
		{name: "active scanner"},
		{name: "joined scanner", joined: true},
		{name: "joined scanner deadline", joined: true, deadline: true},
		{name: "stale scanner marker", joined: true, stale: true},
		{name: "upstream failure stays authoritative", joined: true, upstreamFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.deadline {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			c.Writer.WriteHeaderNow()
			status := relaycommon.NewStreamStatus()
			originalReason := relaycommon.StreamEndReasonDone
			var originalError error
			if tc.upstreamFailure {
				originalReason = relaycommon.StreamEndReasonScannerErr
				originalError = io.ErrUnexpectedEOF
			}
			status.SetEndReason(originalReason, originalError)
			status.RecordError("previous soft error")
			info := &relaycommon.RelayInfo{StreamStatus: status, ReceivedResponseCount: 1}
			if tc.joined {
				markStreamScannerCompleted(c, info)
			}
			if tc.stale {
				info.StreamStatus = relaycommon.NewStreamStatus()
				info.StreamStatus.SetEndReason(originalReason, originalError)
				info.StreamStatus.RecordError("previous soft error")
			}
			failure := wrapDownstreamWriteError(io.ErrClosedPipe)
			DownstreamStreamError(c, info, failure)
			want, wantErr, wantErrors := originalReason, originalError, 1
			if tc.joined && !tc.stale && !tc.upstreamFailure {
				want, wantErr, wantErrors = relaycommon.StreamEndReasonClientGone, failure, 2
				if tc.deadline {
					want = relaycommon.StreamEndReasonRequestDeadline
				}
			}
			if info.StreamStatus.EndReason != want || info.StreamStatus.EndError != wantErr || info.StreamStatus.TotalErrorCount() != wantErrors {
				t.Fatalf("terminal correction=%s err=%v errors=%d; want=%s err=%v errors=%d", info.StreamStatus.EndReason, info.StreamStatus.EndError, info.StreamStatus.TotalErrorCount(), want, wantErr, wantErrors)
			}
		})
	}
}
