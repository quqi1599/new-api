package dto

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// AlphaSearchRequest is the Codex standalone web-search request. RawBody keeps
// protocol-private and future fields intact for transparent forwarding.
type AlphaSearchRequest struct {
	Model   string          `json:"model"`
	Id      string          `json:"id,omitempty"`
	Stream  *bool           `json:"stream,omitempty"`
	RawBody json.RawMessage `json:"-"`
}

func (r *AlphaSearchRequest) GetTokenCountMeta() *types.TokenCountMeta {
	return &types.TokenCountMeta{
		CombineText: string(r.RawBody),
		TokenType:   types.TokenTypeTokenizer,
	}
}

func (r *AlphaSearchRequest) IsStream(_ *gin.Context) bool { return false }

func (r *AlphaSearchRequest) SetModelName(modelName string) {
	if modelName != "" {
		r.Model = modelName
	}
}
