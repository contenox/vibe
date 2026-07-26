package tooleval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/toolguidance"
)

// RunOptions configures one scenario run.
type RunOptions struct {
	// GuidanceOn selects whether the real toolguidance envelope wraps the tools for
	// this run — the CONTENOX_TOOL_GUIDANCE on/off arm of the A/B. The real
	// toolguidance.WrapFromEnv path is exercised (env set briefly at construction),
	// so this is the production switch, not a harness re-implementation of it.
	GuidanceOn bool
	// DB backs local_fs's read-before-write guard. Nil (the harness default) degrades
	// that guard to a no-op, which is deliberate: the harness measures navigation and
	// the task invariant, not the read-before-write rule (that has its own localtools
	// unit tests). Pass a schema'd DB to exercise the guard too.
	DB libdb.DBManager
	// Seed / Temperature are recorded into the result for determinism honesty. They do
	// not themselves drive a scripted model; engineModel forwards them to the provider.
	Seed        *int
	Temperature *float64
	// RunID scopes the toolguidance session counters and namespaces the workspace, so
	// two arms of the same scenario never share counter state or files.
	RunID string
}

// Run materializes the scenario, drives the agentic loop against model with the REAL
// localtools pipeline (optionally wrapped by the REAL toolguidance envelope), collects
// the tool-call trace with per-call format validity, scores both axes, and returns the
// result. workspaceRoot is a caller-owned temp dir (usually t.TempDir()).
//
// The loop is the whole harness: one model turn per iteration, bounded by
// meta.max_iterations with the overflow flagged (the runaway stopper). It is the ONLY
// loop — engineModel and the scripted responders differ only as the Model seam, so the
// hermetic self-test exercises the same plumbing a live-model matrix run does.
func Run(ctx context.Context, s *Scenario, model Model, workspaceRoot string, opts RunOptions) (*RunResult, error) {
	started := time.Now()
	ws, err := s.Materialize(workspaceRoot)
	if err != nil {
		return nil, err
	}

	repo, providerName := buildTools(ws, opts)
	tools, err := advertise(ctx, repo, providerName)
	if err != nil {
		return nil, fmt.Errorf("tooleval: advertise tools: %w", err)
	}

	// Scope the guidance counters to this run (the blueprint's per-session "3rd
	// identical list_dir this session"); harmless when guidance is off.
	runCtx := toolguidance.WithSession(ctx, s.ID+":"+opts.RunID)

	res := &RunResult{
		Scenario:    s.ID,
		Model:       model.Name(),
		GuidanceOn:  opts.GuidanceOn,
		Seed:        opts.Seed,
		Temperature: opts.Temperature,
		StartedAt:   started.UTC(),
	}

	convo := []Message{{Role: RoleUser, Content: s.Instruction}}

	for iter := 1; iter <= s.Meta.MaxIterations; iter++ {
		if err := runCtx.Err(); err != nil {
			return res, err
		}
		res.Iterations = iter

		asst, err := model.Turn(runCtx, convo, tools)
		if err != nil {
			res.DurationMS = msSince(started)
			return res, fmt.Errorf("tooleval: model turn %d: %w", iter, err)
		}
		convo = append(convo, Message{Role: RoleAssistant, Content: asst.Content, ToolCalls: asst.ToolCalls})

		if len(asst.ToolCalls) == 0 {
			res.Completed = true
			break
		}

		for _, tc := range asst.ToolCalls {
			rec := ToolCallRecord{Iteration: iter, Name: tc.Name, RawArgs: tc.Arguments}

			var args map[string]any
			if err := json.Unmarshal([]byte(orEmptyObject(tc.Arguments)), &args); err != nil {
				// FORMAT axis: malformed arguments. Recorded, fed back as a tool
				// error so the model MAY self-correct — never repaired by the harness.
				rec.ArgsValid = false
				res.Trace = append(res.Trace, rec)
				convo = append(convo, toolResult(tc.ID, fmt.Sprintf(
					"tool-eval: could not parse arguments as JSON (%v); resend valid JSON arguments", err)))
				continue
			}
			rec.ArgsValid = true
			provider, leaf := resolveToolName(tc.Name, providerName)
			rec.Provider, rec.Tool = provider, leaf
			rec.Path = extractPath(args)

			result, dt, execErr := repo.Exec(runCtx, time.Now(), args, false, &taskengine.ToolsCall{Name: provider, ToolName: leaf})
			var content string
			if execErr != nil {
				rec.ExecErr = execErr.Error()
				content = "tool error: " + execErr.Error()
			} else {
				rec.ResultType = dataTypeName(dt)
				content = serializeResult(result, dt)
			}
			rec.ResultBytes = len(content)
			res.Trace = append(res.Trace, rec)
			convo = append(convo, toolResult(tc.ID, content))
		}
	}
	if !res.Completed {
		res.HitMaxIter = true
	}

	res.finalize()

	if s.Invariant != nil {
		pass, reasons := s.Invariant(ws, res)
		res.TaskPass = &pass
		res.Reasons = reasons
	}
	res.DurationMS = msSince(started)
	return res, nil
}

// buildTools constructs the REAL local_fs tool provider over the workspace, wrapped by
// the REAL toolguidance envelope iff opts.GuidanceOn — mirroring enginesvc.buildTools
// (localtools.NewLocalFSTools + toolguidance.WrapFromEnv). The env switch is set only
// across the WrapFromEnv call (which reads it once at construction), so no long-lived
// process-global state is mutated; the decorator never re-reads the env per call.
func buildTools(ws string, opts RunOptions) (taskengine.ToolsRepo, string) {
	inner := localtools.NewLocalFSTools(ws, opts.DB)
	repo := withGuidanceEnv(opts.GuidanceOn, func() taskengine.ToolsRepo {
		return toolguidance.WrapFromEnv(inner)
	})
	return repo, localtools.LocalFSToolsName
}

// withGuidanceEnv sets CONTENOX_TOOL_GUIDANCE for the duration of fn and restores it
// after. Not safe to call concurrently — but the guidance switch is process-wide by
// design (toolguidance.go), so a concurrent guidance A/B is not a coherent thing to
// ask for, and the runner drives arms sequentially.
func withGuidanceEnv(on bool, fn func() taskengine.ToolsRepo) taskengine.ToolsRepo {
	const key = "CONTENOX_TOOL_GUIDANCE"
	prev, had := os.LookupEnv(key)
	if on {
		_ = os.Setenv(key, "on")
	} else {
		_ = os.Setenv(key, "off")
	}
	defer func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	}()
	return fn()
}

// advertise lists the provider's leaf tools and namespaces them ("local_fs.list_dir"),
// exactly as taskenv.go does before handing them to the model.
func advertise(ctx context.Context, repo taskengine.ToolsRepo, providerName string) ([]ToolSpec, error) {
	tools, err := repo.GetToolsForToolsByName(ctx, providerName)
	if err != nil {
		return nil, err
	}
	out := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolSpec{
			Name:        providerName + "." + t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out, nil
}

// resolveToolName splits a model-emitted tool name into provider + leaf. A namespaced
// "local_fs.list_dir" splits on the first dot; a bare "list_dir" is treated as a leaf
// on the sole known provider. This mirrors taskexec.go's resolve-then-strip-prefix.
func resolveToolName(name, defaultProvider string) (provider, leaf string) {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i], name[i+1:]
	}
	return defaultProvider, name
}

func toolResult(id, content string) Message {
	return Message{Role: RoleTool, ToolCallID: id, Content: content}
}

// serializeResult mirrors taskengine.serializeToolResultContent so a tool result
// reaches the model in the same shape the real engine would produce.
func serializeResult(result any, dt taskengine.DataType) string {
	switch dt {
	case taskengine.DataTypeNil:
		return "null"
	case taskengine.DataTypeAny, taskengine.DataTypeJSON:
		b, err := json.Marshal(result)
		if err != nil {
			return fmt.Sprintf("%v", result)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", result)
	}
}

func dataTypeName(dt taskengine.DataType) string {
	return dt.String()
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }
