package service

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

// EstimateClaudeInputTokens returns a fast local estimate for
// /v1/messages/count_tokens without selecting an upstream channel.
func EstimateClaudeInputTokens(req *dto.ClaudeRequest) int {
	if req == nil {
		return 0
	}
	normalizeClaudeCountTokenTools(req)
	meta := req.GetTokenCountMeta()
	if meta == nil {
		return 0
	}
	return EstimateTokenByModel(req.Model, meta.CombineText)
}

func normalizeClaudeCountTokenTools(req *dto.ClaudeRequest) {
	if req == nil || req.Tools == nil {
		return
	}
	rawTools, ok := req.Tools.([]any)
	if !ok {
		return
	}
	normalized := make([]any, 0, len(rawTools))
	for _, rawTool := range rawTools {
		switch rawTool.(type) {
		case *dto.Tool, dto.Tool, *dto.ClaudeWebSearchTool, dto.ClaudeWebSearchTool:
			normalized = append(normalized, rawTool)
			continue
		}
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		encoded, err := common.Marshal(toolMap)
		if err != nil {
			continue
		}
		if toolType, ok := toolMap["type"].(string); ok && strings.HasPrefix(toolType, "web_search") {
			var webSearch dto.ClaudeWebSearchTool
			if err := common.Unmarshal(encoded, &webSearch); err == nil && webSearch.Type != "" {
				normalized = append(normalized, &webSearch)
			}
			continue
		}
		var tool dto.Tool
		if err := common.Unmarshal(encoded, &tool); err == nil && tool.Name != "" {
			normalized = append(normalized, &tool)
		}
	}
	req.Tools = normalized
}
