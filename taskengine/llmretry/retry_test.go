package llmretry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ClassNone},
		{"canceled", context.Canceled, ClassCanceled},
		{"deadline", context.DeadlineExceeded, ClassTimeout},
		{"openai 429", fmt.Errorf("OpenAI API returned non-200 status: 429, body: rate limited for model gpt-4"), ClassRateLimit},
		{"anthropic 529", fmt.Errorf("anthropic 529 overloaded"), ClassRateLimit},
		{"openai 503", fmt.Errorf("OpenAI API returned non-200 status: 503 service unavailable"), ClassServerError},
		{"401 unauthorized", fmt.Errorf("status: 401 unauthorized"), ClassAuth},
		{"invalid api key", fmt.Errorf("invalid api key supplied"), ClassAuth},
		{"capacity exceeded", fmt.Errorf("input token count 200000 exceeds context length 128000"), ClassCapacity},
		{"timeout", fmt.Errorf("Post \"https://api/x\": net/http: request canceled (i/o timeout)"), ClassTimeout},
		{"unknown", fmt.Errorf("totally unexpected provider error"), ClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorClass_IsRetryable(t *testing.T) {
	retryable := []ErrorClass{ClassRateLimit, ClassServerError, ClassTimeout}
	for _, c := range retryable {
		if !c.IsRetryable() {
			t.Errorf("expected %q retryable", c)
		}
	}
	notRetryable := []ErrorClass{ClassNone, ClassAuth, ClassCapacity, ClassCanceled, ClassPermanent}
	for _, c := range notRetryable {
		if c.IsRetryable() {
			t.Errorf("expected %q not retryable", c)
		}
	}
}

func fastPolicy(p RetryPolicy) RetryPolicy {
	if p.InitialBackoff == 0 {
		p.InitialBackoff = Duration(time.Millisecond)
	}
	if p.MaxBackoff == 0 {
		p.MaxBackoff = Duration(2 * time.Millisecond)
	}
	return p
}

func TestDo_NoRetryOnAuth(t *testing.T) {
	calls := 0
	_, out, err := Do(context.Background(), fastPolicy(RetryPolicy{MaxAttempts: 5}), "primary", func(model string) (any, error) {
		calls++
		return nil, fmt.Errorf("status: 401 unauthorized")
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected single call, got %d", calls)
	}
	if out.LastErrorClass != ClassAuth {
		t.Fatalf("class = %s", out.LastErrorClass)
	}
	if out.UsedFallback {
		t.Fatalf("fallback should not trigger on auth")
	}
}

func TestDo_NoRetryOnCapacity(t *testing.T) {
	calls := 0
	_, out, err := Do(context.Background(), fastPolicy(RetryPolicy{MaxAttempts: 5}), "primary", func(model string) (any, error) {
		calls++
		return nil, fmt.Errorf("input token count 200000 exceeds context length 128000")
	})
	if err == nil || calls != 1 || out.LastErrorClass != ClassCapacity {
		t.Fatalf("calls=%d class=%s err=%v", calls, out.LastErrorClass, err)
	}
}

func TestDo_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	seq := []error{
		fmt.Errorf("status: 429 too many requests"),
		fmt.Errorf("status: 429 too many requests"),
		nil,
	}
	calls := 0
	res, out, err := Do(context.Background(), fastPolicy(RetryPolicy{MaxAttempts: 5}), "primary", func(model string) (any, error) {
		defer func() { calls++ }()
		if e := seq[calls]; e != nil {
			return nil, e
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.(string) != "ok" {
		t.Fatalf("res = %v", res)
	}
	if out.Attempts != 3 {
		t.Fatalf("attempts = %d", out.Attempts)
	}
	if out.UsedFallback {
		t.Fatalf("did not configure fallback")
	}
}

func TestDo_FallbackAfterThreshold(t *testing.T) {
	calls := 0
	models := []string{}
	_, out, err := Do(context.Background(), fastPolicy(RetryPolicy{
		MaxAttempts:     5,
		FallbackModelID: "fallback",
		FallbackAfter:   2,
	}), "primary", func(model string) (any, error) {
		models = append(models, model)
		calls++
		// Fail twice on primary, then succeed on whatever model is asked.
		if calls < 3 {
			return nil, fmt.Errorf("status: 503 service unavailable")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("expected success after fallback, got %v", err)
	}
	if !out.UsedFallback {
		t.Fatalf("expected fallback used")
	}
	if models[0] != "primary" || models[len(models)-1] != "fallback" {
		t.Fatalf("expected primary→fallback sequence, got %v", models)
	}
}

func TestDo_ExhaustsAttempts(t *testing.T) {
	calls := 0
	_, out, err := Do(context.Background(), fastPolicy(RetryPolicy{MaxAttempts: 3}), "primary", func(model string) (any, error) {
		calls++
		return nil, fmt.Errorf("status: 503")
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if calls != 3 || out.Attempts != 3 {
		t.Fatalf("calls=%d attempts=%d", calls, out.Attempts)
	}
	if out.LastErrorClass != ClassServerError {
		t.Fatalf("class = %s", out.LastErrorClass)
	}
}

func TestDo_ContextCanceledBeforeFirstCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Do(ctx, RetryPolicy{MaxAttempts: 5}, "primary", func(model string) (any, error) {
		t.Fatalf("call should not run after cancel")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDo_ContextCanceledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, _, err := Do(ctx, RetryPolicy{MaxAttempts: 5, InitialBackoff: Duration(50 * time.Millisecond), MaxBackoff: Duration(50 * time.Millisecond)}, "primary", func(model string) (any, error) {
		calls++
		// Cancel after first attempt; sleep should observe cancel.
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		return nil, fmt.Errorf("status: 503")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v (calls=%d)", err, calls)
	}
}

func TestBackoffFor_RateLimitFloor(t *testing.T) {
	p := RetryPolicy{
		InitialBackoff:   Duration(1 * time.Millisecond),
		MaxBackoff:       Duration(2 * time.Millisecond),
		RateLimitMinWait: Duration(100 * time.Millisecond),
	}
	got := backoffFor(p, 1, ClassRateLimit)
	if got < 100*time.Millisecond {
		t.Fatalf("rate-limit floor not applied: %v", got)
	}
	got = backoffFor(p, 1, ClassServerError)
	if got > 2*time.Millisecond {
		t.Fatalf("server-error should respect MaxBackoff, got %v", got)
	}
}

func TestDuration_JSON(t *testing.T) {
	type wrap struct {
		B Duration `json:"b"`
	}
	cases := []struct {
		in   string
		want time.Duration
	}{
		{`{"b":"1s"}`, time.Second},
		{`{"b":"500ms"}`, 500 * time.Millisecond},
		{`{"b":1000000000}`, time.Second},
		{`{"b":""}`, 0},
		{`{"b":null}`, 0},
		{`{}`, 0},
	}
	for _, tc := range cases {
		var w wrap
		if err := json.Unmarshal([]byte(tc.in), &w); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if w.B.D() != tc.want {
			t.Fatalf("%s: got %v want %v", tc.in, w.B.D(), tc.want)
		}
	}
	// Round-trip a policy using string form.
	pin := RetryPolicy{
		MaxAttempts:      3,
		InitialBackoff:   Duration(500 * time.Millisecond),
		MaxBackoff:       Duration(30 * time.Second),
		Jitter:           0.25,
		RateLimitMinWait: Duration(10 * time.Second),
	}
	b, err := json.Marshal(pin)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var pout RetryPolicy
	if err := json.Unmarshal(b, &pout); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, string(b))
	}
	if pout.InitialBackoff.D() != 500*time.Millisecond || pout.MaxBackoff.D() != 30*time.Second {
		t.Fatalf("roundtrip mismatch: %+v", pout)
	}
}

func TestDuration_InvalidString(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Fatalf("expected error for invalid duration string")
	}
}

func TestDo_ZeroPolicyMakesOneAttempt(t *testing.T) {
	calls := 0
	_, out, err := Do(context.Background(), RetryPolicy{}, "primary", func(model string) (any, error) {
		calls++
		return nil, fmt.Errorf("status: 503")
	})
	if err == nil || calls != 1 || out.Attempts != 1 {
		t.Fatalf("zero policy should make 1 attempt; calls=%d attempts=%d err=%v", calls, out.Attempts, err)
	}
}
