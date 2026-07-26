package acpsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/chatservice"
	libacp "github.com/contenox/beam/libacp"
)

const compactDefaultKeep = 8

// handleClear wipes the session's conversation history in place. The ACP
// session keeps its identity (and the client keeps its window); only the
// stored messages are removed.
func (t *Transport) handleClear(ctx context.Context, _ libacp.SessionID, sess *sessionEntry) (string, error) {
	mgr := chatservice.NewManager(sess.WorkspaceID)

	exec, commit, release, err := t.deps.DB.WithTransaction(ctx)
	if err != nil {
		return "", fmt.Errorf("start transaction: %w", err)
	}
	defer release()
	if err := mgr.ClearSession(ctx, exec, sess.InternalSessionID); err != nil {
		return "", err
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return "Conversation history cleared.", nil
}

// handleCompact summarizes older history into a single message, reclaiming
// context while keeping the last `keep` messages verbatim. It reuses the same
// chatservice.CompactHistory core the CLI's `session fork --summary` uses.
func (t *Transport) handleCompact(ctx context.Context, _ libacp.SessionID, sess *sessionEntry, args string) (string, error) {
	keep := compactDefaultKeep
	if s := strings.TrimSpace(args); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return "", fmt.Errorf("invalid keep count %q: expected a non-negative integer", s)
		}
		keep = n
	}

	mgr := chatservice.NewManager(sess.WorkspaceID)
	history, err := mgr.ListMessages(ctx, t.deps.DB.WithoutTransaction(), sess.InternalSessionID)
	if err != nil {
		return "", fmt.Errorf("load history: %w", err)
	}
	if len(history) == 0 {
		return "", fmt.Errorf("no history to compact")
	}

	chain, err := t.loadCompactChain()
	if err != nil {
		return "", err
	}

	templateVars := t.chainTemplateVars(sess)
	templateVars["chain"] = chain.ID
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)

	compacted, err := chatservice.CompactHistory(execCtx, t.deps.Engine.TaskService, chain, history, keep)
	if err != nil {
		return "", err
	}

	// Replace the stored history with the compacted set in place.
	exec, commit, release, err := t.deps.DB.WithTransaction(ctx)
	if err != nil {
		return "", fmt.Errorf("start transaction: %w", err)
	}
	defer release()
	if err := mgr.ClearSession(ctx, exec, sess.InternalSessionID); err != nil {
		return "", err
	}
	if err := mgr.PersistDiff(ctx, exec, sess.InternalSessionID, compacted); err != nil {
		return "", fmt.Errorf("persist compacted history: %w", err)
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	return fmt.Sprintf("Compacted %d messages to %d (kept last %d).", len(history), len(compacted), keep), nil
}

// loadCompactChain reads chain-compact.json from the active .contenox directory,
// falling back to ~/.contenox. It does not import the CLI's resolver (that would
// create an import cycle); this is a plain file lookup, not duplicated logic.
func (t *Transport) loadCompactChain() (*taskengine.TaskChainDefinition, error) {
	const name = "chain-compact.json"
	var candidates []string
	if t.deps.ContenoxDir != "" {
		candidates = append(candidates, filepath.Join(t.deps.ContenoxDir, name))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".contenox", name))
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(data, &chain); err != nil {
			return nil, fmt.Errorf("invalid chain JSON at %q: %w", p, err)
		}
		if chain.ID == "" {
			return nil, fmt.Errorf("chain at %q has empty ID", p)
		}
		return &chain, nil
	}
	return nil, fmt.Errorf("%s not found in %q or ~/.contenox; run 'contenox init' to populate it", name, t.deps.ContenoxDir)
}
