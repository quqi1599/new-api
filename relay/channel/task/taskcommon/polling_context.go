package taskcommon

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const fallbackPollingRequestTimeout = 10 * time.Minute

// PollingRequestTimeout keeps legacy polling callers bounded even when the
// relay-specific non-stream timeout is explicitly disabled. The normal polling
// loop supplies its own (usually earlier) request context.
func PollingRequestTimeout() time.Duration {
	if common.RelayNonStreamTimeout > 0 {
		timeout := time.Duration(common.RelayNonStreamTimeout) * time.Second
		if timeout < fallbackPollingRequestTimeout {
			return timeout
		}
	}
	return fallbackPollingRequestTimeout
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// DoLegacyPollingRequest preserves the old FetchTask API without letting it
// create an unbounded Background request. New polling code should call the
// context-aware FetchTaskContext method directly.
func DoLegacyPollingRequest(do func(context.Context) (*http.Response, error)) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), PollingRequestTimeout())
	resp, err := do(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		cancel()
		return resp, nil
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
