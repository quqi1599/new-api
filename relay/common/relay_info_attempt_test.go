package common

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamAttemptRejectsLateAcceptanceAndPhaseCallbacks(t *testing.T) {
	info := &RelayInfo{ReceivedResponseCount: 2, SendResponseCount: 1}
	deadline := time.Now().Add(time.Minute)
	info.SetFirstValidEventDeadline(deadline)
	info.BeginUpstreamAttempt()
	oldMark := info.UpstreamRequestAcceptanceMarker()
	oldObserve := info.UpstreamAttemptObserver()
	oldMark()
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())

	info.BeginUpstreamAttempt()
	var staleObservations atomic.Int32
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				oldMark()
				oldObserve(func() { staleObservations.Add(1) })
			}
		}()
	}
	workers.Wait()
	require.False(t, info.UpstreamRequestMayHaveBeenAccepted(), "a previous attempt cannot close this attempt's replay gate")
	require.Zero(t, staleObservations.Load(), "late phase/cancel observations must not contaminate the new attempt")
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Equal(t, 1, info.SendResponseCount)
	require.Equal(t, deadline, info.firstValidEventDeadline)

	info.UpstreamRequestAcceptanceMarker()()
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted(), "the current attempt must still close the gate")
	info.UpstreamAttemptObserver()(func() { staleObservations.Add(1) })
	require.EqualValues(t, 1, staleObservations.Load())
}

func TestUpstreamAttemptTransitionWaitsForCurrentPhaseObservation(t *testing.T) {
	info := &RelayInfo{}
	info.BeginUpstreamAttempt()
	observe := info.UpstreamAttemptObserver()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var phase atomic.Int32
	go observe(func() {
		close(started)
		<-release
		phase.Store(1)
	})
	<-started
	go func() {
		info.BeginUpstreamAttempt()
		phase.Store(0)
		close(finished)
	}()
	close(release)
	<-finished
	require.Zero(t, phase.Load(), "phase reset must happen after an in-flight old observer")
	observe(func() { phase.Store(1) })
	require.Zero(t, phase.Load(), "the old observer is fenced after the transition")
}
