package common_handler

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

type closeAwareRerankErrorBody struct {
	closed atomic.Bool
}

func (b *closeAwareRerankErrorBody) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (b *closeAwareRerankErrorBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestRerankHandlerClosesBodyWhenReadFails(t *testing.T) {
	body := &closeAwareRerankErrorBody{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
	}

	_, apiErr := RerankHandler(nil, nil, resp)
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeReadResponseBodyFailed, apiErr.GetErrorCode())
	require.True(t, body.closed.Load())
}
