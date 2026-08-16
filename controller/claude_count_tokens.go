package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// ClaudeCountTokens implements Anthropic's local token-counting endpoint. It
// deliberately skips channel distribution and quota consumption.
func ClaudeCountTokens(c *gin.Context) {
	var req dto.ClaudeRequest
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"type": "error",
			"error": types.ClaudeError{
				Type:    "invalid_request_error",
				Message: "invalid JSON body: " + err.Error(),
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"input_tokens": service.EstimateClaudeInputTokens(&req),
	})
}
