package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundRelayJobSupportsIncrementalReconnect(t *testing.T) {
	job := NewBackgroundRelayJob("task_test", 32)
	t.Cleanup(func() { DeleteBackgroundRelayJob("task_test") })
	job.Start()
	if _, err := job.Append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	snapshot := job.Snapshot(0, 3)
	if string(snapshot.Data) != "fir" || snapshot.NextOffset != 3 || snapshot.Done {
		t.Fatalf("first snapshot = %#v", snapshot)
	}

	updated := make(chan BackgroundRelaySnapshot, 1)
	go func() { updated <- job.WaitForUpdate(context.Background(), 5, time.Second) }()
	if _, err := job.Append([]byte(" second")); err != nil {
		t.Fatal(err)
	}
	snapshot = <-updated
	if string(snapshot.Data) != " second" || snapshot.Offset != 5 {
		t.Fatalf("updated snapshot = %#v", snapshot)
	}

	job.Complete("succeeded", 200, "text/event-stream", map[string]string{"X-Test": "ok"}, RetryStopReasonSuccess)
	snapshot = job.SnapshotAll()
	if !snapshot.Done || !snapshot.Available || snapshot.Status != "succeeded" || snapshot.StopReason != RetryStopReasonSuccess {
		t.Fatalf("completed snapshot = %#v", snapshot)
	}
}

func TestBackgroundRelayJobEnforcesResultLimit(t *testing.T) {
	job := NewBackgroundRelayJob("task_limit", 4)
	t.Cleanup(func() { DeleteBackgroundRelayJob("task_limit") })
	if _, err := job.Append([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := job.Append([]byte("5")); !errors.Is(err, ErrBackgroundRelayResultTooLarge) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestBackgroundRelaySlotReleaseIsIdempotent(t *testing.T) {
	release, ok := TryAcquireBackgroundRelaySlot(77)
	if !ok || release == nil {
		t.Fatal("expected background relay slot")
	}
	release()
	release()
	secondRelease, ok := TryAcquireBackgroundRelaySlot(77)
	if !ok || secondRelease == nil {
		t.Fatal("idempotent release must make the slot available")
	}
	secondRelease()
}
