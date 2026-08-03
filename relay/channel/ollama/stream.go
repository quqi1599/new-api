package ollama

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type ollamaChatStreamChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	// chat
	Message *struct {
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		Thinking  json.RawMessage `json:"thinking"`
		ToolCalls []struct {
			Function struct {
				Name      string      `json:"name"`
				Arguments interface{} `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	// generate
	Response           string `json:"response"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason"`
	TotalDuration      int64  `json:"total_duration"`
	LoadDuration       int64  `json:"load_duration"`
	PromptEvalCount    int    `json:"prompt_eval_count"`
	EvalCount          int    `json:"eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalDuration       int64  `json:"eval_duration"`
	Error              string `json:"error,omitempty"`
}

func toUnix(ts string) int64 {
	if ts == "" {
		return time.Now().Unix()
	}
	// try time.RFC3339 or with nanoseconds
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339, ts)
		if err2 == nil {
			return t2.Unix()
		}
		return time.Now().Unix()
	}
	return t.Unix()
}

// ollamaNDJSONDecoder keeps Ollama's one-object-per-line framing intact while
// the shared stream state machine owns cancellation, timeouts, and goroutine
// cleanup. The adapter validates each JSON object before accepting it.
type ollamaNDJSONDecoder struct{}

func (ollamaNDJSONDecoder) Feed(line string) ([]helper.StreamFrame, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	return []helper.StreamFrame{{Kind: "ndjson", Data: line}}, nil
}

func (ollamaNDJSONDecoder) Flush() ([]helper.StreamFrame, error) { return nil, nil }

func ollamaStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("empty response"), types.ErrorCodeBadResponse, http.StatusBadRequest)
	}
	defer service.CloseResponseBodyGracefully(resp)

	usage := &dto.Usage{}
	var model = info.UpstreamModelName
	var responseId = common.GetUUID()
	var created = time.Now().Unix()
	var toolCallIndex int
	var responseText strings.Builder
	started := false

	helper.StreamScannerHandlerWithDecoder(c, resp, info, ollamaNDJSONDecoder{}, func(frame helper.StreamFrame, sr *helper.StreamResult) {
		var chunk ollamaChatStreamChunk
		if err := common.Unmarshal([]byte(frame.Data), &chunk); err != nil {
			logger.LogError(c, "ollama stream json decode error: "+err.Error())
			sr.Stop(err)
			return
		}
		if strings.TrimSpace(chunk.Error) != "" {
			sr.Stop(fmt.Errorf("Ollama stream returned an error event"))
			return
		}
		if !chunk.Done && chunk.Model == "" && chunk.CreatedAt == "" && chunk.Message == nil && chunk.Response == "" {
			sr.Stop(fmt.Errorf("invalid Ollama stream event"))
			return
		}
		if !sr.Accept() {
			return
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.CreatedAt != "" {
			created = toUnix(chunk.CreatedAt)
		}

		if !started {
			start := helper.GenerateStartEmptyResponse(responseId, created, model, nil)
			if err := helper.ObjectData(c, start); err != nil {
				sr.Stop(err)
				return
			}
			started = true
		}

		if !chunk.Done {
			// delta content
			var content string
			if chunk.Message != nil {
				content = chunk.Message.Content
			} else {
				content = chunk.Response
			}
			delta := dto.ChatCompletionsStreamResponse{
				Id:      responseId,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Index: 0,
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Role: "assistant"},
				}},
			}
			if content != "" {
				delta.Choices[0].Delta.SetContentString(content)
				responseText.WriteString(content)
			}
			if chunk.Message != nil && len(chunk.Message.Thinking) > 0 {
				raw := strings.TrimSpace(string(chunk.Message.Thinking))
				if raw != "" && raw != "null" {
					// Unmarshal the JSON string to get the actual content without quotes
					var thinkingContent string
					if err := common.Unmarshal(chunk.Message.Thinking, &thinkingContent); err == nil {
						delta.Choices[0].Delta.SetReasoningContent(thinkingContent)
					} else {
						// Fallback to raw string if it's not a JSON string
						delta.Choices[0].Delta.SetReasoningContent(raw)
					}
				}
			}
			// tool calls
			if chunk.Message != nil && len(chunk.Message.ToolCalls) > 0 {
				delta.Choices[0].Delta.ToolCalls = make([]dto.ToolCallResponse, 0, len(chunk.Message.ToolCalls))
				for _, tc := range chunk.Message.ToolCalls {
					// arguments -> string
					argBytes, _ := common.Marshal(tc.Function.Arguments)
					toolId := fmt.Sprintf("call_%d", toolCallIndex)
					tr := dto.ToolCallResponse{ID: toolId, Type: "function", Function: dto.FunctionResponse{Name: tc.Function.Name, Arguments: string(argBytes)}}
					tr.SetIndex(toolCallIndex)
					toolCallIndex++
					delta.Choices[0].Delta.ToolCalls = append(delta.Choices[0].Delta.ToolCalls, tr)
				}
			}
			if err := helper.ObjectData(c, delta); err != nil {
				sr.Stop(err)
			}
			return
		}

		// Ollama's done:true object is the only successful terminal marker.
		usage.PromptTokens = chunk.PromptEvalCount
		usage.CompletionTokens = chunk.EvalCount
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		finishReason := chunk.DoneReason
		if finishReason == "" {
			finishReason = "stop"
		}
		// emit stop delta
		if stop := helper.GenerateStopResponse(responseId, created, model, finishReason); stop != nil {
			if err := helper.ObjectData(c, stop); err != nil {
				sr.Stop(err)
				return
			}
		}
		// emit usage frame
		if final := helper.GenerateFinalUsageResponse(responseId, created, model, *usage); final != nil {
			if err := helper.ObjectData(c, final); err != nil {
				sr.Stop(err)
				return
			}
		}
		sr.Done()
	})

	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return nil, streamErr
	}
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		usage = service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	if helper.ShouldFinalizeStream(info) {
		helper.Done(c)
	}
	return usage, nil
}

// non-stream handler for chat/generate
func ollamaChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)
	raw := string(body)
	if common.DebugEnabled {
		println("ollama non-stream raw resp:", raw)
	}

	lines := strings.Split(raw, "\n")
	var (
		aggContent       strings.Builder
		reasoningBuilder strings.Builder
		lastChunk        ollamaChatStreamChunk
		parsedAny        bool
	)
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var ck ollamaChatStreamChunk
		if err := json.Unmarshal([]byte(ln), &ck); err != nil {
			if len(lines) == 1 {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			continue
		}
		parsedAny = true
		lastChunk = ck
		if ck.Message != nil && len(ck.Message.Thinking) > 0 {
			raw := strings.TrimSpace(string(ck.Message.Thinking))
			if raw != "" && raw != "null" {
				// Unmarshal the JSON string to get the actual content without quotes
				var thinkingContent string
				if err := json.Unmarshal(ck.Message.Thinking, &thinkingContent); err == nil {
					reasoningBuilder.WriteString(thinkingContent)
				} else {
					// Fallback to raw string if it's not a JSON string
					reasoningBuilder.WriteString(raw)
				}
			}
		}
		if ck.Message != nil && ck.Message.Content != "" {
			aggContent.WriteString(ck.Message.Content)
		} else if ck.Response != "" {
			aggContent.WriteString(ck.Response)
		}
	}

	if !parsedAny {
		var single ollamaChatStreamChunk
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		lastChunk = single
		if single.Message != nil {
			if len(single.Message.Thinking) > 0 {
				raw := strings.TrimSpace(string(single.Message.Thinking))
				if raw != "" && raw != "null" {
					// Unmarshal the JSON string to get the actual content without quotes
					var thinkingContent string
					if err := json.Unmarshal(single.Message.Thinking, &thinkingContent); err == nil {
						reasoningBuilder.WriteString(thinkingContent)
					} else {
						// Fallback to raw string if it's not a JSON string
						reasoningBuilder.WriteString(raw)
					}
				}
			}
			aggContent.WriteString(single.Message.Content)
		} else {
			aggContent.WriteString(single.Response)
		}
	}

	model := lastChunk.Model
	if model == "" {
		model = info.UpstreamModelName
	}
	created := toUnix(lastChunk.CreatedAt)
	usage := &dto.Usage{PromptTokens: lastChunk.PromptEvalCount, CompletionTokens: lastChunk.EvalCount, TotalTokens: lastChunk.PromptEvalCount + lastChunk.EvalCount}
	content := aggContent.String()
	finishReason := lastChunk.DoneReason
	if finishReason == "" {
		finishReason = "stop"
	}

	msg := dto.Message{Role: "assistant", Content: contentPtr(content)}
	if rc := reasoningBuilder.String(); rc != "" {
		msg.ReasoningContent = &rc
	}
	full := dto.OpenAITextResponse{
		Id:      common.GetUUID(),
		Model:   model,
		Object:  "chat.completion",
		Created: created,
		Choices: []dto.OpenAITextResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: *usage,
	}
	out, _ := common.Marshal(full)
	service.IOCopyBytesGracefully(c, resp, out)
	return usage, nil
}

func contentPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
