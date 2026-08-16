package librelay_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/librelay"
)

// TestUnit_ChainTriggerFramesRoundTrip asserts what one end builds with WithPayload, the other decodes field for field, with the raw input untouched in transit.
func TestUnit_ChainTriggerFramesRoundTrip(t *testing.T) {
	t.Parallel()

	trigger, err := librelay.Frame{
		Type:     librelay.TypeChainTrigger,
		Instance: "inst-1",
	}.WithPayload(librelay.ChainTrigger{
		RequestID:   "req-1",
		Chain:       "chain-on-report.json",
		SessionMode: librelay.ChainSessionNew,
		Input:       json.RawMessage(`{"nid":7,"type":"missionservice.events.report_added","hop":2}`),
	})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	var buf bytes.Buffer
	if err := librelay.NewWriter(&buf).WriteFrame(trigger); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := librelay.NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != librelay.TypeChainTrigger || got.Instance != "inst-1" || got.Session != "" {
		t.Fatalf("envelope = %+v", got)
	}
	var decoded librelay.ChainTrigger
	if err := got.DecodePayload(&decoded); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if decoded.RequestID != "req-1" || decoded.Chain != "chain-on-report.json" ||
		decoded.SessionMode != librelay.ChainSessionNew || decoded.Policy != "" ||
		decoded.SessionName != "" {
		t.Fatalf("payload = %+v", decoded)
	}
	if string(decoded.Input) != `{"nid":7,"type":"missionservice.events.report_added","hop":2}` {
		t.Fatalf("input = %s", decoded.Input)
	}

	named, err := librelay.Frame{
		Type:     librelay.TypeChainTrigger,
		Instance: "inst-1",
	}.WithPayload(librelay.ChainTrigger{
		RequestID:   "req-2",
		Chain:       "chain-on-report.json",
		SessionMode: librelay.ChainSessionReused,
		SessionName: "refund-desk",
		Input:       json.RawMessage(`{"hop":0}`),
	})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	buf.Reset()
	if err := librelay.NewWriter(&buf).WriteFrame(named); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err = librelay.NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	var namedDecoded librelay.ChainTrigger
	if err := got.DecodePayload(&namedDecoded); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if namedDecoded.RequestID != "req-2" ||
		namedDecoded.SessionMode != librelay.ChainSessionReused ||
		namedDecoded.SessionName != "refund-desk" {
		t.Fatalf("payload = %+v", namedDecoded)
	}

	result, err := librelay.Frame{
		Type:     librelay.TypeChainTriggerResult,
		Instance: "inst-1",
	}.WithPayload(librelay.ChainTriggerResult{
		RequestID: "req-1",
		Status:    librelay.ChainTriggerStatusRefused,
		Error:     "unknown chain",
	})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	buf.Reset()
	if err := librelay.NewWriter(&buf).WriteFrame(result); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err = librelay.NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	var res librelay.ChainTriggerResult
	if err := got.DecodePayload(&res); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if res.RequestID != "req-1" || res.Status != librelay.ChainTriggerStatusRefused || res.Error != "unknown chain" {
		t.Fatalf("payload = %+v", res)
	}
}

// TestUnit_ChainTriggerWireShape pins the payload JSON verbatim: the relay is a separate module built against this same contract.
func TestUnit_ChainTriggerWireShape(t *testing.T) {
	t.Parallel()

	trigger, err := librelay.Frame{Type: librelay.TypeChainTrigger, Instance: "inst-1"}.
		WithPayload(librelay.ChainTrigger{
			RequestID:   "req-1",
			Chain:       "chain-on-report.json",
			SessionMode: "new",
			Input:       json.RawMessage(`{"hop":0}`),
		})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	wantTrigger := `{"request_id":"req-1","chain":"chain-on-report.json","session_mode":"new","input":{"hop":0}}`
	if string(trigger.Payload) != wantTrigger {
		t.Fatalf("trigger payload = %s, want %s", trigger.Payload, wantTrigger)
	}

	named, err := librelay.Frame{Type: librelay.TypeChainTrigger, Instance: "inst-1"}.
		WithPayload(librelay.ChainTrigger{
			RequestID:   "req-3",
			Chain:       "chain-on-report.json",
			SessionMode: "reused",
			SessionName: "refund-desk",
			Input:       json.RawMessage(`{"hop":0}`),
		})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	wantNamed := `{"request_id":"req-3","chain":"chain-on-report.json","session_mode":"reused","session_name":"refund-desk","input":{"hop":0}}`
	if string(named.Payload) != wantNamed {
		t.Fatalf("named trigger payload = %s, want %s", named.Payload, wantNamed)
	}

	result, err := librelay.Frame{Type: librelay.TypeChainTriggerResult, Instance: "inst-1"}.
		WithPayload(librelay.ChainTriggerResult{RequestID: "req-1", Status: "ok"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	wantResult := `{"request_id":"req-1","status":"ok"}`
	if string(result.Payload) != wantResult {
		t.Fatalf("result payload = %s, want %s", result.Payload, wantResult)
	}

	// The relay-authored shape decodes, including the optional policy field.
	var decoded librelay.ChainTrigger
	relaySide := `{"request_id":"req-2","chain":"chain-x.json","session_mode":"reused","session_name":"refund-desk","input":[1,2],"policy":"hitl-policy-strict.json"}`
	if err := json.Unmarshal([]byte(relaySide), &decoded); err != nil {
		t.Fatalf("decode relay-side payload: %v", err)
	}
	if decoded.SessionMode != librelay.ChainSessionReused || decoded.Policy != "hitl-policy-strict.json" ||
		decoded.SessionName != "refund-desk" {
		t.Fatalf("decoded = %+v", decoded)
	}

	// Routed as cargo, never handled by a relay as control traffic.
	if librelay.IsControl(librelay.TypeChainTrigger) || librelay.IsControl(librelay.TypeChainTriggerResult) {
		t.Fatal("chain-trigger types must not carry the control prefix")
	}
}
