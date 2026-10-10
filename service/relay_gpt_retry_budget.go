package service

import (
	"net/http"
	"os"
	"strings"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// AlignGPTTextRequestBudget keeps default GPT recovery inside the existing
// request deadline. A slow first attempt must not consume an unrelated 180s
// retry window while the request still has a larger pre-output budget.
// Explicit operator limits and all replay/attempt/round guards remain separate.
func (s *RelayRetryState) AlignGPTTextRequestBudget(c *gin.Context, info *relaycommon.RelayInfo, format types.RelayFormat) {
	if s == nil || info == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		return
	}
	if strings.TrimSpace(os.Getenv("RELAY_RETRY_MAX_ELAPSED_SECONDS")) != "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(info.OriginModelName)), "gpt-") || c.Request.Method != http.MethodPost {
		return
	}
	path := c.Request.URL.Path
	if !((format == types.RelayFormatOpenAI && path == "/v1/chat/completions") ||
		(format == types.RelayFormatOpenAIResponses && path == "/v1/responses")) {
		return
	}

	// Read the already latched absolute deadline; never restart the timer from
	// the latest channel attempt or from the beginning of a retry round.
	now := time.Now()
	remaining, limited := info.RemainingNonStreamBudget()
	if info.IsStream {
		remaining, limited = info.RemainingFirstValidEventBudget()
	}
	var deadline time.Time
	if limited {
		deadline = now.Add(remaining)
	}
	if callerDeadline, ok := c.Request.Context().Deadline(); ok && (deadline.IsZero() || callerDeadline.Before(deadline)) {
		deadline = callerDeadline
	}
	if deadline.IsZero() {
		// An unbounded request keeps the existing finite retry default.
		return
	}
	s.Policy.MaxElapsed = deadline.Sub(s.StartedAt)
}
