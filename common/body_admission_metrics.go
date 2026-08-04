package common

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/constant"
)

var requestBodySizeUpperBounds = [...]int64{
	1 << 20,
	4 << 20,
	16 << 20,
	64 << 20,
	256 << 20,
}

var admissionHoldUpperBounds = [...]time.Duration{
	10 * time.Millisecond,
	100 * time.Millisecond,
	time.Second,
	10 * time.Second,
}

type RequestBodySizeBuckets struct {
	LE1MiB   int64 `json:"le_1_mib"`
	LE4MiB   int64 `json:"le_4_mib"`
	LE16MiB  int64 `json:"le_16_mib"`
	LE64MiB  int64 `json:"le_64_mib"`
	LE256MiB int64 `json:"le_256_mib"`
}

type AdmissionHoldDurationBuckets struct {
	LE10Milliseconds  int64 `json:"le_10_milliseconds"`
	LE100Milliseconds int64 `json:"le_100_milliseconds"`
	LE1Second         int64 `json:"le_1_second"`
	LE10Seconds       int64 `json:"le_10_seconds"`
}

// RequestBodyStats exposes the admission pressure and observed request-body
// distribution through the existing root-only performance stats endpoint.
// Histogram buckets are cumulative, matching Prometheus histogram semantics.
type RequestBodyStats struct {
	ActiveLargeBodySlots              int64                        `json:"active_large_body_slots"`
	LargeBodySlotLimit                int64                        `json:"large_body_slot_limit"`
	RejectedLargeBodyAdmissionsTotal  int64                        `json:"rejected_large_body_admissions_total"`
	ObservedBodiesTotal               int64                        `json:"observed_bodies_total"`
	ObservedBodyBytesTotal            int64                        `json:"observed_body_bytes_total"`
	MaxObservedBodyBytes              int64                        `json:"max_observed_body_bytes"`
	RequestBodySizeBuckets            RequestBodySizeBuckets       `json:"request_body_size_bytes"`
	AdmissionHoldsTotal               int64                        `json:"admission_holds_total"`
	AdmissionHoldDurationSecondsTotal float64                      `json:"admission_hold_duration_seconds_total"`
	AdmissionHoldDurationSecondsMax   float64                      `json:"admission_hold_duration_seconds_max"`
	AdmissionHoldDurationBuckets      AdmissionHoldDurationBuckets `json:"admission_hold_duration_seconds"`
}

var requestBodyMetrics struct {
	activeLargeBodySlots        atomic.Int64
	rejectedLargeBodies         atomic.Int64
	observedBodies              atomic.Int64
	observedBodyBytes           atomic.Int64
	maxObservedBodyBytes        atomic.Int64
	bodySizeBuckets             [len(requestBodySizeUpperBounds)]atomic.Int64
	admissionHolds              atomic.Int64
	admissionHoldNanoseconds    atomic.Int64
	maxAdmissionHoldNanoseconds atomic.Int64
	admissionHoldBuckets        [len(admissionHoldUpperBounds)]atomic.Int64
}

type bodyAdmissionLease struct {
	startedAt time.Time
	once      sync.Once
}

func largeBodySlotLimit() int64 {
	limit := int64(constant.MaxConcurrentLargeRequestBodies)
	if limit <= 0 {
		return 4
	}
	return limit
}

func acquireLargeRequestBodySlot() *bodyAdmissionLease {
	limit := largeBodySlotLimit()
	for {
		current := requestBodyMetrics.activeLargeBodySlots.Load()
		if current >= limit {
			requestBodyMetrics.rejectedLargeBodies.Add(1)
			return nil
		}
		if requestBodyMetrics.activeLargeBodySlots.CompareAndSwap(current, current+1) {
			return &bodyAdmissionLease{startedAt: time.Now()}
		}
	}
}

func (l *bodyAdmissionLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if requestBodyMetrics.activeLargeBodySlots.Add(-1) < 0 {
			requestBodyMetrics.activeLargeBodySlots.Store(0)
		}
		holdDuration := time.Since(l.startedAt)
		if holdDuration < 0 {
			holdDuration = 0
		}
		recordAdmissionHoldDuration(holdDuration)
	})
}

func observeRequestBodySize(size int64) {
	if size < 0 {
		return
	}
	requestBodyMetrics.observedBodies.Add(1)
	requestBodyMetrics.observedBodyBytes.Add(size)
	updateAtomicMax(&requestBodyMetrics.maxObservedBodyBytes, size)
	for index, upperBound := range requestBodySizeUpperBounds {
		if size <= upperBound {
			requestBodyMetrics.bodySizeBuckets[index].Add(1)
		}
	}
}

func recordAdmissionHoldDuration(duration time.Duration) {
	nanoseconds := duration.Nanoseconds()
	requestBodyMetrics.admissionHolds.Add(1)
	requestBodyMetrics.admissionHoldNanoseconds.Add(nanoseconds)
	updateAtomicMax(&requestBodyMetrics.maxAdmissionHoldNanoseconds, nanoseconds)
	for index, upperBound := range admissionHoldUpperBounds {
		if duration <= upperBound {
			requestBodyMetrics.admissionHoldBuckets[index].Add(1)
		}
	}
}

func updateAtomicMax(target *atomic.Int64, candidate int64) {
	for {
		current := target.Load()
		if candidate <= current || target.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func GetRequestBodyStats() RequestBodyStats {
	return RequestBodyStats{
		ActiveLargeBodySlots:             requestBodyMetrics.activeLargeBodySlots.Load(),
		LargeBodySlotLimit:               largeBodySlotLimit(),
		RejectedLargeBodyAdmissionsTotal: requestBodyMetrics.rejectedLargeBodies.Load(),
		ObservedBodiesTotal:              requestBodyMetrics.observedBodies.Load(),
		ObservedBodyBytesTotal:           requestBodyMetrics.observedBodyBytes.Load(),
		MaxObservedBodyBytes:             requestBodyMetrics.maxObservedBodyBytes.Load(),
		RequestBodySizeBuckets: RequestBodySizeBuckets{
			LE1MiB:   requestBodyMetrics.bodySizeBuckets[0].Load(),
			LE4MiB:   requestBodyMetrics.bodySizeBuckets[1].Load(),
			LE16MiB:  requestBodyMetrics.bodySizeBuckets[2].Load(),
			LE64MiB:  requestBodyMetrics.bodySizeBuckets[3].Load(),
			LE256MiB: requestBodyMetrics.bodySizeBuckets[4].Load(),
		},
		AdmissionHoldsTotal:               requestBodyMetrics.admissionHolds.Load(),
		AdmissionHoldDurationSecondsTotal: float64(requestBodyMetrics.admissionHoldNanoseconds.Load()) / float64(time.Second),
		AdmissionHoldDurationSecondsMax:   float64(requestBodyMetrics.maxAdmissionHoldNanoseconds.Load()) / float64(time.Second),
		AdmissionHoldDurationBuckets: AdmissionHoldDurationBuckets{
			LE10Milliseconds:  requestBodyMetrics.admissionHoldBuckets[0].Load(),
			LE100Milliseconds: requestBodyMetrics.admissionHoldBuckets[1].Load(),
			LE1Second:         requestBodyMetrics.admissionHoldBuckets[2].Load(),
			LE10Seconds:       requestBodyMetrics.admissionHoldBuckets[3].Load(),
		},
	}
}

// ResetRequestBodyStats clears cumulative counters while preserving the active
// gauge. In-flight leases can therefore still release safely after a reset.
func ResetRequestBodyStats() {
	requestBodyMetrics.rejectedLargeBodies.Store(0)
	requestBodyMetrics.observedBodies.Store(0)
	requestBodyMetrics.observedBodyBytes.Store(0)
	requestBodyMetrics.maxObservedBodyBytes.Store(0)
	for index := range requestBodyMetrics.bodySizeBuckets {
		requestBodyMetrics.bodySizeBuckets[index].Store(0)
	}
	requestBodyMetrics.admissionHolds.Store(0)
	requestBodyMetrics.admissionHoldNanoseconds.Store(0)
	requestBodyMetrics.maxAdmissionHoldNanoseconds.Store(0)
	for index := range requestBodyMetrics.admissionHoldBuckets {
		requestBodyMetrics.admissionHoldBuckets[index].Store(0)
	}
}
