package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

var reservedBillableToolNames = map[string]struct{}{
	dto.BuildInToolWebSearchPreview: {},
	dto.BuildInToolWebSearch:        {},
	dto.BuildInToolFileSearch:       {},
	dto.BuildInToolGoogleSearch:     {},
	dto.BuildInToolImageGeneration:  {},
}

// CountBillableToolCall records actual completed tool calls. Request tool
// declarations never increment usage.
func (info *RelayInfo) CountBillableToolCall(itemType string, functionName string) {
	if info == nil {
		return
	}
	info.ensureResponsesUsageInfo()
	switch itemType {
	case dto.BuildInCallWebSearchCall:
		info.incrementBillableToolCall(resolveWebSearchToolName(info.ResponsesUsageInfo.BuiltInTools))
	case dto.BuildInCallFileSearchCall:
		info.incrementBillableToolCall(dto.BuildInToolFileSearch)
	case dto.BuildInCallFunctionCall, dto.BuildInCallToolUse:
		functionName = strings.TrimSpace(functionName)
		if functionName == "" {
			return
		}
		if _, reserved := reservedBillableToolNames[functionName]; reserved {
			return
		}
		if operation_setting.GetToolPriceForModel(functionName, info.OriginModelName) <= 0 {
			return
		}
		info.incrementBillableToolCall(functionName)
	}
}

func (info *RelayInfo) ensureResponsesUsageInfo() {
	if info.ResponsesUsageInfo == nil {
		info.ResponsesUsageInfo = &ResponsesUsageInfo{}
	}
	if info.ResponsesUsageInfo.BuiltInTools == nil {
		info.ResponsesUsageInfo.BuiltInTools = make(map[string]*BuildInToolInfo)
	}
}

func resolveWebSearchToolName(tools map[string]*BuildInToolInfo) string {
	if _, ok := tools[dto.BuildInToolWebSearchPreview]; ok {
		return dto.BuildInToolWebSearchPreview
	}
	if _, ok := tools[dto.BuildInToolWebSearch]; ok {
		return dto.BuildInToolWebSearch
	}
	return dto.BuildInToolWebSearchPreview
}

func (info *RelayInfo) incrementBillableToolCall(name string) {
	if existing, ok := info.ResponsesUsageInfo.BuiltInTools[name]; ok && existing != nil {
		existing.CallCount++
		return
	}
	info.ResponsesUsageInfo.BuiltInTools[name] = &BuildInToolInfo{ToolName: name, CallCount: 1}
}

type observedImageGenerationCall struct {
	Quality string
	Size    string
}

// ImageGenerationCallCounter deduplicates stream and final-response views of
// the same completed image_generation_call.
type ImageGenerationCallCounter struct {
	seen  map[string]struct{}
	calls []observedImageGenerationCall
}

func (counter *ImageGenerationCallCounter) Observe(item *dto.ResponsesOutput, outputIndex *int) {
	if counter == nil || item == nil || item.Type != dto.ResponsesOutputTypeImageGenerationCall || strings.TrimSpace(item.Result) == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "failed", "cancelled", "canceled", "incomplete", "partial":
		return
	}

	aliases := make([]string, 0, 4)
	if item.ID != "" {
		aliases = append(aliases, "id:"+item.ID)
	}
	if item.CallId != "" {
		aliases = append(aliases, "call:"+item.CallId)
	}
	if outputIndex != nil && *outputIndex >= 0 {
		aliases = append(aliases, fmt.Sprintf("index:%d", *outputIndex))
	}
	sum := sha256.Sum256([]byte(item.Result))
	aliases = append(aliases, "result:"+hex.EncodeToString(sum[:]))
	if counter.seen == nil {
		counter.seen = make(map[string]struct{})
	}
	for _, alias := range aliases {
		if _, exists := counter.seen[alias]; exists {
			return
		}
	}
	for _, alias := range aliases {
		counter.seen[alias] = struct{}{}
	}
	counter.calls = append(counter.calls, observedImageGenerationCall{Quality: item.Quality, Size: item.Size})
}

func (counter *ImageGenerationCallCounter) Reset() {
	if counter == nil {
		return
	}
	counter.seen = nil
	counter.calls = nil
}

func (counter *ImageGenerationCallCounter) Count() int {
	if counter == nil {
		return 0
	}
	return len(counter.calls)
}

func (counter *ImageGenerationCallCounter) Commit(info *RelayInfo) {
	if info == nil {
		return
	}
	info.ensureResponsesUsageInfo()
	if counter == nil {
		info.ResponsesUsageInfo.ImageGenerationCalls = nil
		return
	}
	count := len(counter.calls)
	if count > dto.MaxImageN {
		count = dto.MaxImageN
	}
	info.ResponsesUsageInfo.ImageGenerationCalls = make([]ImageGenerationCallInfo, 0, count)
	for _, call := range counter.calls[:count] {
		info.ResponsesUsageInfo.ImageGenerationCalls = append(info.ResponsesUsageInfo.ImageGenerationCalls, ImageGenerationCallInfo{
			Quality: call.Quality,
			Size:    call.Size,
		})
	}
}

func IsNonBillableResponsesStatus(status []byte) bool {
	if len(status) == 0 {
		return false
	}
	var value string
	if err := basecommon.Unmarshal(status, &value); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "failed", "cancelled", "canceled", "incomplete":
		return true
	default:
		return false
	}
}

func IsBillableResponsesOutput(item *dto.ResponsesOutput) bool {
	if item == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "failed", "cancelled", "canceled", "incomplete", "partial":
		return false
	default:
		return true
	}
}
