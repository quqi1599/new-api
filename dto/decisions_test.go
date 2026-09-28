package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestDecisionsStructuredCriteriaAndZeroValues(t *testing.T) {
	input := `{"model":"jev-latest","stream":false,"state":{"enabled":false,"count":0},"questions":{"flag":{"type":"noul","instructions":{"ask":"Is enabled true?"},"criteria":{"true":{"enabled":true},"false":[false,0]}},"pick":{"type":"choice","instructions":"Choose","criteria":{"first":{"rank":0},"second":null}},"rate":{"type":"score","instructions":["Rate"],"criteria":[{"level":0},["high"]]}}}`
	var request DecisionsRequest
	require.NoError(t, common.Unmarshal([]byte(input), &request))
	require.NoError(t, request.Validate())
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	require.JSONEq(t, input, string(encoded))
	require.False(t, request.IsStream(nil))
}

func TestDecisionsCriteriaBounds(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		count int
		valid bool
	}{{"choice", 0, false}, {"choice", 255, true}, {"choice", 256, false}, {"score", 1, false}, {"score", 2, true}, {"score", 10, true}, {"score", 11, false}} {
		var criteria any
		if tc.kind == "choice" {
			options := map[string]any{}
			for i := 0; i < tc.count; i++ {
				options[strings.Repeat("x", i+1)] = nil
			}
			criteria = options
		} else {
			levels := make([]string, tc.count)
			criteria = levels
		}
		raw, err := common.Marshal(criteria)
		require.NoError(t, err)
		request := DecisionsRequest{Model: "jev-latest", State: json.RawMessage(`"state"`), Questions: map[string]DecisionsQuestion{"q": {Type: tc.kind, Instructions: json.RawMessage(`"question"`), Criteria: raw}}}
		if tc.valid {
			require.NoError(t, request.Validate())
		} else {
			require.Error(t, request.Validate())
		}
	}
}
