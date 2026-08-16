package openaicompat

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

const (
	responsesEventCreated               = "response.created"
	responsesEventCompleted             = "response.completed"
	responsesEventIncomplete            = "response.incomplete"
	responsesEventOutputTextDelta       = "response.output_text.delta"
	responsesEventOutputItemAdded       = "response.output_item.added"
	responsesEventOutputItemDone        = "response.output_item.done"
	responsesEventFunctionArgsDelta     = "response.function_call_arguments.delta"
	responsesEventFunctionArgsDone      = "response.function_call_arguments.done"
	responsesEventReasoningSummaryDelta = "response.reasoning_summary_text.delta"
	responsesEventReasoningSummaryDone  = "response.reasoning_summary_text.done"
)

func ChatCompletionsResponseToResponsesResponse(resp *dto.OpenAITextResponse, id string) (*dto.OpenAIResponsesResponse, *dto.Usage, error) {
	if resp == nil {
		return nil, nil, fmt.Errorf("response is nil")
	}
	if strings.TrimSpace(id) == "" {
		id = resp.Id
	}
	if strings.TrimSpace(id) == "" {
		id = "resp_" + common.GetUUID()
	}
	usage := responsesUsageFromChat(&resp.Usage)
	out := &dto.OpenAIResponsesResponse{
		ID:        id,
		Object:    "response",
		CreatedAt: chatResponseCreatedAt(resp.Created),
		Status:    []byte(`"completed"`),
		Model:     resp.Model,
		Output:    make([]dto.ResponsesOutput, 0),
		Usage:     usage,
	}
	if len(resp.Choices) == 0 {
		return out, usage, nil
	}

	choice := resp.Choices[0]
	status := responsesStatusFromFinishReason(choice.FinishReason)
	out.Status = []byte(fmt.Sprintf("%q", status))
	text := choice.Message.StringContent()
	if text != "" {
		out.Output = append(out.Output, dto.ResponsesOutput{
			Type:   "message",
			ID:     id + "_msg_0",
			Status: responsesOutputStatus(status),
			Role:   "assistant",
			Content: []dto.ResponsesOutputContent{{
				Type:        "output_text",
				Text:        text,
				Annotations: []interface{}{},
			}},
		})
	}
	if reasoning := choice.Message.GetReasoningContent(); reasoning != "" {
		out.Output = append(out.Output, dto.ResponsesOutput{
			Type:   "reasoning",
			ID:     id + "_reasoning_0",
			Status: responsesOutputStatus(status),
			Content: []dto.ResponsesOutputContent{{
				Type: "summary_text",
				Text: reasoning,
			}},
		})
	}
	for index, toolCall := range choice.Message.ParseToolCalls() {
		callID := strings.TrimSpace(toolCall.ID)
		if callID == "" {
			callID = fmt.Sprintf("%s_call_%d", id, index)
		}
		arguments, err := common.Marshal(toolCall.Function.Arguments)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal function arguments: %w", err)
		}
		out.Output = append(out.Output, dto.ResponsesOutput{
			Type:      "function_call",
			ID:        callID,
			Status:    responsesOutputStatus(status),
			CallId:    callID,
			Name:      toolCall.Function.Name,
			Arguments: arguments,
		})
	}
	return out, usage, nil
}

func responsesUsageFromChat(src *dto.Usage) *dto.Usage {
	usage := &dto.Usage{}
	if src == nil {
		return usage
	}
	*usage = *src
	if usage.InputTokens == 0 {
		usage.InputTokens = usage.PromptTokens
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = usage.CompletionTokens
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.InputTokensDetails == nil {
		details := usage.PromptTokensDetails
		usage.InputTokensDetails = &details
	}
	return usage
}

func chatResponseCreatedAt(value any) int {
	switch created := value.(type) {
	case int:
		return created
	case int64:
		return int(created)
	case float64:
		return int(created)
	case float32:
		return int(created)
	default:
		return int(time.Now().Unix())
	}
}

func responsesStatusFromFinishReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "length", "content_filter":
		return "incomplete"
	default:
		return "completed"
	}
}

func responsesOutputStatus(status string) string {
	if status == "incomplete" {
		return "incomplete"
	}
	return "completed"
}

type ChatToResponsesStreamEvent struct {
	Type    string
	Payload dto.ResponsesStreamResponse
}

type ChatToResponsesStreamState struct {
	ID      string
	Model   string
	Created int64
	Usage   *dto.Usage

	status           string
	sentCreated      bool
	textOutputIndex  int
	textStarted      bool
	textDone         bool
	reasoningIndex   int
	reasoningStarted bool
	reasoningDone    bool
	finalized        bool
	nextOutputIndex  int
	toolsByIndex     map[int]*chatToResponsesStreamTool
	outputOrder      []chatToResponsesOutputRef
	text             strings.Builder
	reasoning        strings.Builder
}

type chatToResponsesStreamTool struct {
	ChatIndex   int
	OutputIndex int
	ID          string
	Name        string
	Arguments   strings.Builder
	Done        bool
}

type chatToResponsesOutputRef struct {
	Kind      string
	ToolIndex int
}

func NewChatToResponsesStreamState(id string, model string) *ChatToResponsesStreamState {
	return &ChatToResponsesStreamState{
		ID:              id,
		Model:           model,
		Created:         time.Now().Unix(),
		Usage:           &dto.Usage{},
		status:          "completed",
		textOutputIndex: -1,
		reasoningIndex:  -1,
		toolsByIndex:    make(map[int]*chatToResponsesStreamTool),
	}
}

func (s *ChatToResponsesStreamState) SetUsage(usage *dto.Usage) {
	if s == nil || usage == nil {
		return
	}
	s.Usage = responsesUsageFromChat(usage)
}

func ChatCompletionsStreamChunkToResponsesEvents(chunk *dto.ChatCompletionsStreamResponse, state *ChatToResponsesStreamState) ([]ChatToResponsesStreamEvent, error) {
	if chunk == nil || state == nil {
		return nil, nil
	}
	if state.ID == "" {
		state.ID = chunk.Id
	}
	if state.Model == "" {
		state.Model = chunk.Model
	}
	if state.Created == 0 {
		state.Created = chunk.Created
	}
	if chunk.Usage != nil {
		state.SetUsage(chunk.Usage)
	}

	events := make([]ChatToResponsesStreamEvent, 0)
	if !state.sentCreated {
		state.sentCreated = true
		events = append(events, state.event(responsesEventCreated, dto.ResponsesStreamResponse{Response: state.createdResponse()}))
	}
	for _, choice := range chunk.Choices {
		if reasoning := choice.Delta.GetReasoningContent(); reasoning != "" {
			events = append(events, state.appendReasoningDelta(reasoning)...)
		}
		if text := choice.Delta.GetContentString(); text != "" {
			events = append(events, state.appendTextDelta(text)...)
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			events = append(events, state.appendToolCallDelta(toolCall)...)
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			state.status = responsesStatusFromFinishReason(*choice.FinishReason)
			events = append(events, state.doneDeltaEvents()...)
		}
	}
	return events, nil
}

func FinalizeChatCompletionsStreamToResponses(state *ChatToResponsesStreamState) []ChatToResponsesStreamEvent {
	if state == nil || state.finalized {
		return nil
	}
	events := state.doneDeltaEvents()
	state.finalized = true
	eventType := responsesEventCompleted
	if state.status == "incomplete" {
		eventType = responsesEventIncomplete
	}
	events = append(events, state.event(eventType, dto.ResponsesStreamResponse{Response: state.finalResponse()}))
	return events
}

func (s *ChatToResponsesStreamState) appendTextDelta(delta string) []ChatToResponsesStreamEvent {
	events := make([]ChatToResponsesStreamEvent, 0, 2)
	if !s.textStarted {
		s.textStarted = true
		s.textOutputIndex = s.nextIndex("message", -1)
		events = append(events, s.event(responsesEventOutputItemAdded, dto.ResponsesStreamResponse{
			OutputIndex: intPointer(s.textOutputIndex),
			Item:        &dto.ResponsesOutput{Type: "message", ID: s.messageID(), Status: "in_progress", Role: "assistant", Content: []dto.ResponsesOutputContent{}},
		}))
	}
	s.text.WriteString(delta)
	events = append(events, s.event(responsesEventOutputTextDelta, dto.ResponsesStreamResponse{
		OutputIndex: intPointer(s.textOutputIndex), ContentIndex: intPointer(0), Delta: delta, ItemID: s.messageID(),
	}))
	return events
}

func (s *ChatToResponsesStreamState) appendReasoningDelta(delta string) []ChatToResponsesStreamEvent {
	events := make([]ChatToResponsesStreamEvent, 0, 2)
	if !s.reasoningStarted {
		s.reasoningStarted = true
		s.reasoningIndex = s.nextIndex("reasoning", -1)
		events = append(events, s.event(responsesEventOutputItemAdded, dto.ResponsesStreamResponse{
			OutputIndex: intPointer(s.reasoningIndex),
			Item:        &dto.ResponsesOutput{Type: "reasoning", ID: s.reasoningID(), Status: "in_progress", Content: []dto.ResponsesOutputContent{}},
		}))
	}
	s.reasoning.WriteString(delta)
	events = append(events, s.event(responsesEventReasoningSummaryDelta, dto.ResponsesStreamResponse{
		OutputIndex: intPointer(s.reasoningIndex), SummaryIndex: intPointer(0), Delta: delta, ItemID: s.reasoningID(),
	}))
	return events
}

func (s *ChatToResponsesStreamState) appendToolCallDelta(toolCall dto.ToolCallResponse) []ChatToResponsesStreamEvent {
	chatIndex := 0
	if toolCall.Index != nil {
		chatIndex = *toolCall.Index
	}
	tool := s.toolsByIndex[chatIndex]
	events := make([]ChatToResponsesStreamEvent, 0, 2)
	if tool == nil {
		tool = &chatToResponsesStreamTool{ChatIndex: chatIndex, OutputIndex: s.nextIndex("tool", chatIndex), ID: strings.TrimSpace(toolCall.ID), Name: strings.TrimSpace(toolCall.Function.Name)}
		if tool.ID == "" {
			tool.ID = fmt.Sprintf("%s_call_%d", s.ID, chatIndex)
		}
		s.toolsByIndex[chatIndex] = tool
		emptyArguments, _ := common.Marshal("")
		events = append(events, s.event(responsesEventOutputItemAdded, dto.ResponsesStreamResponse{
			OutputIndex: intPointer(tool.OutputIndex), ItemID: tool.ID,
			Item: &dto.ResponsesOutput{Type: "function_call", ID: tool.ID, Status: "in_progress", CallId: tool.ID, Name: tool.Name, Arguments: emptyArguments},
		}))
	}
	if toolCall.ID != "" {
		tool.ID = toolCall.ID
	}
	if toolCall.Function.Name != "" {
		tool.Name = toolCall.Function.Name
	}
	if toolCall.Function.Arguments != "" {
		tool.Arguments.WriteString(toolCall.Function.Arguments)
		events = append(events, s.event(responsesEventFunctionArgsDelta, dto.ResponsesStreamResponse{
			OutputIndex: intPointer(tool.OutputIndex), ItemID: tool.ID, Delta: toolCall.Function.Arguments,
		}))
	}
	return events
}

func (s *ChatToResponsesStreamState) doneDeltaEvents() []ChatToResponsesStreamEvent {
	events := make([]ChatToResponsesStreamEvent, 0)
	status := responsesOutputStatus(s.status)
	if s.textStarted && !s.textDone {
		s.textDone = true
		events = append(events,
			s.event("response.output_text.done", dto.ResponsesStreamResponse{OutputIndex: intPointer(s.textOutputIndex), ContentIndex: intPointer(0), ItemID: s.messageID()}),
			s.event(responsesEventOutputItemDone, dto.ResponsesStreamResponse{OutputIndex: intPointer(s.textOutputIndex), Item: s.messageOutput(status)}),
		)
	}
	if s.reasoningStarted && !s.reasoningDone {
		s.reasoningDone = true
		events = append(events,
			s.event(responsesEventReasoningSummaryDone, dto.ResponsesStreamResponse{
				OutputIndex: intPointer(s.reasoningIndex), SummaryIndex: intPointer(0), ItemID: s.reasoningID(),
				Part: &dto.ResponsesReasoningSummaryPart{Type: "summary_text", Text: s.reasoning.String()},
			}),
			s.event(responsesEventOutputItemDone, dto.ResponsesStreamResponse{OutputIndex: intPointer(s.reasoningIndex), Item: s.reasoningOutput(status)}),
		)
	}
	for _, tool := range s.sortedTools() {
		if tool.Done {
			continue
		}
		tool.Done = true
		events = append(events,
			s.event(responsesEventFunctionArgsDone, dto.ResponsesStreamResponse{OutputIndex: intPointer(tool.OutputIndex), ItemID: tool.ID}),
			s.event(responsesEventOutputItemDone, dto.ResponsesStreamResponse{OutputIndex: intPointer(tool.OutputIndex), Item: s.toolOutput(tool, status)}),
		)
	}
	return events
}

func (s *ChatToResponsesStreamState) event(eventType string, payload dto.ResponsesStreamResponse) ChatToResponsesStreamEvent {
	payload.Type = eventType
	return ChatToResponsesStreamEvent{Type: eventType, Payload: payload}
}

func (s *ChatToResponsesStreamState) createdResponse() *dto.OpenAIResponsesResponse {
	return &dto.OpenAIResponsesResponse{ID: s.ID, Object: "response", CreatedAt: int(s.Created), Status: []byte(`"in_progress"`), Model: s.Model, Output: []dto.ResponsesOutput{}}
}

func (s *ChatToResponsesStreamState) finalResponse() *dto.OpenAIResponsesResponse {
	output := make([]dto.ResponsesOutput, 0, len(s.outputOrder))
	status := responsesOutputStatus(s.status)
	for _, ref := range s.outputOrder {
		switch ref.Kind {
		case "message":
			output = append(output, *s.messageOutput(status))
		case "reasoning":
			output = append(output, *s.reasoningOutput(status))
		case "tool":
			if tool := s.toolsByIndex[ref.ToolIndex]; tool != nil {
				output = append(output, *s.toolOutput(tool, status))
			}
		}
	}
	return &dto.OpenAIResponsesResponse{ID: s.ID, Object: "response", CreatedAt: int(s.Created), Status: []byte(fmt.Sprintf("%q", s.status)), Model: s.Model, Output: output, Usage: s.Usage}
}

func (s *ChatToResponsesStreamState) nextIndex(kind string, toolIndex int) int {
	index := s.nextOutputIndex
	s.nextOutputIndex++
	s.outputOrder = append(s.outputOrder, chatToResponsesOutputRef{Kind: kind, ToolIndex: toolIndex})
	return index
}

func (s *ChatToResponsesStreamState) sortedTools() []*chatToResponsesStreamTool {
	indexes := make([]int, 0, len(s.toolsByIndex))
	for index := range s.toolsByIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	tools := make([]*chatToResponsesStreamTool, 0, len(indexes))
	for _, index := range indexes {
		tools = append(tools, s.toolsByIndex[index])
	}
	return tools
}

func (s *ChatToResponsesStreamState) messageID() string   { return s.ID + "_msg_0" }
func (s *ChatToResponsesStreamState) reasoningID() string { return s.ID + "_reasoning_0" }

func (s *ChatToResponsesStreamState) messageOutput(status string) *dto.ResponsesOutput {
	return &dto.ResponsesOutput{Type: "message", ID: s.messageID(), Status: status, Role: "assistant", Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: s.text.String(), Annotations: []interface{}{}}}}
}

func (s *ChatToResponsesStreamState) reasoningOutput(status string) *dto.ResponsesOutput {
	return &dto.ResponsesOutput{Type: "reasoning", ID: s.reasoningID(), Status: status, Content: []dto.ResponsesOutputContent{{Type: "summary_text", Text: s.reasoning.String()}}}
}

func (s *ChatToResponsesStreamState) toolOutput(tool *chatToResponsesStreamTool, status string) *dto.ResponsesOutput {
	arguments, _ := common.Marshal(tool.Arguments.String())
	return &dto.ResponsesOutput{Type: "function_call", ID: tool.ID, Status: status, CallId: tool.ID, Name: tool.Name, Arguments: arguments}
}

func intPointer(value int) *int { return &value }
