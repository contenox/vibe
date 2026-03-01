package vibecli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/planstore"
	"github.com/contenox/vibe/runtimetypes"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// planNameFromGoal creates a human-readable plan name from the goal text.
// e.g. "Fix the auth token expiry bug" → "fix-the-auth-token-expiry-a3f9e12b"
func planNameFromGoal(goal, suffix string) string {
	words := strings.Fields(strings.ToLower(goal))
	if len(words) > 5 {
		words = words[:5]
	}
	slug := nonAlnum.ReplaceAllString(strings.Join(words, "-"), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "plan"
	}
	return slug + "-" + suffix
}

const (
	kvActivePlan = "vibe.plan.active"
)

// getActivePlanID reads the active plan ID from the kv table.
// Returns ("", nil) if no active plan has been set yet.
func getActivePlanID(ctx context.Context, exec libdb.Exec) (string, error) {
	store := runtimetypes.New(exec)
	var id string
	if err := store.GetKV(ctx, kvActivePlan, &id); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read active plan: %w", err)
	}
	return id, nil
}

// setActivePlanID persists the active plan ID to the kv table.
func setActivePlanID(ctx context.Context, exec libdb.Exec, id string) error {
	store := runtimetypes.New(exec)
	raw, err := marshalJSON(id)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, kvActivePlan, raw)
}

// syncPlanMarkdown queries the DB for the given plan and writes a
// GitHub-style markdown file to .contenox/plans/<name>.md
func syncPlanMarkdown(ctx context.Context, exec libdb.Exec, planID string, contenoxDir string) error {
	store := planstore.New(exec)
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

	filePath := filepath.Join(plansDir, fmt.Sprintf("%s.md", plan.Name))
	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write markdown file: %w", err)
	}

	slog.Debug("Synced plan markdown", "path", filePath)
	return nil
}
