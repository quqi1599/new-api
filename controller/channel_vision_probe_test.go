package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"testing"
)

func TestChannelVisionProbeUsesRealClaudeConversion(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{Model: "claude-opus-5.5"}
	require.NoError(t, addChannelVisionProbe(req))
	converted, err := (&claude.Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, *req)
	require.NoError(t, err)
	body, err := common.Marshal(converted)
	require.NoError(t, err)
	require.Equal(t, "image", gjson.GetBytes(body, "messages.0.content.0.type").String())
	require.NotEmpty(t, gjson.GetBytes(body, "messages.0.content.0.source.data").String())
	require.Contains(t, gjson.GetBytes(body, "messages.0.content.1.text").String(), "color")
}

func TestChannelVisionAnswerRequiresCorrectText(t *testing.T) {
	for _, body := range []string{`{"content":[{"type":"text","text":"Red."}]}`, `{"output":[{"content":[{"type":"output_text","text":"red"}]}]}`, `{"choices":[{"message":{"content":"Red"}}]}`} {
		require.NoError(t, validateChannelVisionAnswer([]byte(body), false))
	}
	for _, body := range []string{"", `{"content":[{"type":"text","text":"I cannot see the image"}]}`, `{"content":[{"type":"text","text":"blue"}]}`} {
		require.Error(t, validateChannelVisionAnswer([]byte(body), false))
	}
	require.NoError(t, validateChannelVisionAnswer([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Red\"}\n\ndata: [DONE]\n"), true))
	require.Error(t, validateChannelVisionAnswer([]byte("data: [DONE]\n"), true))
}
