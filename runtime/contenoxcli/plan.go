package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/planstore"
	"github.com/contenox/contenox/runtime/runtimetypes"
)


const kvActivePlan = "contenox.plan.active"

// getActivePlanID reads the active plan ID from the kv table.
// Returns ("", nil) if no active plan has been set yet.
func getActivePlanID(ctx context.Context, exec libdb.Exec, workspaceID string) (string, error) {
	store := runtimetypes.New(exec)
	var id string
	if err := store.GetWorkspaceKV(ctx, workspaceID, kvActivePlan, &id); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read active plan: %w", err)
	}
	return id, nil
}

func setActivePlanID(ctx context.Context, exec libdb.Exec, id string, workspaceID string) error {
	store := runtimetypes.New(exec)
	raw, err := json.Marshal(id)
	if err != nil {
		return fmt.Errorf("failed to marshal plan id: %w", err)
	}
	return store.SetWorkspaceKV(ctx, workspaceID, kvActivePlan, raw)
}

// syncPlanMarkdown queries the DB for the given plan and writes a
// GitHub-style markdown file to .contenox/plans/<name>.md
func syncPlanMarkdown(ctx context.Context, exec libdb.Exec, planID string, contenoxDir string, workspaceID string) error {
	store := planstore.New(exec, workspaceID)
	plan, err := store.GetPlanByID(ctx, planID)
	if err != nil {
		return fmt.Errorf("failed to load plan: %w", err)
	}

	steps, err := store.ListPlanSteps(ctx, planID)
	if err != nil {
		return fmt.Errorf("failed to load plan steps: %w", err)
	}

	plansDir := filepath.Join(contenoxDir, "plans")
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		return fmt.Errorf("failed to create plans directory: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Plan: %s\n\n", plan.Name))
	sb.WriteString(fmt.Sprintf("**Goal:** %s\n", plan.Goal))
	sb.WriteString(fmt.Sprintf("**Status:** %s\n\n", plan.Status))
	sb.WriteString("## Steps\n\n")

	for _, step := range steps {
		var checkbox string
		switch step.Status {
		case planstore.StepStatusCompleted:
			checkbox = "[x]"
		case planstore.StepStatusFailed, planstore.StepStatusSkipped:
			checkbox = "[-]"
		default:
			checkbox = "[ ]"
		}

		sb.WriteString(fmt.Sprintf("%d. %s **%s**\n", step.Ordinal, checkbox, step.Description))

		if step.ExecutionResult != "" {
			sb.WriteString(fmt.Sprintf("   > Result: %s\n", step.ExecutionResult))
		}
	}

	// filepath.Base prevents a crafted/hallucinated plan name like
	// "../../.ssh/authorized_keys" from writing outside the plans directory.
	safeFileName := filepath.Base(plan.Name) + ".md"
	filePath := filepath.Join(plansDir, safeFileName)
	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write markdown file: %w", err)
	}

	slog.Debug("Synced plan markdown", "path", filePath)
	return nil
}
