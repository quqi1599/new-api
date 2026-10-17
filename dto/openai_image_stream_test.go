package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestImageStreamAndExplicitZeroFieldsRoundTrip(t *testing.T) {
	for _, stream := range []string{"true", "false", "null"} {
		body := `{"model":"gpt-image-2","prompt":"test","stream":` + stream + `,"partial_images":0,"output_compression":0,"watermark":false}`
		var request ImageRequest
		require.NoError(t, common.Unmarshal([]byte(body), &request))
		require.Equal(t, stream == "true", request.IsStream(nil))
		encoded, err := common.Marshal(request)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"partial_images":0`)
		require.Contains(t, string(encoded), `"output_compression":0`)
		require.Contains(t, string(encoded), `"watermark":false`)
		if stream != "null" {
			require.Contains(t, string(encoded), `"stream":`+stream)
		} else {
			require.NotContains(t, string(encoded), `"stream"`)
		}
	}
	var request ImageRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-image-2","prompt":"test"}`), &request))
	require.False(t, request.IsStream(nil))
	require.Nil(t, request.Stream)
	require.Error(t, common.Unmarshal([]byte(`{"model":"gpt-image-2","stream":"true"}`), &request))
}
