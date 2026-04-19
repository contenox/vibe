package planstore_test

import (
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/planstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ── Plan CRUD ──────────────────────────────────────────────────────────────────

func TestUnit_CreateAndGetPlanByID(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{
		ID:   uuid.NewString(),
		Name: "plan-" + uuid.NewString()[:8],
		Goal: "build a rocket",
	}
	require.NoError(t, st.CreatePlan(ctx, plan))
	require.NotZero(t, plan.CreatedAt)
	require.NotZero(t, plan.UpdatedAt)
	require.Equal(t, planstore.PlanStatusActive, plan.Status)

	got, err := st.GetPlanByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, plan.ID, got.ID)
	require.Equal(t, plan.Name, got.Name)
	require.Equal(t, plan.Goal, got.Goal)
	require.Equal(t, planstore.PlanStatusActive, got.Status)
	require.WithinDuration(t, plan.CreatedAt, got.CreatedAt, time.Second)
}

func TestUnit_GetPlanByName(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "named-plan", Goal: "whatever"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	got, err := st.GetPlanByName(ctx, "named-plan")
	require.NoError(t, err)
	require.Equal(t, plan.ID, got.ID)
}

func TestUnit_GetPlanByID_NotFound(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	_, err := st.GetPlanByID(ctx, uuid.NewString())
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

func TestUnit_GetPlanByName_NotFound(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	_, err := st.GetPlanByName(ctx, "no-such-plan")
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

func TestUnit_ListPlans_Empty(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plans, err := st.ListPlans(ctx)
	require.NoError(t, err)
	require.Empty(t, plans)
}

func TestUnit_ListPlans(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	for i := 0; i < 3; i++ {
		require.NoError(t, st.CreatePlan(ctx, &planstore.Plan{
			ID:   uuid.NewString(),
			Name: "plan-" + uuid.NewString()[:8],
			Goal: "goal",
		}))
	}

	plans, err := st.ListPlans(ctx)
	require.NoError(t, err)
	require.Len(t, plans, 3)
}

func TestUnit_UpdatePlanStatus(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	require.NoError(t, st.UpdatePlanStatus(ctx, plan.ID, planstore.PlanStatusCompleted))

	got, err := st.GetPlanByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusCompleted, got.Status)
}

func TestUnit_DeletePlan(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))
	require.NoError(t, st.DeletePlan(ctx, plan.ID))

	_, err := st.GetPlanByID(ctx, plan.ID)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

func TestUnit_DeletePlan_NotFound(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	err := st.DeletePlan(ctx, uuid.NewString())
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

// ── GetActivePlan ─────────────────────────────────────────────────────────────

func TestUnit_GetActivePlan(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	got, err := st.GetActivePlan(ctx)
	require.NoError(t, err)
	require.Equal(t, plan.ID, got.ID)
	require.Equal(t, planstore.PlanStatusActive, got.Status)
}

func TestUnit_GetActivePlan_ReturnsLastUpdated(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	p1 := &planstore.Plan{ID: uuid.NewString(), Name: "p1-" + uuid.NewString()[:8], Goal: "g"}
	p2 := &planstore.Plan{ID: uuid.NewString(), Name: "p2-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, p1))
	time.Sleep(2 * time.Millisecond) // ensure distinct updated_at
	require.NoError(t, st.CreatePlan(ctx, p2))

	got, err := st.GetActivePlan(ctx)
	require.NoError(t, err)
	require.Equal(t, p2.ID, got.ID)
}

func TestUnit_GetActivePlan_NotFound(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	_, err := st.GetActivePlan(ctx)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

func TestUnit_GetActivePlan_IgnoresCompleted(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))
	require.NoError(t, st.UpdatePlanStatus(ctx, plan.ID, planstore.PlanStatusCompleted))

	_, err := st.GetActivePlan(ctx)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

// ── Step CRUD ─────────────────────────────────────────────────────────────────

func newPlan(t *testing.T, st planstore.Store, ctx interface{ Deadline() (time.Time, bool) }) *planstore.Plan {
	t.Helper()
	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	// ctx here is context.Context — cast needed because test helpers use context.TODO()
	return plan
}

func TestUnit_CreateAndListPlanSteps(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "step one"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "step two"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))

	got, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "step one", got[0].Description)
	require.Equal(t, planstore.StepStatusPending, got[0].Status)
}

func TestUnit_UpdatePlanStepStatus_Completed(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	step := &planstore.PlanStep{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "do it"}
	require.NoError(t, st.CreatePlanSteps(ctx, step))

	require.NoError(t, st.UpdatePlanStepStatus(ctx, step.ID, planstore.StepStatusCompleted, "done!"))

	steps, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.StepStatusCompleted, steps[0].Status)
	require.Equal(t, "done!", steps[0].ExecutionResult)
	require.False(t, steps[0].ExecutedAt.IsZero())
}

func TestUnit_UpdatePlanStepStatus_RetryResetsPending(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	step := &planstore.PlanStep{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "do it"}
	require.NoError(t, st.CreatePlanSteps(ctx, step))
	require.NoError(t, st.UpdatePlanStepStatus(ctx, step.ID, planstore.StepStatusFailed, "oops"))
	// Retry: put back to pending
	require.NoError(t, st.UpdatePlanStepStatus(ctx, step.ID, planstore.StepStatusPending, ""))

	steps, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.StepStatusPending, steps[0].Status)
	require.Empty(t, steps[0].ExecutionResult)
	require.True(t, steps[0].ExecutedAt.IsZero())
}

func TestUnit_DeletePendingPlanSteps(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "done"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "pending"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))
	require.NoError(t, st.UpdatePlanStepStatus(ctx, steps[0].ID, planstore.StepStatusCompleted, "ok"))

	require.NoError(t, st.DeletePendingPlanSteps(ctx, plan.ID))

	got, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, planstore.StepStatusCompleted, got[0].Status)
}

// ── ClaimNextPendingStep ──────────────────────────────────────────────────────

func TestUnit_ClaimNextPendingStep(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "first"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "second"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))

	claimed, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, steps[0].ID, claimed.ID)
	require.Equal(t, planstore.StepStatusRunning, claimed.Status)

	// Verify DB reflects running status.
	all, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.StepStatusRunning, all[0].Status)
	require.Equal(t, planstore.StepStatusPending, all[1].Status)
}

func TestUnit_ClaimNextPendingStep_ClaimsLowestOrdinal(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	// Create out of ordinal order to verify ORDER BY ordinal ASC.
	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 3, Description: "third"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "first"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "second"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))

	claimed, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Ordinal)
}

func TestUnit_ClaimNextPendingStep_SkipsRunning(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "first"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "second"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))

	// Claim step 1.
	_, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.NoError(t, err)

	// Claim again — should get step 2 (step 1 is running, not pending).
	claimed2, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 2, claimed2.Ordinal)
}

func TestUnit_ClaimNextPendingStep_NoSteps(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	_, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

func TestUnit_ClaimNextPendingStep_AllCompleted(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	step := &planstore.PlanStep{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "done"}
	require.NoError(t, st.CreatePlanSteps(ctx, step))
	require.NoError(t, st.UpdatePlanStepStatus(ctx, step.ID, planstore.StepStatusCompleted, "ok"))

	_, err := st.ClaimNextPendingStep(ctx, plan.ID)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

// ── CascadeDelete ─────────────────────────────────────────────────────────────

func TestUnit_DeletePlan_CascadesSteps(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{ID: uuid.NewString(), Name: "p-" + uuid.NewString()[:8], Goal: "g"}
	require.NoError(t, st.CreatePlan(ctx, plan))

	steps := []*planstore.PlanStep{
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 1, Description: "s1"},
		{ID: uuid.NewString(), PlanID: plan.ID, Ordinal: 2, Description: "s2"},
	}
	require.NoError(t, st.CreatePlanSteps(ctx, steps...))

	require.NoError(t, st.DeletePlan(ctx, plan.ID))

	// Steps should be gone (CASCADE).
	got, err := st.ListPlanSteps(ctx, plan.ID)
	require.NoError(t, err)
	require.Empty(t, got)
}

// ── SessionID ─────────────────────────────────────────────────────────────────

func TestUnit_Plan_SessionID_RoundTrips(t *testing.T) {
	ctx, db := SetupStore(t)
	st := planstore.New(db.WithoutTransaction())

	plan := &planstore.Plan{
		ID:        uuid.NewString(),
		Name:      "p-" + uuid.NewString()[:8],
		Goal:      "g",
		SessionID: "sess-abc",
	}
	require.NoError(t, st.CreatePlan(ctx, plan))

	got, err := st.GetPlanByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, "sess-abc", got.SessionID)
}
