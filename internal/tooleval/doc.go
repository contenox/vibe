// Package tooleval is the incident-driven, per-model tool-eval harness sketched in
// the closing section of docs/development/blueprints/tool-hardening.md ("The eval
// harness"). It is the concrete home for that blueprint's rec 10 — "malformed-tool-
// call rate as a first-class per-model metric" — and the mechanism that would have
// caught "fine on Gemini, degrades elsewhere" before the first live fleet derailment.
//
// # What it measures (two independent axes, per model × scenario)
//
//   - Task success: did the scenario's INVARIANT hold (the task got done, the
//     hostile file was left alone) — asserted by a Go invariant func registered by
//     scenario id, NEVER by matching an exact tool-call sequence, because competent
//     models reach the same end through different paths (blueprint: "models differ
//     in path").
//   - First-attempt tool-format compliance: what fraction of the model's tool calls
//     arrived with arguments that parse as JSON at all. The harness NEVER repairs a
//     malformed call (repair is localtools' job, rec 6); it SCORES it, so the
//     malformed-rate is its own reported column beside pass/fail — aider's
//     percent_cases_well_formed, restated.
//
// # The seam (why the loop is swappable)
//
// The runner owns ONE agentic loop and drives the REAL localtools pipeline through
// the real taskengine.ToolsRepo contract and the real toolguidance envelope. The
// only swappable part is the Model interface: engineModel drives a configured real
// model (llmrepo-backed); a deterministic scripted responder drives the hermetic
// self-test. Same loop, same tools, same guidance layer — only the brain changes.
// This is the seam the tool-hardening harness sketch calls "replayed-response mode
// for determinism, live-model mode for the matrix".
//
// # Determinism honesty
//
// Model runs are measurements over a STOCHASTIC system, not assertions about one.
// The harness pins seed and temperature where the provider honors them, runs N=1 by
// default with a repeat knob, and records both in every result. A green matrix cell
// is evidence, not proof; a red cell (a specific model × scenario) is the signal.
// The toolguidance A/B scenario is measurement-only for exactly this reason: it
// reports the deltas its package's falsifiable claim names and asserts none of them
// (toolguidance.go, "What the eval harness must falsify").
//
// # Layout
//
//	runtime/tooleval/
//	  scenarios/<slug>/instruction.md   # the literal task handed to the model
//	                  /fixture/         # static on-disk tree, copied into a temp workspace
//	                  /meta.json        # {tool_family, required, max_iterations, tags}
//	  invariants.go                     # Register(id, invariantFunc) — the verify.go idea, in Go
//	  fixtures.go                       # Register(id, fixtureBuilder) — hostile shapes too big/odd to commit
//
// Fixtures and instructions are DATA (shareable with localtools unit tests, per the
// blueprint's "one ground truth"); invariants and the synthetic-fixture builders are
// Go, registered by scenario id, so discovery stays in the codebase's Go-test
// discipline rather than a shell verify.sh.
package tooleval
