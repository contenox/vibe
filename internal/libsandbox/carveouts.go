package libsandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type carveoutDoc struct {
	Filesystem []FSCarveout  `json:"filesystem"`
	Network    []NetCarveout `json:"network"`
}

// LoadCarveouts decodes and validates a JSON carve-out document (filesystem/network holes) into fs and net, rejecting unknown fields and wrapping errors in ErrInvalidCarveout; paths are not resolved here.
func LoadCarveouts(r io.Reader) (fs []FSCarveout, net []NetCarveout, err error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var doc carveoutDoc
	if derr := dec.Decode(&doc); derr != nil {
		if errors.Is(derr, io.EOF) {
			return nil, nil, nil // empty document: deny-default, not an error
		}
		return nil, nil, fmt.Errorf("%w: decode: %w", ErrInvalidCarveout, derr)
	}

	for i, c := range doc.Filesystem {
		if verr := validateFSCarveout(c); verr != nil {
			return nil, nil, fmt.Errorf("%w: filesystem[%d]: %w", ErrInvalidCarveout, i, verr)
		}
	}
	for i, c := range doc.Network {
		if verr := validateNetCarveout(c); verr != nil {
			return nil, nil, fmt.Errorf("%w: network[%d]: %w", ErrInvalidCarveout, i, verr)
		}
	}
	return doc.Filesystem, doc.Network, nil
}
