package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

// ResponsesRequestToChatCompletionsRequest converts the subset shared by the
// Responses and Chat Completions APIs. It is intentionally provider-neutral;
// Claude-specific defaults and media loading remain in the Claude adaptor.
func ResponsesRequestToChatCompletionsRequest(req *dto.OpenAIResponsesRequest) (*dto.GeneralOpenAIRequest, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	unsupported := make([]string, 0, 4)
	if responsesRawPresent(req.Conversation) {
		unsupported = append(unsupported, "conversation")
	}
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		unsupported = append(unsupported, "previous_response_id")
	}
	if responsesRawPresent(req.Prompt) {
		unsupported = append(unsupported, "prompt")
	}
	if responsesRawPresent(req.ContextManagement) {
		unsupported = append(unsupported, "context_management")
	}
	if len(unsupported) > 0 {
		return nil, fmt.Errorf("responses to chat conversion does not support stateful fields: %s", strings.Join(unsupported, ", "))
	}

	out := &dto.GeneralOpenAIRequest{
		Model:          req.Model,
		Stream:         req.Stream,
		MaxTokens:      req.MaxOutputTokens,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		Metadata:       req.Metadata,
		EnableThinking: req.EnableThinking,
		ThinkingBudget: req.ThinkingBudget,
		THINKING:       req.THINKING,
	}
	if req.Reasoning != nil {
		out.ReasoningEffort = req.Reasoning.Effort
	}
	if err := decodeResponsesParallelToolCalls(req.ParallelToolCalls, &out.ParallelTooCalls); err != nil {
		return nil, err
	}
	if err := decodeResponsesTools(req.Tools, &out.Tools); err != nil {
		return nil, err
	}
	toolChoice, err := decodeResponsesToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = toolChoice

	if len(req.Instructions) > 0 {
		var instructions string
		if err := common.Unmarshal(req.Instructions, &instructions); err != nil {
			return nil, fmt.Errorf("invalid instructions: %w", err)
		}
		if strings.TrimSpace(instructions) != "" {
			message := dto.Message{Role: "system"}
			message.SetStringContent(instructions)
			out.Messages = append(out.Messages, message)
		}
	}

	if len(req.Input) == 0 {
		return out, nil
	}
	var input any
	if err := common.Unmarshal(req.Input, &input); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	switch value := input.(type) {
	case string:
		message := dto.Message{Role: "user"}
		message.SetStringContent(value)
		out.Messages = append(out.Messages, message)
	case []any:
		for index, item := range value {
			itemMap, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("input item %d must be an object", index)
			}
			if err := appendResponsesInputItem(&out.Messages, itemMap); err != nil {
				return nil, fmt.Errorf("input item %d: %w", index, err)
			}
		}
	default:
		return nil, fmt.Errorf("input must be a string or array")
	}

	return out, nil
}

func responsesRawPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func decodeResponsesParallelToolCalls(raw json.RawMessage, target **bool) error {
	if len(raw) == 0 {
		return nil
	}
	var value bool
	if err := common.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("invalid parallel_tool_calls: %w", err)
	}
	*target = common.GetPointer(value)
	return nil
}

func decodeResponsesTools(raw json.RawMessage, target *[]dto.ToolCallRequest) error {
	if len(raw) == 0 {
		return nil
	}
	var tools []map[string]any
	if err := common.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("invalid tools: %w", err)
	}
	for index, tool := range tools {
		toolType := strings.TrimSpace(common.Interface2String(tool["type"]))
		if toolType != "function" {
			continue
		}
		name := strings.TrimSpace(common.Interface2String(tool["name"]))
		description := common.Interface2String(tool["description"])
		parameters := tool["parameters"]
		if function, ok := tool["function"].(map[string]any); ok {
			if name == "" {
				name = strings.TrimSpace(common.Interface2String(function["name"]))
			}
			if description == "" {
				description = common.Interface2String(function["description"])
			}
			if parameters == nil {
				parameters = function["parameters"]
			}
		}
		if name == "" {
			return fmt.Errorf("tool %d is missing name", index)
		}
		*target = append(*target, dto.ToolCallRequest{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        name,
				Description: description,
				Parameters:  parameters,
			},
		})
	}
	return nil
}

func decodeResponsesToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := common.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	choice, ok := value.(map[string]any)
	if !ok || strings.TrimSpace(common.Interface2String(choice["type"])) != "function" {
		return value, nil
	}
	name := strings.TrimSpace(common.Interface2String(choice["name"]))
	if name == "" {
		return value, nil
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": name},
	}, nil
}

func appendResponsesInputItem(messages *[]dto.Message, item map[string]any) error {
	itemType := strings.TrimSpace(common.Interface2String(item["type"]))
	switch itemType {
	case "function_call", "custom_tool_call":
		return appendResponsesFunctionCall(messages, item, itemType)
	case "function_call_output", "custom_tool_call_output":
		return appendResponsesFunctionOutput(messages, item)
	}

	role := strings.TrimSpace(common.Interface2String(item["role"]))
	switch role {
	case "assistant", "system", "developer", "user":
	default:
		role = "user"
	}
	if role == "developer" {
		role = "system"
	}
	message := dto.Message{Role: role}
	if err := setResponsesMessageContent(&message, item["content"]); err != nil {
		return err
	}
	*messages = append(*messages, message)
	return nil
}

func appendResponsesFunctionCall(messages *[]dto.Message, item map[string]any, itemType string) error {
	callID := strings.TrimSpace(common.Interface2String(item["call_id"]))
	if callID == "" {
		callID = strings.TrimSpace(common.Interface2String(item["id"]))
	}
	name := strings.TrimSpace(common.Interface2String(item["name"]))
	if callID == "" || name == "" {
		return errors.New("function call is missing call_id or name")
	}
	argumentKey := "arguments"
	if itemType == "custom_tool_call" {
		argumentKey = "input"
	}
	arguments, err := responsesValueString(item[argumentKey])
	if err != nil {
		return fmt.Errorf("invalid function arguments: %w", err)
	}
	toolCall := dto.ToolCallRequest{
		ID:   callID,
		Type: "function",
		Function: dto.FunctionRequest{
			Name:      name,
			Arguments: arguments,
		},
	}

	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == "assistant" {
		last := &(*messages)[len(*messages)-1]
		toolCalls := last.ParseToolCalls()
		toolCalls = append(toolCalls, toolCall)
		last.SetToolCalls(toolCalls)
		return nil
	}
	message := dto.Message{Role: "assistant"}
	message.SetNullContent()
	message.SetToolCalls([]dto.ToolCallRequest{toolCall})
	*messages = append(*messages, message)
	return nil
}

func appendResponsesFunctionOutput(messages *[]dto.Message, item map[string]any) error {
	callID := strings.TrimSpace(common.Interface2String(item["call_id"]))
	if callID == "" {
		callID = strings.TrimSpace(common.Interface2String(item["id"]))
	}
	if callID == "" {
		return errors.New("function output is missing call_id")
	}
	output, err := responsesValueString(item["output"])
	if err != nil {
		return fmt.Errorf("invalid function output: %w", err)
	}
	message := dto.Message{Role: "tool", ToolCallId: callID}
	message.SetStringContent(output)
	*messages = append(*messages, message)
	return nil
}

func setResponsesMessageContent(message *dto.Message, value any) error {
	if value == nil {
		message.SetStringContent("")
		return nil
	}
	if text, ok := value.(string); ok {
		message.SetStringContent(text)
		return nil
	}
	parts, ok := value.([]any)
	if !ok {
		return errors.New("message content must be a string or array")
	}
	media := make([]dto.MediaContent, 0, len(parts))
	for _, partValue := range parts {
		part, ok := partValue.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(common.Interface2String(part["type"])) {
		case "input_text", "output_text", "text":
			media = append(media, dto.MediaContent{Type: dto.ContentTypeText, Text: common.Interface2String(part["text"])})
		case "input_image":
			media = append(media, dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: normalizeResponsesImageURL(part["image_url"], part["detail"])})
		case "input_file":
			file := map[string]any{}
			for _, key := range []string{"filename", "file_data", "file_id"} {
				if part[key] != nil {
					file[key] = part[key]
				}
			}
			media = append(media, dto.MediaContent{Type: dto.ContentTypeFile, File: file})
		case "input_audio":
			media = append(media, dto.MediaContent{Type: dto.ContentTypeInputAudio, InputAudio: part["input_audio"]})
		}
	}
	if len(media) == 1 && media[0].Type == dto.ContentTypeText {
		message.SetStringContent(media[0].Text)
		return nil
	}
	message.SetMediaContent(media)
	return nil
}

func normalizeResponsesImageURL(value any, detail any) any {
	switch image := value.(type) {
	case string:
		return map[string]any{"url": image, "detail": common.Interface2String(detail)}
	case map[string]any:
		return image
	default:
		return value
	}
}

func responsesValueString(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	encoded, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
