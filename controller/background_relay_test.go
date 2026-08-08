package controller

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
)

func TestBackgroundRelayTaskIDIsIdempotentAndScoped(t *testing.T) {
	first := backgroundRelayTaskID(1, 2, "same-key")
	second := backgroundRelayTaskID(1, 2, "same-key")
	if first != second {
		t.Fatalf("idempotent task ids differ: %q != %q", first, second)
	}
	if first == backgroundRelayTaskID(1, 3, "same-key") || first == backgroundRelayTaskID(2, 2, "same-key") {
		t.Fatal("idempotency key must be scoped by user and token")
	}
	if first == backgroundRelayTaskID(1, 2, "different-key") {
		t.Fatal("different idempotency keys must produce different task ids")
	}
}

func TestBackgroundRelayTextJobEnvelopeExceedsRelayPhaseBudgets(t *testing.T) {
	if defaultBackgroundRelayTextJobTimeoutSeconds <= 540 {
		t.Fatalf("background text job timeout = %d, must leave cleanup margin after 540s relay budgets", defaultBackgroundRelayTextJobTimeoutSeconds)
	}
}

func TestBackgroundRelayRequestFingerprintIncludesRouteAndFormat(t *testing.T) {
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/v1/chat/completions"}}
	first := backgroundRelayRequestFingerprint(request, types.RelayFormatOpenAI, []byte(`{"model":"gpt-example"}`))
	if first != backgroundRelayRequestFingerprint(request, types.RelayFormatOpenAI, []byte(`{"model":"gpt-example"}`)) {
		t.Fatal("identical requests must have identical fingerprints")
	}
	otherRoute := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/v1/responses"}}
	if first == backgroundRelayRequestFingerprint(otherRoute, types.RelayFormatOpenAI, []byte(`{"model":"gpt-example"}`)) {
		t.Fatal("different routes must have different fingerprints")
	}
	if first == backgroundRelayRequestFingerprint(request, types.RelayFormatClaude, []byte(`{"model":"gpt-example"}`)) {
		t.Fatal("different relay formats must have different fingerprints")
	}
}

func TestBackgroundRelayHTTPWriterStreamsIntoReconnectBuffer(t *testing.T) {
	job := service.NewBackgroundRelayJob("task_writer", 16)
	t.Cleanup(func() { service.DeleteBackgroundRelayJob("task_writer") })
	writer := newBackgroundRelayHTTPWriter(job)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write([]byte("data: ok\n\n")); err != nil {
		t.Fatal(err)
	}
	snapshot := job.SnapshotAll()
	if snapshot.HTTPStatus != http.StatusOK || snapshot.ContentType != "text/event-stream" || string(snapshot.Data) != "data: ok\n\n" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if writer.err() != nil {
		t.Fatalf("unexpected writer error: %v", writer.err())
	}

	limitedJob := service.NewBackgroundRelayJob("task_writer_limit", 1)
	t.Cleanup(func() { service.DeleteBackgroundRelayJob("task_writer_limit") })
	limitedWriter := newBackgroundRelayHTTPWriter(limitedJob)
	if _, err := limitedWriter.Write([]byte("too large")); !errors.Is(err, service.ErrBackgroundRelayResultTooLarge) {
		t.Fatalf("limit error = %v", err)
	}
}
