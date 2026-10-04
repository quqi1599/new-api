package claude

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

const fixturePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func TestResponsesClaudePreservesMultimodalContent(t *testing.T) {
	for _, model := range []string{"claude-opus-5.5", "claude-sonnet-4-6"} {
		for _, tc := range []struct {
			name     string
			content  any
			wantType string
			wantData string
			wantText string
		}{
			{"image_only", []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + fixturePNG}}, "image", fixturePNG, ""},
			{"image_and_text", []any{map[string]any{"type": "input_text", "text": "describe"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + fixturePNG}}, "image", fixturePNG, "describe"},
			{"image_object", []any{map[string]any{"type": "input_image", "image_url": map[string]any{"url": "data:image/png;base64," + fixturePNG}}}, "image", fixturePNG, ""},
			{"pdf", []any{map[string]any{"type": "input_file", "filename": "a.pdf", "file_data": "JVBERi0xLjQK"}}, "document", "JVBERi0xLjQK", ""},
			{"text_file", []any{map[string]any{"type": "input_file", "filename": "a.txt", "file_data": base64.StdEncoding.EncodeToString([]byte("file contents"))}}, "text", "", "file contents"},
		} {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				input, err := common.Marshal([]any{map[string]any{"role": "user", "content": tc.content}})
				require.NoError(t, err)
				result, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{Model: model, Input: input})
				require.NoError(t, err)
				req := result.(*dto.ClaudeRequest)
				require.Len(t, req.Messages, 1)
				parts, err := common.Any2Type[[]dto.ClaudeMediaMessage](req.Messages[0].Content)
				require.NoError(t, err)
				found, foundText := false, tc.wantText == ""
				for _, part := range parts {
					if part.Type == tc.wantType && (tc.wantData == "" || part.Source != nil && part.Source.Data == tc.wantData) {
						found = true
					}
					if part.Text != nil && *part.Text == tc.wantText {
						foundText = true
					}
				}
				require.True(t, found, "expected media missing: %#v", parts)
				require.True(t, foundText, "text lost")
			})
		}
	}
}

func TestResponsesClaudeToolImageBatch(t *testing.T) {
	input, err := common.Marshal([]any{
		map[string]any{"type": "function_call", "call_id": "one", "name": "screenshot", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "one", "output": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + fixturePNG}}},
		map[string]any{"role": "user", "content": "describe the screenshot"},
	})
	require.NoError(t, err)
	result, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{Model: "claude-opus-5.5", Input: input})
	require.NoError(t, err)
	images, results := 0, 0
	for _, m := range result.(*dto.ClaudeRequest).Messages {
		parts, _ := common.Any2Type[[]dto.ClaudeMediaMessage](m.Content)
		for _, p := range parts {
			if p.Type == "image" {
				images++
				require.Equal(t, fixturePNG, p.Source.Data)
			}
			if p.Type == "tool_result" {
				results++
			}
		}
	}
	require.Equal(t, 1, images)
	require.Equal(t, 1, results)
}

func TestResponsesClaudeRejectsUnresolvableMedia(t *testing.T) {
	for _, part := range []map[string]any{
		{"type": "input_image", "file_id": "file-private-id"},
		{"type": "input_image", "image_url": ""},
		{"type": "input_file", "file_id": "file-private-id"},
		{"type": "input_file", "filename": "a.bin", "file_data": "YWJj"},
		{"type": "input_file", "filename": "a.txt", "file_data": "//4="},
		{"type": "input_audio", "input_audio": map[string]any{"format": "wav", "data": "YWJj"}},
	} {
		input, err := common.Marshal([]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "do not drop my attachment"}, part}}})
		require.NoError(t, err)
		_, err = (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{Model: "claude-opus-5.5", Input: input})
		var apiErr *types.NewAPIError
		require.True(t, errors.As(err, &apiErr), "%v", err)
		require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
		require.True(t, types.IsSkipRetryError(apiErr))
		require.NotContains(t, err.Error(), "file-private-id")
	}
}

func TestChatAndNativeClaudeImagesRemainSupported(t *testing.T) {
	var chat dto.GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"model":"claude-opus-5.5","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,`+fixturePNG+`"}}]}]}`, &chat))
	converted, err := RequestOpenAI2ClaudeMessage(nil, chat)
	require.NoError(t, err)
	parts, _ := common.Any2Type[[]dto.ClaudeMediaMessage](converted.Messages[0].Content)
	require.Len(t, parts, 1)
	require.Equal(t, fixturePNG, parts[0].Source.Data)
	native, err := (&Adaptor{}).ConvertClaudeRequest(nil, nil, converted)
	require.NoError(t, err)
	require.Same(t, converted, native)
}

func TestResponsesClaudeURLImage(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString(fixturePNG)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer server.Close()
	settings := system_setting.GetFetchSetting()
	old := *settings
	oldMax := constant.MaxFileDownloadMB
	t.Cleanup(func() { *settings = old; constant.MaxFileDownloadMB = oldMax })
	settings.EnableSSRFProtection = false // Only the loopback fixture in this test.
	constant.MaxFileDownloadMB = 1
	service.InitHttpClient()
	input, err := common.Marshal([]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": server.URL + "/red.png"}}}})
	require.NoError(t, err)
	result, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{Model: "claude-opus-5.5", Input: input})
	require.NoError(t, err)
	parts, err := common.Any2Type[[]dto.ClaudeMediaMessage](result.(*dto.ClaudeRequest).Messages[0].Content)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, fixturePNG, parts[0].Source.Data)
}
