package chatsessionmodes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/google/uuid"
)

// ActivePlanInjector injects a snapshot of the active execution plan (if any) for plan mode.
type ActivePlanInjector struct {
	Plans planservice.Service
}

// Inject implements Injector. When there is no active plan, returns nil, nil.
func (a *ActivePlanInjector) Inject(ctx context.Context, in InjectInput) ([]taskengine.Message, error) {
	if a == nil || a.Plans == nil {
		return nil, nil
	}
	plan, steps, err := a.Plans.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("active plan: %w", err)
	}
	if plan == nil {
		return nil, nil
	}
	payload, err := json.Marshal(activePlanSnapshot{
		Plan:  plan,
		Steps: steps,
	})
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("[Context kind=active_plan]\n%s", string(payload))
	return []taskengine.Message{{
		ID:        uuid.NewString(),
		Role:      "system",
		Content:   body,
		Timestamp: in.Now,
	}}, nil
}

type activePlanSnapshot struct {
	Plan  *planstore.Plan       `json:"plan"`
	Steps []*planstore.PlanStep `json:"steps"`
}
