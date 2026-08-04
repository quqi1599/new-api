package common

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestCountBillableToolCallCountsOnlyActualSupportedCalls(t *testing.T) {
	info := &RelayInfo{
		OriginModelName: "gpt-4.1",
		ResponsesUsageInfo: &ResponsesUsageInfo{BuiltInTools: map[string]*BuildInToolInfo{
			dto.BuildInToolWebSearch:  {ToolName: dto.BuildInToolWebSearch},
			dto.BuildInToolFileSearch: {ToolName: dto.BuildInToolFileSearch},
		}},
	}

	info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
	info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
	info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
	info.CountBillableToolCall(dto.BuildInCallFunctionCall, dto.BuildInToolWebSearch)
	info.CountBillableToolCall(dto.BuildInCallFunctionCall, "unpriced_custom_tool")

	require.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount)
	require.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
	require.NotContains(t, info.ResponsesUsageInfo.BuiltInTools, "unpriced_custom_tool")
}

func TestImageGenerationCallCounterDeduplicatesFiltersAndCaps(t *testing.T) {
	counter := &ImageGenerationCallCounter{}
	index := 0
	counter.Observe(&dto.ResponsesOutput{
		Type: dto.ResponsesOutputTypeImageGenerationCall, ID: "image-1", Status: "completed",
		Result: "base64-image-1", Quality: "low", Size: "1024x1024",
	}, &index)
	counter.Observe(&dto.ResponsesOutput{
		Type: dto.ResponsesOutputTypeImageGenerationCall, ID: "image-1", Status: "completed",
		Result: "base64-image-1", Quality: "low", Size: "1024x1024",
	}, &index)
	counter.Observe(&dto.ResponsesOutput{
		Type: dto.ResponsesOutputTypeImageGenerationCall, ID: "failed-image", Status: "failed",
		Result: "base64-failed",
	}, nil)
	counter.Observe(&dto.ResponsesOutput{
		Type: dto.ResponsesOutputTypeImageGenerationCall, ID: "empty-image", Status: "completed",
	}, nil)
	for i := 0; i < dto.MaxImageN+2; i++ {
		counter.Observe(&dto.ResponsesOutput{
			Type: dto.ResponsesOutputTypeImageGenerationCall, ID: fmt.Sprintf("image-extra-%d", i),
			Status: "completed", Result: fmt.Sprintf("base64-extra-%d", i), Quality: "high", Size: "1024x1024",
		}, nil)
	}

	info := &RelayInfo{}
	counter.Commit(info)

	require.Len(t, info.ResponsesUsageInfo.ImageGenerationCalls, dto.MaxImageN)
	require.Equal(t, ImageGenerationCallInfo{Quality: "low", Size: "1024x1024"}, info.ResponsesUsageInfo.ImageGenerationCalls[0])
}

func TestResponsesBillabilityStatus(t *testing.T) {
	require.True(t, IsNonBillableResponsesStatus([]byte(`"incomplete"`)))
	require.True(t, IsNonBillableResponsesStatus([]byte(`"cancelled"`)))
	require.False(t, IsNonBillableResponsesStatus([]byte(`"completed"`)))
	require.False(t, IsBillableResponsesOutput(&dto.ResponsesOutput{Status: "partial"}))
	require.True(t, IsBillableResponsesOutput(&dto.ResponsesOutput{Status: "completed"}))
}
