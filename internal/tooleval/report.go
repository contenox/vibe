package tooleval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ToolCallRecord is one row of the tool-call trace: everything the two axes and the
// invariants are scored from. It is deliberately a record of what the model DID, not
// a prescription of what it should have done.
type ToolCallRecord struct {
	Iteration int    `json:"iteration"`
	Name      string `json:"name"`               // provider-namespaced, as the model emitted it
	Provider  string `json:"provider,omitempty"` // resolved provider ("local_fs")
	Tool      string `json:"tool,omitempty"`     // resolved leaf ("list_dir")
	RawArgs   string `json:"raw_args"`           // the exact argument string the model produced
	// ArgsValid is the FORMAT axis for this call: did RawArgs parse as JSON. A false
	// here is a malformed call — recorded, fed back as an error result, never repaired.
	ArgsValid   bool   `json:"args_valid"`
	Path        string `json:"path,omitempty"` // best-effort declared path (scope signal; see metrics.go limits)
	ResultType  string `json:"result_type,omitempty"`
	ResultBytes int    `json:"result_bytes"`
	ExecErr     string `json:"exec_err,omitempty"`
}

// Metrics are the navigation-scope quantities the toolguidance falsifiable claim
// names (toolguidance.go): with guidance on, repeat-call and re-read rates should
// fall and landed-scope should shrink WITHOUT a rise in malformed-rate. The harness
// computes them independently from the trace — "the harness measures the deltas;
// this package only supplies the signal and the switch".
type Metrics struct {
	RepeatIdenticalCalls int `json:"repeat_identical_calls"` // calls that repeated an earlier identical (tool,args)
	ReReads              int `json:"re_reads"`               // read-like calls on an already-read path
	DistinctPaths        int `json:"distinct_paths"`         // scope: how many distinct paths were touched
	DistinctReadPaths    int `json:"distinct_read_paths"`
}

// RunResult is one model × scenario × guidance-arm run.
type RunResult struct {
	Scenario   string `json:"scenario"`
	Model      string `json:"model"`
	Provider   string `json:"provider,omitempty"`
	GuidanceOn bool   `json:"guidance_on"`

	Iterations int  `json:"iterations"`
	Completed  bool `json:"completed"` // model ended the loop itself (emitted a no-tool turn)
	HitMaxIter bool `json:"hit_max_iterations"`

	// TaskPass is the task-success axis. Nil means measurement-only (a scenario with
	// no invariant, e.g. guidance-ab) — the report row IS the deliverable there.
	TaskPass *bool    `json:"task_pass,omitempty"`
	Reasons  []string `json:"reasons,omitempty"`

	// Format axis.
	TotalCalls     int  `json:"total_calls"`
	MalformedCalls int  `json:"malformed_calls"`
	WellFormed     bool `json:"well_formed"` // aider's per-case sense: zero malformed calls

	Metrics Metrics          `json:"metrics"`
	Trace   []ToolCallRecord `json:"trace"`

	// Determinism honesty: recorded, not assumed. Nil where the provider did not
	// take the knob.
	Seed        *int     `json:"seed,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	DurationMS float64   `json:"duration_ms"`
	Notes      []string  `json:"notes,omitempty"`
}

// MalformedRate is malformed calls over total calls; 0 when no calls were made.
func (r *RunResult) MalformedRate() float64 {
	if r.TotalCalls == 0 {
		return 0
	}
	return float64(r.MalformedCalls) / float64(r.TotalCalls)
}

// finalize computes the format axis and the scope metrics from the collected trace.
func (r *RunResult) finalize() {
	r.TotalCalls = len(r.Trace)
	r.MalformedCalls = 0
	for _, t := range r.Trace {
		if !t.ArgsValid {
			r.MalformedCalls++
		}
	}
	r.WellFormed = r.MalformedCalls == 0
	r.Metrics = computeMetrics(r.Trace)
}

// MatrixRow is one cell of the published matrix (blueprint: "published as a matrix
// ... so 'works on Gemini, breaks on X' is a specific red cell"). Malformed-rate is
// its own column (rec 10).
type MatrixRow struct {
	Model         string   `json:"model"`
	Provider      string   `json:"provider,omitempty"`
	Scenario      string   `json:"scenario"`
	GuidanceOn    bool     `json:"guidance_on"`
	TaskPass      *bool    `json:"task_pass,omitempty"`
	WellFormed    bool     `json:"well_formed"`
	MalformedRate float64  `json:"malformed_rate"`
	Iterations    int      `json:"iterations"`
	HitMaxIter    bool     `json:"hit_max_iterations"`
	Metrics       Metrics  `json:"metrics"`
	Seed          *int     `json:"seed,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
}

func rowFromRun(r *RunResult) MatrixRow {
	return MatrixRow{
		Model:         r.Model,
		Provider:      r.Provider,
		Scenario:      r.Scenario,
		GuidanceOn:    r.GuidanceOn,
		TaskPass:      r.TaskPass,
		WellFormed:    r.WellFormed,
		MalformedRate: r.MalformedRate(),
		Iterations:    r.Iterations,
		HitMaxIter:    r.HitMaxIter,
		Metrics:       r.Metrics,
		Seed:          r.Seed,
		Temperature:   r.Temperature,
	}
}

// ABDelta is one guidance A/B measurement: the same scenario run guidance-off vs on.
// It asserts NOTHING (measurement, not a gate); the delta fields ARE the deliverable.
// A negative delta on repeats/re-reads/scope with a non-positive malformed delta is
// the toolguidance claim holding; the opposite (or a malformed rise) is it failing.
type ABDelta struct {
	Scenario string `json:"scenario"`
	Model    string `json:"model"`

	RepeatDelta   int      `json:"repeat_delta"`  // on - off (want <= 0)
	ReReadDelta   int      `json:"re_read_delta"` // on - off (want <= 0)
	ScopeDelta    int      `json:"scope_delta"`   // on - off (want <= 0)
	MalformedRate struct { // want on <= off (no weak-model noise failure)
		Off float64 `json:"off"`
		On  float64 `json:"on"`
	} `json:"malformed_rate"`
}

func abDelta(off, on *RunResult) ABDelta {
	d := ABDelta{Scenario: off.Scenario, Model: off.Model}
	d.RepeatDelta = on.Metrics.RepeatIdenticalCalls - off.Metrics.RepeatIdenticalCalls
	d.ReReadDelta = on.Metrics.ReReads - off.Metrics.ReReads
	d.ScopeDelta = on.Metrics.DistinctPaths - off.Metrics.DistinctPaths
	d.MalformedRate.Off = off.MalformedRate()
	d.MalformedRate.On = on.MalformedRate()
	return d
}

// Report is the full harness emission: the flat matrix, the guidance A/B deltas, and
// the per-run detail (trace included) for offline forensics.
type Report struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Rows        []MatrixRow `json:"rows"`
	ABDeltas    []ABDelta   `json:"ab_deltas,omitempty"`
	Runs        []RunResult `json:"runs"`
	Notes       []string    `json:"notes,omitempty"`
}

// NewReport assembles a Report from a set of runs and any guidance A/B pairs.
func NewReport(runs []*RunResult, abPairs [][2]*RunResult, notes ...string) Report {
	rep := Report{GeneratedAt: time.Now().UTC(), Notes: notes}
	for _, r := range runs {
		rep.Rows = append(rep.Rows, rowFromRun(r))
		rep.Runs = append(rep.Runs, *r)
	}
	for _, p := range abPairs {
		rep.ABDeltas = append(rep.ABDeltas, abDelta(p[0], p[1]))
	}
	return rep
}

// WriteJSON writes the full report as indented JSON.
func (rep Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// WriteMatrix renders the human-facing matrix table to w: one line per cell, with
// malformed-rate as its own column beside pass/fail, then the guidance A/B deltas.
func (rep Report) WriteMatrix(w io.Writer) {
	rows := append([]MatrixRow(nil), rep.Rows...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Scenario != rows[j].Scenario {
			return rows[i].Scenario < rows[j].Scenario
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return !rows[i].GuidanceOn && rows[j].GuidanceOn
	})
	fmt.Fprintln(w, "tool-eval matrix (model × scenario) — measurements over a stochastic system, N=1 unless noted")
	fmt.Fprintf(w, "%-24s %-22s %-6s %-5s %-8s %-9s %-5s %s\n",
		"SCENARIO", "MODEL", "GUIDE", "TASK", "WELLFRM", "MALFORM%", "ITERS", "SCOPE(paths/reads/repeats)")
	for _, r := range rows {
		fmt.Fprintf(w, "%-24s %-22s %-6s %-5s %-8s %-9.1f %-5d %d/%d/%d\n",
			trunc(r.Scenario, 24), trunc(r.Model, 22), onoff(r.GuidanceOn), passStr(r.TaskPass),
			yn(r.WellFormed), r.MalformedRate*100, r.Iterations,
			r.Metrics.DistinctPaths, r.Metrics.ReReads, r.Metrics.RepeatIdenticalCalls)
	}
	if len(rep.ABDeltas) > 0 {
		fmt.Fprintln(w, "\nguidance A/B deltas (on - off; negative repeat/reread/scope = claim holding; measurement only)")
		for _, d := range rep.ABDeltas {
			fmt.Fprintf(w, "  %-24s %-22s repeat %+d  reread %+d  scope %+d  malformed off=%.1f%% on=%.1f%%\n",
				trunc(d.Scenario, 24), trunc(d.Model, 22), d.RepeatDelta, d.ReReadDelta, d.ScopeDelta,
				d.MalformedRate.Off*100, d.MalformedRate.On*100)
		}
	}
}

// WriteToFile writes the JSON report to a timestamped file under dir, creating dir,
// and returns the path. Used by the gated suite so a run lands in a results file.
func (rep Report) WriteToFile(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("tooleval-%s.json", rep.GeneratedAt.Format("20060102-150405"))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := rep.WriteJSON(f); err != nil {
		return "", err
	}
	return path, nil
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func passStr(p *bool) string {
	switch {
	case p == nil:
		return "—"
	case *p:
		return "PASS"
	default:
		return "FAIL"
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
