package benchreport

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/contenox/libtransport"
)

// Harness adapts a concrete backend to the benchmark runner. OpenSession opens a
// fresh (cold) session on the model under test; Turn builds the manifest-keyed
// prefix/suffix inputs for the given stable/volatile text (the caller owns prompt
// planning, so the runner stays backend-neutral).
type Harness interface {
	OpenSession(ctx context.Context) (transport.Session, error)
	Turn(stable, suffix string) (transport.PrefixInput, transport.SuffixInput)
}

// Scenario is the workspace-context shape to measure: a stable prefix reused
// across turns, a baseline and a changed suffix, an edited stable prefix that must
// miss the cache, and the suffix sizes for the headline TTFT curve.
type Scenario struct {
	Stable        string
	Suffix        string
	ChangedSuffix string
	EditedStable  string
	SuffixCurve   []string
	MaxDecode     int
}

type turnMetrics struct {
	prefix   transport.PrefixStatus
	suffix   transport.SuffixStatus
	prefixMS float64
	suffixMS float64
	ttftMS   float64
	decodeMS float64
	tokens   int
	text     string
}

// Run executes the scenario against the harness and returns the filled report.
// It drives only the transport.Session contract, so any backend that implements
// it (llama.cpp, OpenVINO, the in-memory service) can be measured identically.
func Run(ctx context.Context, h Harness, sc Scenario, meta Report) (Report, error) {
	r := meta
	max := sc.MaxDecode
	if max <= 0 {
		max = 16
	}

	// --- Cold baseline: fresh session, full prefill, decode. ---
	cold, err := h.OpenSession(ctx)
	if err != nil {
		return r, err
	}
	defer cold.Close()
	coldM, err := runTurn(ctx, h, cold, sc.Stable, sc.Suffix, max)
	if err != nil {
		return r, err
	}
	r.ColdFullPrefill = ColdPrefill{
		Tokens:    coldM.prefix.PrefixTokens,
		MS:        coldM.prefixMS,
		PromptTPS: perSec(coldM.prefix.PrefixTokens, coldM.prefixMS),
		TTFTMS:    coldM.ttftMS,
	}
	r.Decode = Decode{OutputTokens: coldM.tokens, TokensPerSec: perSec(coldM.tokens, coldM.decodeMS)}

	// --- Warm same prefix: reuse the live session with the identical prefix. ---
	warmM, err := runTurn(ctx, h, cold, sc.Stable, sc.Suffix, max)
	if err != nil {
		return r, err
	}
	r.WarmSamePrefix = WarmSame{
		CachedTokens: warmM.prefix.ReusedTokens,
		NewTokens:    warmM.prefix.PrefilledTokens,
		MS:           warmM.prefixMS,
		TTFTMS:       warmM.ttftMS,
		HitRate:      ratio(warmM.prefix.ReusedTokens, warmM.prefix.PrefixTokens),
	}

	// --- Warm changed suffix: stable KV stays warm, only the suffix changes. ---
	if sc.ChangedSuffix != "" {
		warmCh, err := runTurn(ctx, h, cold, sc.Stable, sc.ChangedSuffix, max)
		if err != nil {
			return r, err
		}
		coldRefSess, err := h.OpenSession(ctx)
		if err != nil {
			return r, err
		}
		coldRef, err := runTurn(ctx, h, coldRefSess, sc.Stable, sc.ChangedSuffix, max)
		coldRefSess.Close()
		if err != nil {
			return r, err
		}
		r.WarmChangedSuffix = WarmChanged{
			CachedTokens:     warmCh.prefix.ReusedTokens,
			SuffixTokens:     warmCh.suffix.SuffixTokens,
			MS:               warmCh.suffixMS,
			TTFTMS:           warmCh.ttftMS,
			OutputEqualsCold: warmCh.text == coldRef.text,
		}
	}

	// --- Edited stable segment: must be a precise cache miss, still correct. ---
	if sc.EditedStable != "" {
		editM, err := runTurn(ctx, h, cold, sc.EditedStable, sc.Suffix, max)
		if err != nil {
			return r, err
		}
		coldEditSess, err := h.OpenSession(ctx)
		if err != nil {
			return r, err
		}
		coldEdit, err := runTurn(ctx, h, coldEditSess, sc.EditedStable, sc.Suffix, max)
		coldEditSess.Close()
		if err != nil {
			return r, err
		}
		r.EditedStable = EditedStable{
			ExpectedCacheMiss: true,
			ActualCacheMiss:   editM.prefix.ReusedTokens < editM.prefix.PrefixTokens,
			OutputEqualsCold:  editM.text == coldEdit.text,
		}
	}

	// --- Headline curve: TTFT as the changed suffix grows. ---
	for _, suf := range sc.SuffixCurve {
		m, err := runTurn(ctx, h, cold, sc.Stable, suf, max)
		if err != nil {
			return r, err
		}
		r.SuffixTTFTCurve = append(r.SuffixTTFTCurve, SuffixTTFT{
			SuffixTokens: m.suffix.SuffixTokens,
			TTFTMS:       m.ttftMS,
		})
	}

	// --- Failure cases. ---
	r.FailureCases.CancelDecode = checkCancelDecode(ctx, h, sc)

	return r, nil
}

// runTurn issues EnsurePrefix -> PrefillSuffix -> Decode and measures each stage.
func runTurn(ctx context.Context, h Harness, sess transport.Session, stable, suffix string, maxDecode int) (turnMetrics, error) {
	var m turnMetrics
	pin, sin := h.Turn(stable, suffix)

	t0 := time.Now()
	ps, err := sess.EnsurePrefix(ctx, pin)
	if err != nil {
		return m, err
	}
	m.prefix = ps
	m.prefixMS = msSince(t0)

	t1 := time.Now()
	ss, err := sess.PrefillSuffix(ctx, sin)
	if err != nil {
		return m, err
	}
	m.suffix = ss
	m.suffixMS = msSince(t1)

	t2 := time.Now()
	ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: maxDecode, Seed: ptr(7), Temperature: fptr(0)})
	if err != nil {
		return m, err
	}
	first := true
	for chunk := range ch {
		if chunk.Error != nil {
			return m, chunk.Error
		}
		if first {
			m.ttftMS = msSince(t2)
			first = false
		}
		if chunk.Text != "" {
			m.tokens++
			m.text += chunk.Text
		}
	}
	m.decodeMS = msSince(t2)
	return m, nil
}

// checkCancelDecode confirms a decode started under an already-canceled context
// terminates with an error rather than streaming.
func checkCancelDecode(ctx context.Context, h Harness, sc Scenario) bool {
	sess, err := h.OpenSession(ctx)
	if err != nil {
		return false
	}
	defer sess.Close()
	pin, sin := h.Turn(sc.Stable, sc.Suffix)
	if _, err := sess.EnsurePrefix(ctx, pin); err != nil {
		return false
	}
	if _, err := sess.PrefillSuffix(ctx, sin); err != nil {
		return false
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	ch, err := sess.Decode(cctx, transport.DecodeConfig{MaxTokens: 32})
	if err != nil {
		return errors.Is(err, context.Canceled)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			return errors.Is(chunk.Error, context.Canceled)
		}
	}
	return false
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }
func perSec(n int, ms float64) float64 {
	if ms <= 0 {
		return 0
	}
	return float64(n) / (ms / 1000.0)
}
func ratio(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}
func ptr(i int) *int          { return &i }
func fptr(f float64) *float64 { return &f }
