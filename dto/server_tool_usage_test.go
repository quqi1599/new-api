package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestServerToolUsagePresenceAndValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"web_search_requests":null}`} {
		var usage ServerToolUsage
		require.NoError(t, common.Unmarshal([]byte(raw), &usage))
		require.Nil(t, usage.WebSearchRequests)
	}
	for _, count := range []int{0, 1, 7} {
		usage := ServerToolUsage{WebSearchRequests: common.GetPointer(count)}
		wire, err := common.Marshal(usage)
		require.NoError(t, err)
		var decoded ServerToolUsage
		require.NoError(t, common.Unmarshal(wire, &decoded))
		require.Equal(t, count, *decoded.WebSearchRequests)
	}
	for _, value := range []string{`-1`, `1.5`, `"2"`, `true`, `9223372036854775808`, `{}`, `[]`} {
		var usage ServerToolUsage
		require.Error(t, common.Unmarshal([]byte(`{"web_search_requests":`+value+`}`), &usage), value)
	}
}

func TestServerToolUsageMergeIsCumulativeAndDetached(t *testing.T) {
	first := &ServerToolUsage{WebSearchRequests: common.GetPointer(3)}
	merged := MergeServerToolUsage(nil, first)
	*first.WebSearchRequests = 99
	for _, next := range []*ServerToolUsage{nil, {}, {WebSearchRequests: common.GetPointer(3)}} {
		merged = MergeServerToolUsage(merged, next)
		require.Equal(t, 3, *merged.WebSearchRequests)
	}
	merged = MergeServerToolUsage(merged, &ServerToolUsage{WebSearchRequests: common.GetPointer(0)})
	encoded, err := common.Marshal(merged)
	require.NoError(t, err)
	require.JSONEq(t, `{"web_search_requests":0}`, string(encoded))
}
