package claude

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestThinkingControlsAcrossClaudeConversions(t *testing.T) {
	for _, model := range []string{"qwen3.8-max", "qwen3.8-flash", "kimi-k2.6", "glm-5.2", "deepseek-v4-pro", "doubao-seed-2.0-pro"} {
		for _, format := range []string{"chat", "responses"} {
			for _, fields := range []string{
				`"enable_thinking":false`,
				`"thinking":{"type":"disabled","budget_tokens":2048}`,
				`"thinking":{"type":"disabled"},"enable_thinking":true`,
			} {
				t.Run(model+"/"+format+"/"+fields, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					var got *dto.ClaudeRequest
					if format == "chat" {
						var req dto.GeneralOpenAIRequest
						require.NoError(t, common.UnmarshalJsonStr(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],%s}`, model, fields), &req))
						var err error
						got, err = RequestOpenAI2ClaudeMessage(c, req)
						require.NoError(t, err)
					} else {
						var req dto.OpenAIResponsesRequest
						require.NoError(t, common.UnmarshalJsonStr(fmt.Sprintf(`{"model":%q,"input":"hi",%s}`, model, fields), &req))
						result, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, nil, req)
						require.NoError(t, err)
						got = result.(*dto.ClaudeRequest)
					}
					require.Equal(t, &dto.Thinking{Type: "disabled"}, got.Thinking)
					require.Empty(t, got.OutputConfig)
				})
			}
		}
	}
}

func TestThinkingControlsPrecedenceAndMissingValues(t *testing.T) {
	for _, tt := range []struct{ fields, want string }{
		{`"reasoning_effort":"none","reasoning":{"max_tokens":2048}`, "disabled"},
		{`"enable_thinking":false,"reasoning_effort":"high"`, "disabled"},
		{`"thinking":{"type":"disabled"},"reasoning_effort":"high"`, "disabled"},
		{`"thinking":{"type":"enabled","budget_tokens":2048}`, "enabled"},
		{`"enable_thinking":null,"thinking":null`, ""},
		{`"reasoning_effort":"low"`, "enabled"},
	} {
		t.Run(tt.fields, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			var req dto.GeneralOpenAIRequest
			require.NoError(t, common.UnmarshalJsonStr(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],`+tt.fields+`}`, &req))
			got, err := RequestOpenAI2ClaudeMessage(c, req)
			require.NoError(t, err)
			if tt.want == "" {
				require.Nil(t, got.Thinking)
			} else {
				require.Equal(t, tt.want, got.Thinking.Type)
			}
		})
	}
}

func TestResponsesEffortNoneReachesClaude(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	var req dto.OpenAIResponsesRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"model":"qwen3.8-max","input":"hi","reasoning":{"effort":"none"}}`, &req))
	result, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, nil, req)
	require.NoError(t, err)
	require.Equal(t, &dto.Thinking{Type: "disabled"}, result.(*dto.ClaudeRequest).Thinking)
}
