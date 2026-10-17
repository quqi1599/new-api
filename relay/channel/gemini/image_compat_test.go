package gemini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// PR #6371 registers the GA names so Chat requests actually enable image output.
func TestGeminiImageGAChatRequestWire(t *testing.T) {
	for _, model := range []string{
		"gemini-3-pro-image", "gemini-3.1-flash-image",
		"gemini-3-pro-image-preview", "gemini-3.1-flash-image-preview", "gemini-2.5-flash-image",
	} {
		for _, channelType := range []int{constant.ChannelTypeGemini, constant.ChannelTypeVertexAi} {
			t.Run(model+"/"+constant.GetChannelTypeName(channelType), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: channelType, UpstreamModelName: model,
				}}
				var request dto.GeneralOpenAIRequest
				require.NoError(t, common.Unmarshal([]byte(`{"messages":[{"role":"user","content":"Draw a blue circle"}],"extra_body":{"google":{"image_config":{"aspect_ratio":"16:9","image_size":"4K"}}}}`), &request))
				converted, err := (&Adaptor{}).ConvertOpenAIRequest(c, info, &request)
				require.NoError(t, err)
				body, err := common.Marshal(converted)
				require.NoError(t, err)
				var wire struct {
					GenerationConfig struct {
						ResponseModalities []string `json:"responseModalities"`
						ImageConfig        struct {
							AspectRatio string `json:"aspectRatio"`
							ImageSize   string `json:"imageSize"`
						} `json:"imageConfig"`
					} `json:"generationConfig"`
					Contents []dto.GeminiChatContent `json:"contents"`
				}
				require.NoError(t, common.Unmarshal(body, &wire))
				require.Equal(t, []string{"TEXT", "IMAGE"}, wire.GenerationConfig.ResponseModalities)
				require.Equal(t, "16:9", wire.GenerationConfig.ImageConfig.AspectRatio)
				require.Equal(t, "4K", wire.GenerationConfig.ImageConfig.ImageSize)
				require.Len(t, wire.Contents, 1)
				require.Equal(t, "Draw a blue circle", wire.Contents[0].Parts[0].Text)
				require.Contains(t, (&Adaptor{}).GetModelList(), model)
			})
		}
	}
}

func TestGeminiImageCompatibilityPreservesOtherTransports(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: constant.ChannelTypeGemini, ChannelBaseUrl: "https://example.invalid", UpstreamModelName: "gemini-2.5-flash",
	}}
	plain, err := CovertOpenAI2Gemini(c, dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "user", Content: "hello"}}}, info)
	require.NoError(t, err)
	require.Empty(t, plain.GenerationConfig.ResponseModalities)

	info.UpstreamModelName = "gemini-3-pro-image"
	var native dto.GeminiChatRequest
	require.NoError(t, common.Unmarshal([]byte(`{"contents":[{"role":"user","parts":[{"text":"Draw a circle"},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"3:2","imageSize":"2K"}}}`), &native))
	before, err := common.Marshal(&native)
	require.NoError(t, err)
	converted, err := adaptor.ConvertGeminiRequest(c, info, &native)
	require.NoError(t, err)
	after, err := common.Marshal(converted)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	url, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	require.Equal(t, "https://example.invalid/v1beta/models/gemini-3-pro-image:generateContent", url)
	_, err = adaptor.ConvertImageRequest(c, info, dto.ImageRequest{Prompt: "Draw a circle"})
	require.ErrorContains(t, err, "only imagen models are supported")

	info.UpstreamModelName = "imagen-4.0-generate-001"
	imagen, err := adaptor.ConvertImageRequest(c, info, dto.ImageRequest{Prompt: "Draw a circle", Size: "1792x1024", N: common.GetPointer(uint(2))})
	require.NoError(t, err)
	require.Equal(t, dto.GeminiImageRequest{
		Instances:  []dto.GeminiImageInstance{{Prompt: "Draw a circle"}},
		Parameters: dto.GeminiImageParameters{SampleCount: 2, AspectRatio: "16:9", PersonGeneration: "allow_adult"},
	}, imagen)
	url, err = adaptor.GetRequestURL(info)
	require.NoError(t, err)
	require.Equal(t, "https://example.invalid/v1beta/models/imagen-4.0-generate-001:predict", url)
}

func TestGeminiImageCompatibilityRespectsCustomModelList(t *testing.T) {
	settings := model_setting.GetGeminiSettings()
	previous := settings.SupportedImagineModels
	settings.SupportedImagineModels = []string{"custom-image-model"}
	t.Cleanup(func() { settings.SupportedImagineModels = previous })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request := dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "user", Content: "Draw a circle"}}}
	for _, model := range []string{"gemini-3-pro-image", "custom-image-model"} {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: model}}
		converted, err := CovertOpenAI2Gemini(c, request, info)
		require.NoError(t, err)
		if model == "custom-image-model" {
			require.Equal(t, []string{"TEXT", "IMAGE"}, converted.GenerationConfig.ResponseModalities)
		} else {
			require.Empty(t, converted.GenerationConfig.ResponseModalities)
		}
	}
}
