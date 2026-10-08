package helper

import (
	"math"
	"strconv"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const responsesDeliveryKey = "responses_terminal_delivery"

// Adapted from CLIProxyAPI commits 3522e481 and 25913086: a failed Responses
// stream needs a lifecycle terminal, with the error inside response.error.
// Keep only delivery metadata; output text, tool arguments and usage are not
// reconstructed when the upstream never supplied a terminal response.
type responsesDeliveryState struct {
	mu           sync.Mutex
	responseID   string
	nextSequence int64
	sequenceFull bool
	terminal     bool
}

func responsesDelivery(c *gin.Context) *responsesDeliveryState {
	if v, ok := c.Get(responsesDeliveryKey); ok {
		return v.(*responsesDeliveryState)
	}
	state := &responsesDeliveryState{}
	c.Set(responsesDeliveryKey, state)
	return state
}

func observeResponsesDelivery(c *gin.Context, eventType, data string) {
	state := responsesDelivery(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	root := gjson.Parse(data)
	if id := root.Get("response.id"); id.Type == gjson.String && len(id.String()) <= 4096 && id.String() != "" {
		state.responseID = id.String()
	}
	if sequence := root.Get("sequence_number"); sequence.Type == gjson.Number {
		if n, err := strconv.ParseInt(sequence.Raw, 10, 64); err == nil && n >= state.nextSequence {
			state.nextSequence = n
		}
	}
	if state.nextSequence == math.MaxInt64 {
		state.sequenceFull = true
	} else {
		state.nextSequence++
	}
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
		state.terminal = true
	}
}

func sendResponsesTerminalFailure(c *gin.Context, relayErr *types.NewAPIError) error {
	state := responsesDelivery(c)
	state.mu.Lock()
	terminal, id, sequence, sequenceFull := state.terminal, state.responseID, state.nextSequence, state.sequenceFull
	state.mu.Unlock()
	if terminal {
		return nil
	}
	response := gin.H{"status": "failed", "error": relayErr.ToOpenAIError()}
	if id != "" {
		response["id"] = id
	}
	event := gin.H{"type": "response.failed", "response": response}
	if !sequenceFull {
		event["sequence_number"] = sequence
	}
	body, err := common.Marshal(event)
	if err != nil {
		return err
	}
	return ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.failed"}, string(body))
}
