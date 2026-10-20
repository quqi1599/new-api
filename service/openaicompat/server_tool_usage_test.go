package openaicompat

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestServerToolUsageSurvivesChatResponsesConversions(t *testing.T) {
	for _, count := range []*int{nil, common.GetPointer(0), common.GetPointer(3)} {
		src := &dto.Usage{PromptTokens: 10, CompletionTokens: 5, ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: count}}
		responses := responsesUsageFromChat(src)
		chat := UsageFromResponsesUsage(responses)
		require.Equal(t, 15, chat.TotalTokens)
		if count == nil {
			require.Nil(t, responses.ServerToolUse)
			require.Nil(t, chat.ServerToolUse)
		} else {
			require.Equal(t, *count, *chat.ServerToolUse.WebSearchRequests)
			*count = 99
			require.NotEqual(t, 99, *chat.ServerToolUse.WebSearchRequests)
			*responses.ServerToolUse.WebSearchRequests = 88
			require.NotEqual(t, 88, *chat.ServerToolUse.WebSearchRequests)
		}
	}
}
