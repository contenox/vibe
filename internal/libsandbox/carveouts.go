package libsandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// carveoutDoc is the on-disk necessity-list shape (see LoadCarveouts),
// sharing FSCarveout/NetCarveout with the in-memory Spec.
type carveoutDoc struct {
	Filesystem []FSCarveout  `json:"filesystem"`
	Network    []NetCarveout `json:"network"`
}

// LoadCarveouts decodes a necessity list naming the wall's holes and why
// each is needed:
//
//	{
//	  "filesystem": [ {"path":"~/.claude","mode":"ro","needs":"agent auth/config to start"} ],
//	  "network":    [ {"host":"registry.npmjs.org","needs":"npm install fetch"} ]
//	}
//
// Deny-default: an empty or absent document yields empty lists. Unknown
// fields are rejected (DisallowUnknownFields) so a typo like "mods" fails
// loudly instead of silently mis-widening a hole. Every entry is validated
// (mode, justification, traversal, host).
//
// All errors wrap ErrInvalidCarveout. This function does not resolve "~" or
// otherwise interpret paths — that belongs to the enforcement layer.
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
