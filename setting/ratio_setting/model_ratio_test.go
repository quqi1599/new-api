package ratio_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGPT5CompletionRatioCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		ratio  float64
		locked bool
	}{
		{name: "gpt-5", ratio: 8, locked: true},
		{name: "gpt-5.1", ratio: 8, locked: true},
		{name: "gpt-5.2-chat", ratio: 8, locked: true},
		{name: "gpt-5.3", ratio: 8, locked: true},
		{name: "gpt-5.4", ratio: 6, locked: true},
		{name: "gpt-5.4-nano", ratio: 6.25, locked: true},
		{name: "gpt-5.5", ratio: 6, locked: false},
		{name: "gpt-5.6-sol", ratio: 6, locked: false},
		{name: "gpt-5.7", ratio: 8, locked: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ratio, locked := getHardcodedCompletionModelRatio(test.name)
			assert.Equal(t, test.ratio, ratio)
			assert.Equal(t, test.locked, locked)
		})
	}
}
