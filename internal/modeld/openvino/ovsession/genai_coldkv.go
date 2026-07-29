package ovsession

import "errors"

// ErrColdKVUnsupported means the linked GenAI bridge does not expose external
// KV import/export. OpenVINO still has internal KV state; this is the modeld
// bridge capability gate.
var ErrColdKVUnsupported = errors.New("openvino GenAI cold KV import/export is not available")

// ColdKVRange identifies a logical token block moving between OpenVINO's hot KV
// and modeld's host-RAM cold store.
type ColdKVRange struct {
	Start        int
	End          int
	DestStart    int
	Tokens       []int
	PrefixTokens []int
	TokenHash    string
}
