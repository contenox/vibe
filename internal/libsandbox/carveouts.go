package libsandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// carveoutDoc is the on-disk necessity-list shape (see LoadCarveouts). The
// entry types are FSCarveout/NetCarveout themselves, so the wire format and the
// in-memory Spec share one definition.
type carveoutDoc struct {
	Filesystem []FSCarveout  `json:"filesystem"`
	Network    []NetCarveout `json:"network"`
}

// LoadCarveouts decodes a necessity list — the JSON that names the wall's holes
// and, for each, why the agent breaks without it:
//
//	{
//	  "filesystem": [ {"path":"~/.claude","mode":"ro","needs":"agent auth/config to start"} ],
//	  "network":    [ {"host":"registry.npmjs.org","needs":"npm install fetch"} ]
//	}
//
// It is deny-default: an empty or absent document (empty reader, "{}", "null")
// yields empty lists — nothing is allowed unless it is written down. Unknown
// fields are rejected (DisallowUnknownFields), so a typo like "mods" instead of
// "mode" fails loudly rather than silently widening or narrowing a hole. Every
// entry is validated — mode ∈ {ModeRO, ModeRW}, a non-empty justification, no
// ".." traversal in a path, a non-empty host — because a hole that cannot say
// why it exists must not be admitted.
//
// All errors wrap ErrInvalidCarveout. The returned lists are ready to place in a
// Spec; this function does not resolve "~" or otherwise interpret paths — that
// belongs to the enforcement layer, not the parser.
func LoadCarveouts(r io.Reader) (fs []FSCarveout, net []NetCarveout, err error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var doc carveoutDoc
	if derr := dec.Decode(&doc); derr != nil {
		if errors.Is(derr, io.EOF) {
			// Deny-default: an empty document allows nothing. This is a benign
			// "nothing to load", not a malformed list, so it is not an error.
			return nil, nil, nil
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
