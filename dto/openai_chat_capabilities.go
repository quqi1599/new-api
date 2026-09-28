package dto

import (
	"strings"
	"time"
)

// OpenAIChatCapabilities is adapted from upstream #7211 and #7559. Model
// identity, sampling support and output limits are independent capabilities.
type OpenAIChatCapabilities struct {
	UseMaxCompletionTokens bool
	UseDeveloperRole       bool
	SupportsTemperature    bool
	SupportsTopP           bool
	SupportsLogProbs       bool
}

// GetOpenAIChatCapabilities takes the mapped model with its effort suffix
// removed. Unknown models retain their fields instead of inheriting GPT rules.
func GetOpenAIChatCapabilities(modelName, effort string) OpenAIChatCapabilities {
	caps := OpenAIChatCapabilities{SupportsTemperature: true, SupportsTopP: true, SupportsLogProbs: true}
	if strings.HasPrefix(modelName, "o1") || strings.HasPrefix(modelName, "o3") || strings.HasPrefix(modelName, "o4") {
		caps.UseMaxCompletionTokens = true
		caps.UseDeveloperRole = !strings.HasPrefix(modelName, "o1-mini") && !strings.HasPrefix(modelName, "o1-preview")
		caps.SupportsTemperature = false
		return caps
	}
	isGPT5 := modelName == "gpt-5" || strings.HasPrefix(modelName, "gpt-5-") || strings.HasPrefix(modelName, "gpt-5.")
	isSolLuna := isOpenAIModelSnapshot(modelName, "gpt-6-sol") || isOpenAIModelSnapshot(modelName, "gpt-6-luna")
	if !isGPT5 && !isSolLuna && !isOpenAIModelSnapshot(modelName, "gpt-6-astra") {
		return caps
	}
	caps.UseMaxCompletionTokens = true
	caps.UseDeveloperRole = true
	sampling := false
	if effort == "" || effort == "none" {
		sampling = isSolLuna
		for _, base := range []string{"gpt-5.1", "gpt-5.2", "gpt-5.4"} {
			if isOpenAIModelSnapshot(modelName, base) {
				sampling = true
				break
			}
		}
	}
	caps.SupportsTemperature = sampling
	caps.SupportsTopP = sampling
	caps.SupportsLogProbs = sampling
	return caps
}

func isOpenAIModelSnapshot(modelName, baseModel string) bool {
	if modelName == baseModel {
		return true
	}
	suffix, ok := strings.CutPrefix(modelName, baseModel+"-")
	if !ok {
		return false
	}
	_, err := time.Parse(time.DateOnly, suffix)
	return err == nil
}
