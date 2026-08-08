package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetRelayResponseHeaderTimeoutSeconds(t *testing.T) {
	t.Run("uses safe default when no legacy value exists", func(t *testing.T) {
		t.Setenv("RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
		assert.Equal(t, defaultRelayResponseHeaderTimeoutSeconds, getRelayResponseHeaderTimeoutSeconds(0))
	})

	t.Run("caps unsafe legacy relay timeout at the safe phase default", func(t *testing.T) {
		t.Setenv("RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
		assert.Equal(t, defaultRelayResponseHeaderTimeoutSeconds, getRelayResponseHeaderTimeoutSeconds(600))
	})

	t.Run("preserves a smaller legacy relay timeout", func(t *testing.T) {
		t.Setenv("RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
		assert.Equal(t, 120, getRelayResponseHeaderTimeoutSeconds(120))
	})

	t.Run("new phase setting overrides legacy value", func(t *testing.T) {
		t.Setenv("RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS", "420")
		assert.Equal(t, 420, getRelayResponseHeaderTimeoutSeconds(600))
	})

	t.Run("explicit zero disables the phase timeout", func(t *testing.T) {
		t.Setenv("RELAY_RESPONSE_HEADER_TIMEOUT_SECONDS", "0")
		assert.Zero(t, getRelayResponseHeaderTimeoutSeconds(600))
	})
}

func TestGetNonNegativeEnvOrDefaultRejectsNegativeValues(t *testing.T) {
	t.Setenv("RELAY_DIAL_TIMEOUT_SECONDS", "-1")
	assert.Equal(t, defaultRelayDialTimeoutSeconds, getNonNegativeEnvOrDefault("RELAY_DIAL_TIMEOUT_SECONDS", defaultRelayDialTimeoutSeconds))
}

func TestGetRelayFirstEventTimeoutSeconds(t *testing.T) {
	t.Run("uses a bounded first-event default", func(t *testing.T) {
		t.Setenv("RELAY_FIRST_EVENT_TIMEOUT_SECONDS", "")
		assert.Equal(t, defaultRelayFirstEventTimeoutSeconds, getRelayFirstEventTimeoutSeconds())
	})

	t.Run("reads explicit first-event timeout", func(t *testing.T) {
		t.Setenv("RELAY_FIRST_EVENT_TIMEOUT_SECONDS", "45")
		assert.Equal(t, 45, getRelayFirstEventTimeoutSeconds())
	})
}

func TestFirstEventTotalTimeoutDefaultFitsInsideOuterProxyBudget(t *testing.T) {
	t.Setenv("RELAY_FIRST_EVENT_TOTAL_TIMEOUT_SECONDS", "")
	assert.Equal(t, 540, getNonNegativeEnvOrDefault(
		"RELAY_FIRST_EVENT_TOTAL_TIMEOUT_SECONDS",
		defaultRelayFirstEventTotalTimeoutSeconds,
	))
}

func TestRelayTimeoutDefaultsFitInsideOuterProxyBudget(t *testing.T) {
	assert.Equal(t, 520, defaultRelayResponseHeaderTimeoutSeconds)
	assert.Equal(t, 540, defaultRelayFirstEventTimeoutSeconds)
	assert.Equal(t, 540, defaultRelayFirstEventTotalTimeoutSeconds)
	assert.Equal(t, 540, defaultRelayNonStreamTimeoutSeconds)
	assert.Less(t, defaultRelayResponseHeaderTimeoutSeconds, defaultRelayFirstEventTotalTimeoutSeconds)
}

func TestGetRelayStreamHeartbeatSeconds(t *testing.T) {
	t.Run("uses post-first-event default", func(t *testing.T) {
		t.Setenv("RELAY_STREAM_HEARTBEAT_SECONDS", "")
		t.Setenv("RELAY_PRE_FIRST_EVENT_HEARTBEAT_SECONDS", "")
		assert.Equal(t, defaultRelayStreamHeartbeatSeconds, getRelayStreamHeartbeatSeconds())
	})

	t.Run("uses the new post-first-event setting", func(t *testing.T) {
		t.Setenv("RELAY_STREAM_HEARTBEAT_SECONDS", "27")
		t.Setenv("RELAY_PRE_FIRST_EVENT_HEARTBEAT_SECONDS", "9")
		assert.Equal(t, 27, getRelayStreamHeartbeatSeconds())
	})

	t.Run("explicit zero disables post-first-event heartbeat", func(t *testing.T) {
		t.Setenv("RELAY_STREAM_HEARTBEAT_SECONDS", "0")
		t.Setenv("RELAY_PRE_FIRST_EVENT_HEARTBEAT_SECONDS", "15")
		assert.Zero(t, getRelayStreamHeartbeatSeconds())
	})

	t.Run("keeps the legacy value as a post-first-event fallback", func(t *testing.T) {
		t.Setenv("RELAY_STREAM_HEARTBEAT_SECONDS", "")
		t.Setenv("RELAY_PRE_FIRST_EVENT_HEARTBEAT_SECONDS", "21")
		assert.Equal(t, 21, getRelayStreamHeartbeatSeconds())
	})
}

func TestRelayTimeoutConfigurationWarnings(t *testing.T) {
	t.Run("safe defaults", func(t *testing.T) {
		assert.Empty(t, relayTimeoutConfigurationWarnings(520, 540, 540, 540))
	})

	t.Run("reports ineffective or ambiguous phase ordering", func(t *testing.T) {
		warnings := relayTimeoutConfigurationWarnings(600, 700, 540, 580)
		assert.Len(t, warnings, 3)
		assert.Contains(t, warnings[0], "RELAY_FIRST_EVENT_TOTAL_TIMEOUT_SECONDS", "warning should explain the phase ordering")
		assert.Contains(t, warnings[1], "RELAY_NON_STREAM_TIMEOUT_SECONDS", "warning should explain the body budget ordering")
		assert.Contains(t, warnings[2], "shortened", "warning should explain the effective first-event cap")
	})

	t.Run("zero disables a guard without warning", func(t *testing.T) {
		assert.Empty(t, relayTimeoutConfigurationWarnings(0, 0, 0, 0))
	})
}
