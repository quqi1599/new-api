package types

import (
	"strings"

	"github.com/QuantumNous/new-api/constant"
)

type RelayFormat string

const (
	RelayFormatOpenAI                    RelayFormat = "openai"
	RelayFormatClaude                                = "claude"
	RelayFormatGemini                                = "gemini"
	RelayFormatOpenAIResponses                       = "openai_responses"
	RelayFormatOpenAIResponsesCompaction             = "openai_responses_compaction"
	RelayFormatOpenAIAudio                           = "openai_audio"
	RelayFormatOpenAIImage                           = "openai_image"
	RelayFormatOpenAIRealtime                        = "openai_realtime"
	RelayFormatRerank                                = "rerank"
	RelayFormatEmbedding                             = "embedding"

	RelayFormatTask    = "task"
	RelayFormatMjProxy = "mj_proxy"
)

// RelayFormatToPreferredChannelTypes returns the native/preferred channel types
// for a given relay format. When multiple channels support the same model,
// channels matching these types are prioritized during selection.
func RelayFormatToPreferredChannelTypes(format RelayFormat) []int {
	switch format {
	case RelayFormatOpenAI, RelayFormatOpenAIAudio, RelayFormatOpenAIImage, RelayFormatOpenAIRealtime:
		return []int{constant.ChannelTypeOpenAI}
	case RelayFormatClaude:
		return []int{constant.ChannelTypeAnthropic}
	case RelayFormatGemini:
		return []int{constant.ChannelTypeGemini}
	case RelayFormatOpenAIResponses, RelayFormatOpenAIResponsesCompaction:
		return []int{constant.ChannelTypeOpenAI, constant.ChannelTypeCodex}
	}
	return nil
}

// PathToPreferredChannelTypes derives preferred channel types from the request URL path.
// Used in middleware where RelayFormat is not yet available.
func PathToPreferredChannelTypes(path string) []int {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return []int{constant.ChannelTypeAnthropic}
	case strings.HasPrefix(path, "/v1beta/models"):
		return []int{constant.ChannelTypeGemini}
	case strings.HasPrefix(path, "/v1/responses"):
		return []int{constant.ChannelTypeOpenAI, constant.ChannelTypeCodex}
	case isOpenAIPath(path):
		return []int{constant.ChannelTypeOpenAI}
	}
	return nil
}

func isOpenAIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/moderations") ||
		strings.HasPrefix(path, "/v1/images/") ||
		strings.HasPrefix(path, "/v1/edits") ||
		strings.HasPrefix(path, "/v1/audio/") ||
		strings.HasPrefix(path, "/v1/realtime")
}
