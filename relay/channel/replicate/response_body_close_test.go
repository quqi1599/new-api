package replicate

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

type closeAwareErrorBody struct {
	closed atomic.Bool
}

func (b *closeAwareErrorBody) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (b *closeAwareErrorBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestDoResponseClosesBodyWhenReadFails(t *testing.T) {
	body := &closeAwareErrorBody{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
	}

	_, apiErr := (&Adaptor{}).DoResponse(nil, resp, nil)
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeReadResponseBodyFailed, apiErr.GetErrorCode())
	require.True(t, body.closed.Load())
}
