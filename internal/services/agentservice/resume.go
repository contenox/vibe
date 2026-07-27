// resume.go is the service half of the S6 durable-envelope slice: persisting
// a suspended run's checkpoint (checkpointSaver, installed on every Prompt
// context) and resuming it from ANY process (ResumeFromCheckpoint — the entry
// the post-S6 `contenox approvals respond` verb and hitlservice's resume hook
// both build on).
package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

var (
	// ErrNoCheckpoint reports that no suspended run is stored under the
	// approval ID — the clean "nothing to resume" answer (an in-session
	// fast-path approval, or an approval that never gated a suspension).
	ErrNoCheckpoint = errors.New("agentservice: no suspended run is checkpointed under this approval")
	// ErrApprovalUnanswered reports a resume attempted before the approval's
	// verdict landed. Resume is verdict-driven: answer the approval
	// (hitlservice.Respond) and the resume follows.
	ErrApprovalUnanswered = errors.New("agentservice: the approval backing this checkpoint has no verdict yet; respond to it first")
)

// resumeClaimStaleness bounds how long a resume claim excludes other
// resumers: a process killed mid-resume relinquishes its claim by staleness,
// so the run stays recoverable without any cooperative unlock.
const resumeClaimStaleness = 10 * time.Minute

// checkpointSaver adapts the runtimetypes checkpoint table to
// taskengine.CheckpointSaver, enriching the engine's checkpoint with the
// identity only this layer knows (session, chain ref) before it is
// serialized. Installed per run by Prompt and by ResumeFromCheckpoint (a
// resumed run may suspend again on a later gated call).
type checkpointSaver struct {
	store     runtimetypes.Store
	sessionID string
	chainRef  string
}

func (s *checkpointSaver) SaveCheckpoint(ctx context.Context, cp *taskengine.Checkpoint) error {
	cp.SessionID = s.sessionID
	cp.ChainRef = s.chainRef
	payload, err := taskengine.MarshalCheckpoint(cp)
	if err != nil {
		return fmt.Errorf("agentservice: serialize checkpoint for approval %s: %w", cp.ApprovalID, err)
	}
	row := &runtimetypes.ChainCheckpoint{
		ID:            cp.ApprovalID,
		SchemaVersion: taskengine.CheckpointSchemaVersion,
		Payload:       payload,
		SessionID:     cp.SessionID,
		RequestID:     cp.RequestID,
		CreatedAt:     cp.CreatedAt,
	}
	if err := s.store.CreateChainCheckpoint(ctx, row); err != nil {
		return fmt.Errorf("agentservice: persist checkpoint for approval %s: %w", cp.ApprovalID, err)
	}
	return nil
}

func (a *agent) checkpointSaver(sessionID, chainRef string) taskengine.CheckpointSaver {
	return &checkpointSaver{
		store:     runtimetypes.New(a.deps.DB.WithoutTransaction()),
		sessionID: sessionID,
		chainRef:  chainRef,
	}
}

// ResumeHook adapts this package's resume entry into the hook
// hitlservice.Respond invokes when a verdict arrives for an approval whose
// waiter is gone. Registration happens at the composition root
// (hitlservice.SetResumeHook(svc, agentservice.ResumeHook(deps))), keeping
// the dependency arrow pointing from the root downward — hitlservice never
// imports this package.
func ResumeHook(deps Deps) hitlservice.ResumeHook {
	return func(ctx context.Context, approvalID string) error {
		_, err := ResumeFromCheckpoint(ctx, deps, approvalID)
		if errors.Is(err, ErrNoCheckpoint) {
			return hitlservice.ErrNoCheckpoint
		}
		return err
	}
}

// resumeIfAlreadyAnswered closes the suspend-time race window: when the
// approval's verdict landed before the suspending Prompt could even return,
// no hook had a checkpoint to resume — so the suspending process resumes
// inline. Reported resumed=false whenever the approval is still pending (the
// normal case) or anything about the check fails (the hook path remains).
func (a *agent) resumeIfAlreadyAnswered(ctx context.Context, approvalID string) (*PromptResponse, bool) {
	store := runtimetypes.New(a.deps.DB.WithoutTransaction())
	row, err := store.GetHITLApproval(ctx, approvalID)
	if err != nil || row.State == runtimetypes.HITLApprovalPending {
		return nil, false
	}
	resp, err := ResumeFromCheckpoint(ctx, a.deps, approvalID)
	if err != nil {
		// The checkpoint stays annotated/claim-stale-recoverable; surfacing a
		// suspended response here keeps "prompt returned" truthful.
		reportErr, _, end := a.tracker().Start(ctx, "resume", "inline_after_suspend", "approval_id", approvalID)
		reportErr(err)
		end()
		return nil, false
	}
	return resp, true
}

// ResumeFromCheckpoint loads the run suspended under approvalID, injects the
// approval's verdict, and runs the chain to completion with the SAME
// persistence and event behavior as a normal run. It is process-independent
// by construction: any process that can build Deps (engine + DB) can resume a
// run another process suspended — the property the post-S6 CLI
// `approvals respond` verb depends on.
//
// Semantics:
//   - approved → the pending tool call executes and the chain continues;
//   - denied/expired → the existing deny semantics (the model sees the
//     standard deny message and the chain continues);
//   - the resumed run may suspend AGAIN on a later gated call — then the
//     consumed checkpoint is replaced by the new one and the response reports
//     the new approval ID;
//   - a successful terminal DELETES the checkpoint; a resume failure RETAINS
//     it with a failure annotation (never silently lose a run).
//
// Exactly one resumer proceeds per checkpoint (a claim CAS in the store); a
// concurrent resume observes ErrNoCheckpoint.
func ResumeFromCheckpoint(ctx context.Context, deps Deps, approvalID string) (*PromptResponse, error) {
	if deps.Engine == nil || deps.DB == nil {
		return nil, fmt.Errorf("agentservice: ResumeFromCheckpoint requires an engine and a database")
	}
	store := runtimetypes.New(deps.DB.WithoutTransaction())

	row, err := store.GetHITLApproval(ctx, approvalID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil, fmt.Errorf("agentservice: approval %s does not exist: %w", approvalID, err)
		}
		return nil, fmt.Errorf("agentservice: load approval %s: %w", approvalID, err)
	}
	approved, err := verdictFromApproval(row)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	if err := store.ClaimChainCheckpoint(ctx, approvalID, now, now.Add(-resumeClaimStaleness)); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			// No checkpoint, or another live resumer holds the claim — either
			// way there is nothing for THIS caller to run.
			return nil, fmt.Errorf("%w (approval %s)", ErrNoCheckpoint, approvalID)
		}
		return nil, fmt.Errorf("agentservice: claim checkpoint %s: %w", approvalID, err)
	}
	cpRow, err := store.GetChainCheckpoint(ctx, approvalID)
	if err != nil {
		return nil, fmt.Errorf("agentservice: load claimed checkpoint %s: %w", approvalID, err)
	}
	cp, err := taskengine.UnmarshalCheckpoint(cpRow.Payload)
	if err != nil {
		// Version drift or corruption: annotate and keep — the run is stranded
		// but visible, and a newer binary may load it.
		_ = store.SetChainCheckpointFailure(ctx, approvalID, "decode: "+err.Error())
		return nil, fmt.Errorf("agentservice: decode checkpoint %s (retained with failure annotation): %w", approvalID, err)
	}

	// Rebuild the exec environment the suspended run held: same request ID
	// (the durable event journal of that request continues across the
	// suspension — the documented linkage), same template vars, allowlist,
	// context length, and session identity.
	if cp.RequestID != "" {
		ctx = context.WithValue(ctx, libtracker.ContextKeyRequestID, cp.RequestID)
	} else {
		ctx = libtracker.WithNewRequestID(ctx)
	}
	ctx = taskengine.WithTemplateVars(ctx, cp.TemplateVars)
	if cp.ContextLength > 0 {
		ctx = taskengine.WithRequestedContextLength(ctx, cp.ContextLength)
	}
	if cp.HasToolsAllowlist {
		ctx = taskengine.WithRuntimeToolsAllowlist(ctx, cp.ToolsAllowlist)
	}
	if cp.SessionID != "" {
		ctx = context.WithValue(ctx, runtimetypes.SessionIDContextKey, cp.SessionID)
		ctx = llmrepo.WithSessionKey(ctx, llmrepo.DeriveSessionKey(cp.SessionID))
	}
	// The verdict rides the context; the HITL wrapper consumes it for exactly
	// this call ID instead of asking again. Later gated calls of the resumed
	// run gate normally (and may suspend afresh).
	ctx = taskengine.WithApprovalVerdicts(ctx, map[string]bool{approvalID: approved})
	ctx = taskengine.WithResumeCheckpoint(ctx, cp)
	ctx = taskengine.WithCheckpointSaver(ctx, &checkpointSaver{
		store:     store,
		sessionID: cp.SessionID,
		chainRef:  cp.ChainRef,
	})

	output, outputType, units, execErr := deps.Engine.TaskService.Execute(ctx, cp.Chain, cp.History, taskengine.DataTypeChatHistory)

	var susp *taskengine.ChainSuspendedError
	if execErr != nil && errors.As(execErr, &susp) {
		// Suspended again on a later call: the run is now represented by the
		// NEW checkpoint; the consumed one is done.
		if err := store.DeleteChainCheckpoint(ctx, approvalID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			return nil, fmt.Errorf("agentservice: run re-suspended as approval %s but deleting consumed checkpoint %s failed: %w", susp.ApprovalID, approvalID, err)
		}
		return &PromptResponse{
			Output:              output,
			OutputType:          outputType,
			Steps:               units,
			StopReason:          StopSuspended,
			SuspendedApprovalID: susp.ApprovalID,
		}, nil
	}
	if execErr != nil {
		// Keep the checkpoint, annotated: the verdict is recorded and a retry
		// (after the claim goes stale) can run the chain again.
		if annErr := store.SetChainCheckpointFailure(ctx, approvalID, execErr.Error()); annErr != nil {
			return nil, fmt.Errorf("agentservice: resume of approval %s failed (%v) AND annotating its checkpoint failed: %w", approvalID, execErr, annErr)
		}
		return &PromptResponse{
			Output:     output,
			OutputType: outputType,
			Steps:      units,
			StopReason: InferStopReason(execErr, units),
		}, fmt.Errorf("agentservice: resume of approval %s failed (checkpoint retained with failure annotation): %w", approvalID, execErr)
	}

	// Same persistence behavior as a normal run: SynthesizeHistory over the
	// checkpointed prior transcript + the resumed run's steps, deduped by
	// message ID in PersistDiff, so nothing the suspended segment already
	// produced is written twice.
	if cp.SessionID != "" {
		resumer := &agent{deps: deps}
		resumer.persistHistory(ctx, cp.SessionID, cp.History, units, nil, cp.ChainRef)
	}

	if err := store.DeleteChainCheckpoint(ctx, approvalID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
		// The run completed and persisted; a leftover row is noise, not loss —
		// report through the response error would overstate it, so wrap softly.
		return nil, fmt.Errorf("agentservice: resumed run for approval %s completed, but deleting its checkpoint failed: %w", approvalID, err)
	}

	return &PromptResponse{
		Output:     output,
		OutputType: outputType,
		Steps:      units,
		StopReason: InferStopReason(nil, units),
	}, nil
}

// verdictFromApproval maps a terminal approval row to the boolean verdict the
// resume injects. Expired rows carry their OnTimeout outcome in the stored
// resolution payload ({"approved": bool} — the documented shape on
// runtimetypes.HITLApproval.Resolution); absent or unparseable resolution
// denies, the safe direction.
func verdictFromApproval(row *runtimetypes.HITLApproval) (bool, error) {
	switch row.State {
	case runtimetypes.HITLApprovalApproved:
		return true, nil
	case runtimetypes.HITLApprovalDenied:
		return false, nil
	case runtimetypes.HITLApprovalExpired:
		var res struct {
			Approved *bool `json:"approved"`
		}
		if len(row.Resolution) > 0 && json.Unmarshal(row.Resolution, &res) == nil && res.Approved != nil {
			return *res.Approved, nil
		}
		return false, nil
	case runtimetypes.HITLApprovalPending:
		return false, fmt.Errorf("%w (approval %s)", ErrApprovalUnanswered, row.ID)
	default:
		return false, fmt.Errorf("agentservice: approval %s is in unknown state %q", row.ID, row.State)
	}
}
