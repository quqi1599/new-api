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
