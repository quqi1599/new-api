package common

import (
	"io"

	basecommon "github.com/QuantumNous/new-api/common"
)

// NewOutboundJSONBody stores an already-marshaled upstream request in the
// configured memory/disk BodyStorage. The caller must close the returned closer.
func NewOutboundJSONBody(data []byte) (body io.Reader, size int64, closer io.Closer, err error) {
	storage, err := basecommon.CreateBodyStorage(data)
	if err != nil {
		return nil, 0, nil, err
	}
	// Large-body admission protects read/parse/transform work. Do not hold the
	// scarce slot while waiting for a long-lived upstream response.
	storage.ReleaseAdmission()
	return basecommon.ReaderOnly(storage), storage.Size(), storage, nil
}
