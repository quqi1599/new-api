package claude

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBatch1ClaudeStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop any
		want []string
		bad  bool
	}{
		{"absent", nil, nil, false},
		{"string", "END", []string{"END"}, false},
		{"strings", []string{"END", ""}, []string{"END", ""}, false},
		{"json array", []any{"END", ""}, []string{"END", ""}, false},
		{"mixed", []any{"END", 1}, nil, true},
		{"null item", []any{nil}, nil, true},
		{"object", map[string]any{"bad": true}, nil, true},
		{"number", 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			got, err := RequestOpenAI2ClaudeMessage(c, dto.GeneralOpenAIRequest{Model: "fixture", Stop: tc.stop, Messages: []dto.Message{{Role: "user", Content: "hi"}}})
			if tc.bad {
				require.Error(t, err)
				wrapped := types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
				require.Equal(t, 400, wrapped.StatusCode)
				require.True(t, types.IsSkipRetryError(wrapped))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.StopSequences)
		})
	}
}

func TestBatch1ClaudeAdjacentTools(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, third, want string
		two                              bool
	}{
		{"two tool calls", "", "", "", "...", true},
		{"tool then text", "", "checking", "", "checking", false},
		{"literal ellipsis", "...", "checking", "", "... checking", true},
		{"repeated merge", "", "...", "checking", "... checking", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := dto.Message{Role: "assistant", Content: tc.first}
			first.SetToolCalls([]dto.ToolCallRequest{{ID: "a", Type: "function", Function: dto.FunctionRequest{Name: "lookup", Arguments: `{}`}}})
			second := dto.Message{Role: "assistant", Content: tc.second}
			if tc.two {
				second.SetToolCalls([]dto.ToolCallRequest{{ID: "b", Type: "function", Function: dto.FunctionRequest{Name: "lookup", Arguments: `{}`}}})
			}
			messages := []dto.Message{{Role: "user", Content: "hi"}, first, second}
			if tc.third != "" {
				messages = append(messages, dto.Message{Role: "assistant", Content: tc.third})
			}
			messages = append(messages, dto.Message{Role: "tool", ToolCallId: "a", Content: "result"})
			if tc.two {
				messages = append(messages, dto.Message{Role: "tool", ToolCallId: "b", Content: "result"})
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			got, err := RequestOpenAI2ClaudeMessage(c, dto.GeneralOpenAIRequest{Model: "fixture", Messages: messages})
			require.NoError(t, err)
			wire, err := common.Marshal(got.Messages[1].Content)
			require.NoError(t, err)
			var blocks []dto.ClaudeMediaMessage
			require.NoError(t, common.Unmarshal(wire, &blocks))
			var ids []string
			var text string
			for _, block := range blocks {
				if block.Type == "tool_use" {
					ids = append(ids, block.Id)
				}
				if block.Type == "text" {
					text += block.GetText()
				}
			}
			wantIDs := []string{"a"}
			if tc.two {
				wantIDs = append(wantIDs, "b")
			}
			require.Equal(t, wantIDs, ids)
			require.Equal(t, tc.want, text)
		})
	}
}
