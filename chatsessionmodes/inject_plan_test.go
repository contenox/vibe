package chatsessionmodes

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/stretchr/testify/require"
)

type stubPlanSvc struct {
	plan  *planstore.Plan
	steps []*planstore.PlanStep
	err   error
}

var _ planservice.Service = (*stubPlanSvc)(nil)

func (s *stubPlanSvc) New(context.Context, string, *taskengine.TaskChainDefinition) (*planstore.Plan, []*planstore.PlanStep, string, error) {
	return nil, nil, "", nil
}
func (s *stubPlanSvc) Replan(context.Context, *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error) {
	return nil, "", nil
}
func (s *stubPlanSvc) Next(context.Context, planservice.Args, *taskengine.TaskChainDefinition, *taskengine.TaskChainDefinition) (string, string, error) {
	return "", "", nil
}
func (s *stubPlanSvc) Retry(context.Context, int) (string, error) { return "", nil }
func (s *stubPlanSvc) Skip(context.Context, int) (string, error)  { return "", nil }
func (s *stubPlanSvc) Active(context.Context) (*planstore.Plan, []*planstore.PlanStep, error) {
	return s.plan, s.steps, s.err
}
func (s *stubPlanSvc) Show(context.Context) (string, error) { return "", nil }
func (s *stubPlanSvc) List(context.Context) ([]*planstore.Plan, error) {
	return nil, nil
}
func (s *stubPlanSvc) SetActive(context.Context, string) error { return nil }
func (s *stubPlanSvc) Delete(context.Context, string) error    { return nil }
func (s *stubPlanSvc) Clean(context.Context) (int, error)       { return 0, nil }

func TestActivePlanInjector_NoPlan(t *testing.T) {
	t.Parallel()
	inj := &ActivePlanInjector{Plans: &stubPlanSvc{}}
	msgs, err := inj.Inject(context.Background(), InjectInput{
		Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Nil(t, msgs)
}

func TestActivePlanInjector_WithPlan(t *testing.T) {
	t.Parallel()
	inj := &ActivePlanInjector{Plans: &stubPlanSvc{
		plan: &planstore.Plan{ID: "p1", Name: "t", Goal: "g"},
	}}
	msgs, err := inj.Inject(context.Background(), InjectInput{Now: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Content, "[Context kind=active_plan]")
	require.Contains(t, msgs[0].Content, `"goal":"g"`)
}
