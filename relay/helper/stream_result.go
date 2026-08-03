package helper

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// StreamResult is passed to each dataHandler invocation, providing methods
// to record soft errors, signal fatal stops, or mark normal completion.
// StreamScannerHandler checks IsStopped() after each callback invocation.
type StreamResult struct {
	status   *relaycommon.StreamStatus
	accept   func() bool
	accepted bool
	stopped  bool
}

func newStreamResult(status *relaycommon.StreamStatus, accept func() bool) *StreamResult {
	return &StreamResult{status: status, accept: accept}
}

// Accept confirms that the current SSE data frame is valid for the upstream
// protocol. Adapters must call this after parsing and semantic validation, but
// before their first downstream write. Only accepted frames satisfy the
// first-event deadline, reset the inter-event idle timer, or start heartbeats.
func (r *StreamResult) Accept() bool {
	if r == nil || r.stopped {
		return false
	}
	if r.accepted {
		return true
	}
	if r.accept != nil && !r.accept() {
		r.stopped = true
		return false
	}
	r.accepted = true
	return true
}

// Error records a soft error. The stream continues processing.
// Can be called multiple times per chunk.
func (r *StreamResult) Error(err error) {
	if err == nil {
		return
	}
	r.status.RecordError(err.Error())
}

// Stop records a fatal error and marks the stream to stop after this chunk.
func (r *StreamResult) Stop(err error) {
	if err != nil {
		r.status.RecordError(err.Error())
	}
	r.status.SetEndReason(relaycommon.StreamEndReasonHandlerStop, err)
	r.stopped = true
}

// Done signals that the handler has finished processing normally
// (e.g., Dify "message_end"). The stream stops after this chunk.
func (r *StreamResult) Done() {
	if r == nil || r.stopped {
		return
	}
	r.status.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	r.stopped = true
}

// IsStopped returns whether Stop() or Done() was called during this chunk.
func (r *StreamResult) IsStopped() bool {
	return r.stopped
}

// reset clears the per-chunk stopped flag so the object can be reused.
func (r *StreamResult) reset() {
	r.accepted = false
	r.stopped = false
}
