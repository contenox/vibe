package librelay_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/librelay"
)

func roundTripFrame(t *testing.T, f librelay.Frame, payload any) librelay.Frame {
	t.Helper()
	f, err := f.WithPayload(payload)
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var buf bytes.Buffer
	if err := librelay.NewWriter(&buf).WriteFrame(f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := librelay.NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return got
}

func TestUnit_AskFramesRoundTrip(t *testing.T) {
	t.Parallel()

	rule := 3
	expires := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	published := librelay.AskPublished{
		AskID:       "ask-1",
		SessionID:   "cnx-sess-1",
		MissionID:   "m-1",
		AgentName:   "refund-desk",
		ToolsName:   "billing",
		ToolName:    "issue_refund",
		PolicyName:  "hitl-policy-default.json",
		MatchedRule: &rule,
		ArgsSummary: "refund 40 EUR to customer 8812",
		ExpiresAt:   expires,
	}

	got := roundTripFrame(t, librelay.Frame{Type: librelay.TypeAskPublished, Instance: "inst-1"}, published)
	if got.Type != librelay.TypeAskPublished || got.Instance != "inst-1" || got.Session != "" {
		t.Fatalf("envelope = %+v", got)
	}
	var decodedPublished librelay.AskPublished
	if err := got.DecodePayload(&decodedPublished); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if decodedPublished.AskID != "ask-1" || decodedPublished.SessionID != "cnx-sess-1" ||
		decodedPublished.MissionID != "m-1" || decodedPublished.AgentName != "refund-desk" ||
		decodedPublished.ToolsName != "billing" || decodedPublished.ToolName != "issue_refund" ||
		decodedPublished.PolicyName != "hitl-policy-default.json" ||
		decodedPublished.ArgsSummary != "refund 40 EUR to customer 8812" {
		t.Fatalf("payload = %+v", decodedPublished)
	}
	if decodedPublished.MatchedRule == nil || *decodedPublished.MatchedRule != 3 {
		t.Fatalf("matched rule = %v", decodedPublished.MatchedRule)
	}
	if !decodedPublished.ExpiresAt.Equal(expires) {
		t.Fatalf("expires at = %s, want %s", decodedPublished.ExpiresAt, expires)
	}

	got = roundTripFrame(t, librelay.Frame{Type: librelay.TypeAskResolved, Instance: "inst-1"},
		librelay.AskResolved{AskID: "ask-1", Reason: librelay.AskResolvedSuperseded})
	var decodedResolved librelay.AskResolved
	if err := got.DecodePayload(&decodedResolved); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if decodedResolved.AskID != "ask-1" || decodedResolved.Reason != librelay.AskResolvedSuperseded {
		t.Fatalf("payload = %+v", decodedResolved)
	}

	decided := time.Date(2026, 8, 16, 11, 30, 0, 0, time.UTC)
	got = roundTripFrame(t, librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"},
		librelay.AskVerdict{
			AskID:     "ask-1",
			Decision:  librelay.AskDecisionDeny,
			Guidance:  "customer is outside the refund window",
			DecidedBy: "u_9",
			DecidedAt: decided,
		})
	var decodedVerdict librelay.AskVerdict
	if err := got.DecodePayload(&decodedVerdict); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if decodedVerdict.AskID != "ask-1" || decodedVerdict.Decision != librelay.AskDecisionDeny ||
		decodedVerdict.Answer != "" || decodedVerdict.Guidance != "customer is outside the refund window" ||
		decodedVerdict.DecidedBy != "u_9" {
		t.Fatalf("payload = %+v", decodedVerdict)
	}
	if !decodedVerdict.DecidedAt.Equal(decided) {
		t.Fatalf("decided at = %s, want %s", decodedVerdict.DecidedAt, decided)
	}
}

func TestUnit_AskWireShape(t *testing.T) {
	t.Parallel()

	rule := 3
	tests := []struct {
		name    string
		frame   librelay.Frame
		payload any
		want    string
	}{
		{
			name:  "published",
			frame: librelay.Frame{Type: librelay.TypeAskPublished, Instance: "inst-1"},
			payload: librelay.AskPublished{
				AskID:       "ask-1",
				SessionID:   "cnx-sess-1",
				MissionID:   "m-1",
				AgentName:   "refund-desk",
				ToolsName:   "billing",
				ToolName:    "issue_refund",
				PolicyName:  "hitl-policy-default.json",
				MatchedRule: &rule,
				ArgsSummary: "refund 40 EUR to customer 8812",
				ExpiresAt:   time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
			},
			want: `{"ask_id":"ask-1","session_id":"cnx-sess-1","mission_id":"m-1","agent_name":"refund-desk","tools_name":"billing","tool_name":"issue_refund","policy_name":"hitl-policy-default.json","matched_rule":3,"args_summary":"refund 40 EUR to customer 8812","expires_at":"2026-08-16T12:00:00Z"}`,
		},
		{
			name:    "published without an expiry",
			frame:   librelay.Frame{Type: librelay.TypeAskPublished, Instance: "inst-1"},
			payload: librelay.AskPublished{AskID: "ask-2"},
			want:    `{"ask_id":"ask-2"}`,
		},
		{
			name:    "resolved",
			frame:   librelay.Frame{Type: librelay.TypeAskResolved, Instance: "inst-1"},
			payload: librelay.AskResolved{AskID: "ask-1", Reason: librelay.AskResolvedExpired},
			want:    `{"ask_id":"ask-1","reason":"expired"}`,
		},
		{
			name:    "verdict allowing",
			frame:   librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"},
			payload: librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAllow},
			want:    `{"ask_id":"ask-1","decision":"allow"}`,
		},
		{
			name:  "verdict answering",
			frame: librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"},
			payload: librelay.AskVerdict{
				AskID:     "ask-1",
				Decision:  librelay.AskDecisionAnswer,
				Answer:    "use the 2019 pricing table",
				DecidedBy: "u_9",
				DecidedAt: time.Date(2026, 8, 16, 11, 30, 0, 0, time.UTC),
			},
			want: `{"ask_id":"ask-1","decision":"answer","answer":"use the 2019 pricing table","decided_by":"u_9","decided_at":"2026-08-16T11:30:00Z"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := tc.frame.WithPayload(tc.payload)
			if err != nil {
				t.Fatalf("WithPayload: %v", err)
			}
			if string(f.Payload) != tc.want {
				t.Fatalf("payload = %s, want %s", f.Payload, tc.want)
			}
		})
	}
}

func TestUnit_AskPublishedCarriesNoCallArguments(t *testing.T) {
	t.Parallel()

	forbidden := []string{"args", "arguments", "raw_input", "input", "params", "diff", "content", "payload"}
	ty := reflect.TypeOf(librelay.AskPublished{})
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		for _, bad := range forbidden {
			if name == bad {
				t.Fatalf("AskPublished.%s carries %q: an ask crossing to a business approver states a summary, never the gated call's own input", ty.Field(i).Name, name)
			}
		}
	}

	f, err := librelay.Frame{Type: librelay.TypeAskPublished, Instance: "inst-1"}.
		WithPayload(librelay.AskPublished{AskID: "ask-1", ArgsSummary: "refund 40 EUR to customer 8812"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.Payload, &fields); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, bad := range forbidden {
		if _, ok := fields[bad]; ok {
			t.Fatalf("payload carries %q: %s", bad, f.Payload)
		}
	}
}

func TestUnit_AskVerdictCarriesNoAuthorityModel(t *testing.T) {
	t.Parallel()

	forbidden := []string{"team", "team_id", "teams", "seat", "seats", "quorum", "approvers", "required", "role", "roles", "members", "n_of_m"}
	ty := reflect.TypeOf(librelay.AskVerdict{})
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		for _, bad := range forbidden {
			if name == bad {
				t.Fatalf("AskVerdict.%s carries %q: the verdict is already settled and the runtime never learns what a team is", ty.Field(i).Name, name)
			}
		}
	}
}

func TestUnit_AskFramesAreCargoNotControl(t *testing.T) {
	t.Parallel()

	for _, ty := range []string{librelay.TypeAskPublished, librelay.TypeAskResolved, librelay.TypeAskVerdict} {
		if librelay.IsControl(ty) {
			t.Fatalf("IsControl(%q) = true: an ask frame is cargo and a relay that does not know it must route it, not answer it", ty)
		}
	}
}

func TestUnit_AskFramesAreNotificationsAnOlderPeerCanDrop(t *testing.T) {
	t.Parallel()

	frames := []librelay.Frame{
		{Type: librelay.TypeAskPublished, Instance: "inst-1"},
		{Type: librelay.TypeAskResolved, Instance: "inst-1"},
		{Type: librelay.TypeAskVerdict, Instance: "inst-1"},
	}
	for _, f := range frames {
		if err := f.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", f.Type, err)
		}
		if f.IsRequest() || f.IsResponse() {
			t.Fatalf("%q classified as request/response %v/%v", f.Type, f.IsRequest(), f.IsResponse())
		}
		if _, owed := librelay.Unsupported(f); owed {
			t.Fatalf("Unsupported(%q) owes a reply: a peer that has never heard of the type must drop it and leave nobody waiting", f.Type)
		}
	}
}

func TestUnit_AskPayloadsIgnoreFieldsAddedLater(t *testing.T) {
	t.Parallel()

	var published librelay.AskPublished
	newer := `{"ask_id":"ask-1","args_summary":"refund 40 EUR","invented_later":{"deep":[1,2]},"expires_at":"2026-08-16T12:00:00Z"}`
	if err := json.Unmarshal([]byte(newer), &published); err != nil {
		t.Fatalf("decode a newer ask.published: %v", err)
	}
	if published.AskID != "ask-1" || published.ArgsSummary != "refund 40 EUR" ||
		!published.ExpiresAt.Equal(time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("payload = %+v", published)
	}

	var verdict librelay.AskVerdict
	newerVerdict := `{"ask_id":"ask-1","decision":"deny","decided_by":"u_9","settled_from":"a field a later relay added"}`
	if err := json.Unmarshal([]byte(newerVerdict), &verdict); err != nil {
		t.Fatalf("decode a newer ask.verdict: %v", err)
	}
	if verdict.AskID != "ask-1" || verdict.Decision != librelay.AskDecisionDeny || verdict.DecidedBy != "u_9" {
		t.Fatalf("payload = %+v", verdict)
	}
}

func TestUnit_AskVocabulariesAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for name, value := range map[string]string{
		"TypeAskPublished":       librelay.TypeAskPublished,
		"TypeAskResolved":        librelay.TypeAskResolved,
		"TypeAskVerdict":         librelay.TypeAskVerdict,
		"TypeChainTrigger":       librelay.TypeChainTrigger,
		"TypeChainTriggerResult": librelay.TypeChainTriggerResult,
		"TypeACPMessage":         librelay.TypeACPMessage,
		"TypeACPDetach":          librelay.TypeACPDetach,
		"TypeResume":             librelay.TypeResume,
		"TypeResumed":            librelay.TypeResumed,
	} {
		if other, ok := seen[value]; ok {
			t.Fatalf("%s and %s are both %q", name, other, value)
		}
		seen[value] = name
	}

	reasons := map[string]bool{librelay.AskResolvedAnswered: true, librelay.AskResolvedExpired: true, librelay.AskResolvedSuperseded: true}
	if len(reasons) != 3 {
		t.Fatalf("resolution reasons collide: %v", reasons)
	}
	decisions := map[string]bool{librelay.AskDecisionAllow: true, librelay.AskDecisionDeny: true, librelay.AskDecisionAnswer: true}
	if len(decisions) != 3 {
		t.Fatalf("verdict decisions collide: %v", decisions)
	}
}
