package dto

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
)

// ServerToolUsage is upstream-reported cumulative usage, not a request tool
// declaration. Nil is unreported; a non-nil zero is authoritative.
type ServerToolUsage struct {
	WebSearchRequests *int `json:"web_search_requests,omitempty"`
}

var ErrInvalidServerToolUsage = errors.New("invalid upstream server-tool usage")

func (u *ServerToolUsage) UnmarshalJSON(data []byte) error {
	type wireUsage ServerToolUsage
	var value wireUsage
	if err := common.Unmarshal(data, &value); err != nil {
		return ErrInvalidServerToolUsage
	}
	if value.WebSearchRequests != nil && *value.WebSearchRequests < 0 {
		return ErrInvalidServerToolUsage
	}
	*u = ServerToolUsage(value)
	return nil
}

// MergeServerToolUsage applies a partial cumulative snapshot without sharing
// count pointers or erasing earlier counts when later snapshots omit them.
func MergeServerToolUsage(current, incoming *ServerToolUsage) *ServerToolUsage {
	var count *int
	if current != nil && current.WebSearchRequests != nil {
		count = common.GetPointer(*current.WebSearchRequests)
	}
	if incoming != nil && incoming.WebSearchRequests != nil {
		count = common.GetPointer(*incoming.WebSearchRequests)
	}
	if count == nil {
		return nil
	}
	return &ServerToolUsage{WebSearchRequests: count}
}
