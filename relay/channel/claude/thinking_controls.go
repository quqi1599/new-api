package claude

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

// Preserve native thinking controls when Chat requests use a Claude-compatible
// route. An explicit off switch wins over an effort or budget setting.
func applyOpenAIThinkingControls(request dto.GeneralOpenAIRequest, output *dto.ClaudeRequest) error {
	var native *dto.Thinking
	if len(request.THINKING) > 0 {
		if err := common.Unmarshal(request.THINKING, &native); err != nil {
			return err
		}
	}
	var enabled *bool
	if len(request.EnableThinking) > 0 {
		if err := common.Unmarshal(request.EnableThinking, &enabled); err != nil {
			return err
		}
	}
	disabled := strings.EqualFold(strings.TrimSpace(request.ReasoningEffort), "none") || (enabled != nil && !*enabled)
	if native != nil {
		switch strings.ToLower(strings.TrimSpace(native.Type)) {
		case "disabled", "off", "none":
			disabled = true
		}
		output.Thinking = native
	}
	if disabled {
		output.Thinking = &dto.Thinking{Type: "disabled"}
		output.OutputConfig = nil
	}
	return nil
}
