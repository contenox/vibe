// Package agentservice runs prompts against a task chain and persists their
// session history. resume.go persists a suspended run's checkpoint and
// resumes it from any process via ResumeFromCheckpoint.
package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

var (
	// ErrNoCheckpoint reports no suspended run stored under the approval ID.
	ErrNoCheckpoint = errors.New("agentservice: no suspended run is checkpointed under this approval")
	// ErrApprovalUnanswered reports a resume attempted before the verdict landed.
	ErrApprovalUnanswered = errors.New("agentservice: the approval backing this checkpoint has no verdict yet; respond to it first")
)

// resumeClaimStaleness bounds how long a resume claim excludes other resumers.
const resumeClaimStaleness = 10 * time.Minute

// checkpointSaver adapts the runtimetypes checkpoint table to taskengine.CheckpointSaver.
type checkpointSaver struct {
	store     runtimetypes.Store
	sessionID string
	chainRef  string
}

// checkpointEnvelope wraps taskengine's own versioned checkpoint bytes with
// one piece of state taskengine has no business owning: the session's
// workspace root (vfs.WithSessionCwd), captured from ctx at suspend time and
// restored onto the resumed run's ctx so a relative local_fs/git/jq path
// resolves exactly as the original call would have — never against whichever
// process happens to answer the approval. Additive on top of taskengine's own
// wire format: a payload with no "checkpoint" field is one written before
// this envelope existed (see unwrapCheckpointEnvelope).
type checkpointEnvelope struct {
	WorkspaceRoot string          `json:"workspace_root,omitempty"`
	Checkpoint    json.RawMessage `json:"checkpoint"`
}

// unwrapCheckpointEnvelope splits a persisted checkpoint row's payload back
// into taskengine's own wire bytes plus the workspace root this package
// wrapped around them. A payload with no "checkpoint" field predates the
// envelope: raw passes through unchanged and the root is "" (resolveCwd's
// per-tool fail-closed path then refuses any relative path it cannot anchor,
// rather than silently resolving against the resumer's own cwd).
func unwrapCheckpointEnvelope(raw []byte) (checkpoint []byte, workspaceRoot string) {
	var env checkpointEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Checkpoint) > 0 {
		return env.Checkpoint, env.WorkspaceRoot
	}
	return raw, ""
}

func (s *checkpointSaver) SaveCheckpoint(ctx context.Context, cp *taskengine.Checkpoint) error {
	cp.SessionID = s.sessionID
	cp.ChainRef = s.chainRef
	inner, err := taskengine.MarshalCheckpoint(cp)
	if err != nil {
		return fmt.Errorf("agentservice: serialize checkpoint for approval %s: %w", cp.ApprovalID, err)
	}
	payload, err := json.Marshal(checkpointEnvelope{
		WorkspaceRoot: vfs.SessionCwdFromContext(ctx),
		Checkpoint:    inner,
	})
	if err != nil {
		return fmt.Errorf("agentservice: envelope checkpoint for approval %s: %w", cp.ApprovalID, err)
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

// ResumeHook adapts ResumeFromCheckpoint into hitlservice.Respond's resume hook.
func ResumeHook(deps Deps) hitlservice.ResumeHook {
	return func(ctx context.Context, approvalID string) error {
		_, err := ResumeFromCheckpoint(ctx, deps, approvalID)
		if errors.Is(err, ErrNoCheckpoint) {
			return hitlservice.ErrNoCheckpoint
		}
		return err
	}
}

// resumeIfAlreadyAnswered resumes inline when a verdict beats the suspending Prompt's return.
func (a *agent) resumeIfAlreadyAnswered(ctx context.Context, approvalID string) (*PromptResponse, bool) {
	store := runtimetypes.New(a.deps.DB.WithoutTransaction())
	row, err := store.GetHITLApproval(ctx, approvalID)
	if err != nil || row.State == runtimetypes.HITLApprovalPending {
		return nil, false
	}
	resp, err := ResumeFromCheckpoint(ctx, a.deps, approvalID)
	if err != nil {
		reportErr, _, end := a.tracker().Start(ctx, "resume", "inline_after_suspend", "approval_id", approvalID)
		reportErr(err)
		end()
		return nil, false
	}
	return resp, true
}

// ResumeFromCheckpoint loads the run suspended under approvalID, injects the
// verdict, and runs the chain to completion, from any process holding Deps.
// Success deletes the checkpoint; failure retains it annotated. Exactly one
// resumer proceeds per checkpoint; a concurrent resume observes ErrNoCheckpoint.
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
	// An attention ask resumes with text; a permission ask keeps the boolean path.
	isAttention := hitlservice.IsAttentionAsk(row)
	var approved bool
	if isAttention {
		if row.State == runtimetypes.HITLApprovalPending {
			return nil, fmt.Errorf("%w (ask %s)", ErrApprovalUnanswered, row.ID)
		}
	} else {
		approved, err = verdictFromApproval(row)
		if err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	if err := store.ClaimChainCheckpoint(ctx, approvalID, now, now.Add(-resumeClaimStaleness)); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			// No checkpoint, or another live resumer holds the claim.
			return nil, fmt.Errorf("%w (approval %s)", ErrNoCheckpoint, approvalID)
		}
		return nil, fmt.Errorf("agentservice: claim checkpoint %s: %w", approvalID, err)
	}
	cpRow, err := store.GetChainCheckpoint(ctx, approvalID)
	if err != nil {
		return nil, fmt.Errorf("agentservice: load claimed checkpoint %s: %w", approvalID, err)
	}
	innerPayload, workspaceRoot := unwrapCheckpointEnvelope(cpRow.Payload)
	cp, err := taskengine.UnmarshalCheckpoint(innerPayload)
	if err != nil {
		// Version drift or corruption: annotate and keep, stranded but visible.
		_ = store.SetChainCheckpointFailure(ctx, approvalID, "decode: "+err.Error())
		return nil, fmt.Errorf("agentservice: decode checkpoint %s (retained with failure annotation): %w", approvalID, err)
	}

	// Rebuild the suspended run's exec environment.
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
	// Restores the session's own workspace root (see checkpointEnvelope) so a
	// resumed local_fs/git/jq call anchors a relative path exactly as the
	// original run would have, regardless of this process's own cwd.
	ctx = vfs.WithSessionCwd(ctx, workspaceRoot)
	// Without it, mission tools are invisible to the resumed chain.
	if row.MissionID != nil && *row.MissionID != "" {
		ctx = missiontools.WithMissionID(ctx, *row.MissionID)
	}
	// The verdict rides the context, keyed by call ID.
	if isAttention {
		ans := taskengine.AttentionAnswer{}
		if text := strings.TrimSpace(hitlservice.AnswerOf(row)); text != "" {
			ans = taskengine.AttentionAnswer{Answered: true, Text: text}
		}
		ctx = taskengine.WithAttentionAnswers(ctx, map[string]taskengine.AttentionAnswer{approvalID: ans})
	} else {
		ctx = taskengine.WithApprovalVerdicts(ctx, map[string]bool{approvalID: approved})
	}
	ctx = taskengine.WithResumeCheckpoint(ctx, cp)
	ctx = taskengine.WithCheckpointSaver(ctx, &checkpointSaver{
		store:     store,
		sessionID: cp.SessionID,
		chainRef:  cp.ChainRef,
	})

	output, outputType, units, execErr := deps.Engine.TaskService.Execute(ctx, cp.Chain, cp.History, taskengine.DataTypeChatHistory)

	var susp *taskengine.ChainSuspendedError
	if execErr != nil && errors.As(execErr, &susp) {
		// Suspended again: the run is now represented by the new checkpoint.
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
		// Keep the checkpoint, annotated, so a retry can run it again.
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

	// Deduped by message ID: nothing already produced is written twice.
	if cp.SessionID != "" {
		resumer := &agent{deps: deps}
		resumer.persistHistory(ctx, cp.SessionID, cp.History, units, nil, cp.ChainRef)
	}

	if err := store.DeleteChainCheckpoint(ctx, approvalID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
		// The run completed and persisted; a leftover row is noise, not loss.
		return nil, fmt.Errorf("agentservice: resumed run for approval %s completed, but deleting its checkpoint failed: %w", approvalID, err)
	}

	return &PromptResponse{
		Output:     output,
		OutputType: outputType,
		Steps:      units,
		StopReason: InferStopReason(nil, units),
	}, nil
}

// verdictFromApproval maps a terminal approval row to the injected verdict;
// absent or unparseable resolution denies.
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
