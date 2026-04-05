package planapi

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
)

// AddPlanRoutes registers all /plans routes on mux.
func AddPlanRoutes(mux *http.ServeMux, svc planservice.Service, chains taskchainservice.Service) {
	h := &handler{svc: svc, chains: chains}

	mux.HandleFunc("POST /plans", h.newPlan)
	mux.HandleFunc("GET /plans", h.listPlans)
	mux.HandleFunc("POST /plans/clean", h.cleanPlans)
	mux.HandleFunc("GET /plans/active", h.getActive)
	mux.HandleFunc("POST /plans/active/next", h.nextStep)
	mux.HandleFunc("POST /plans/active/replan", h.replan)
	mux.HandleFunc("POST /plans/active/steps/{ordinal}/retry", h.retryStep)
	mux.HandleFunc("POST /plans/active/steps/{ordinal}/skip", h.skipStep)
	mux.HandleFunc("PUT /plans/{name}/activate", h.activate)
	mux.HandleFunc("DELETE /plans/{name}", h.deletePlan)
}

type handler struct {
	svc    planservice.Service
	chains taskchainservice.Service
}

// lookupChain fetches a TaskChainDefinition by ID from the chain service.
func (h *handler) lookupChain(r *http.Request, id string) (*taskengine.TaskChainDefinition, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: chain_id is required", apiframework.ErrBadRequest)
	}
	chain, err := h.chains.Get(r.Context(), id)
	if err != nil {
		return nil, fmt.Errorf("chain %q not found: %w", id, err)
	}
	return chain, nil
}

// ── Request/Response types ────────────────────────────────────────────────────

type newPlanRequest struct {
	Goal           string `json:"goal"`
	PlannerChainID string `json:"planner_chain_id"`
}

type newPlanResponse struct {
	Plan     *planstore.Plan       `json:"plan"`
	Steps    []*planstore.PlanStep `json:"steps"`
	Markdown string                `json:"markdown"`
}

type nextStepRequest struct {
	ExecutorChainID string `json:"executor_chain_id"`
	WithShell       bool   `json:"with_shell"`
	WithAuto        bool   `json:"with_auto"`
}

type nextStepResponse struct {
	Result   string `json:"result"`
	Markdown string `json:"markdown"`
}

type replanRequest struct {
	PlannerChainID string `json:"planner_chain_id"`
}

type replanResponse struct {
	Steps    []*planstore.PlanStep `json:"steps"`
	Markdown string                `json:"markdown"`
}

type activeResponse struct {
	Plan  *planstore.Plan       `json:"plan"`
	Steps []*planstore.PlanStep `json:"steps"`
}

type markdownResponse struct {
	Markdown string `json:"markdown"`
}

type cleanResponse struct {
	Removed int `json:"removed"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// Creates a new plan from a free-text goal.
//
// The planner_chain_id must reference an existing TaskChainDefinition.
// The chain is called with the goal text; it must return a JSON array of step strings.
// The new plan becomes the active plan.
func (h *handler) newPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := apiframework.Decode[newPlanRequest](r) // @request planapi.newPlanRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	if req.Goal == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("%w: goal is required", apiframework.ErrBadRequest), apiframework.CreateOperation)
		return
	}
	chain, err := h.lookupChain(r, req.PlannerChainID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	plan, steps, md, err := h.svc.New(ctx, req.Goal, chain)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, newPlanResponse{Plan: plan, Steps: steps, Markdown: md}) // @response planapi.newPlanResponse
}

// Lists all plans.
func (h *handler) listPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plans, err := h.svc.List(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, plans) // @response []*planstore.Plan
}

// Returns the active plan and all its steps.
func (h *handler) getActive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plan, steps, err := h.svc.Active(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	if plan == nil {
		_ = apiframework.Error(w, r, fmt.Errorf("%w: no active plan", apiframework.ErrNotFound), apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, activeResponse{Plan: plan, Steps: steps}) // @response planapi.activeResponse
}

// Executes the next pending step of the active plan.
//
// executor_chain_id must reference a TaskChainDefinition that accepts a step
// description string and returns the execution result.
func (h *handler) nextStep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := apiframework.Decode[nextStepRequest](r) // @request planapi.nextStepRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	chain, err := h.lookupChain(r, req.ExecutorChainID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	result, md, err := h.svc.Next(ctx, planservice.Args{WithShell: req.WithShell, WithAuto: req.WithAuto}, chain)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, nextStepResponse{Result: result, Markdown: md}) // @response planapi.nextStepResponse
}

// Replaces remaining pending steps with a freshly generated plan.
//
// Completed steps are preserved; the planner is called with a recap of the
// original goal plus the completed steps to produce the new remaining steps.
func (h *handler) replan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := apiframework.Decode[replanRequest](r) // @request planapi.replanRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	chain, err := h.lookupChain(r, req.PlannerChainID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	steps, md, err := h.svc.Replan(ctx, chain)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, replanResponse{Steps: steps, Markdown: md}) // @response planapi.replanResponse
}

// Resets a step to pending so it will be retried on the next Next call.
func (h *handler) retryStep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawOrdinal := apiframework.GetPathParam(r, "ordinal", "The 1-based step ordinal.")
	ordinal, err := parseOrdinal(rawOrdinal)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	md, err := h.svc.Retry(ctx, ordinal)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, markdownResponse{Markdown: md}) // @response planapi.markdownResponse
}

// Marks a step as intentionally skipped.
func (h *handler) skipStep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawOrdinal := apiframework.GetPathParam(r, "ordinal", "The 1-based step ordinal.")
	ordinal, err := parseOrdinal(rawOrdinal)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	md, err := h.svc.Skip(ctx, ordinal)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, markdownResponse{Markdown: md}) // @response planapi.markdownResponse
}

// Switches the active plan to the named plan (archives the previous active).
func (h *handler) activate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := apiframework.GetPathParam(r, "name", "The plan name to activate.")
	if name == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("%w: name is required", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}
	if err := h.svc.SetActive(ctx, name); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, fmt.Sprintf("plan %q is now active", name)) // @response string
}

// Permanently deletes a plan by name.
func (h *handler) deletePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := apiframework.GetPathParam(r, "name", "The plan name to delete.")
	if name == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("%w: name is required", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}
	if err := h.svc.Delete(ctx, name); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, fmt.Sprintf("plan %q deleted", name)) // @response string
}

// Removes all completed or archived plans.
func (h *handler) cleanPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := h.svc.Clean(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, cleanResponse{Removed: n}) // @response planapi.cleanResponse
}

// ── private helpers ───────────────────────────────────────────────────────────

func parseOrdinal(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("%w: ordinal is required", apiframework.ErrBadPathValue)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: ordinal must be a positive integer", apiframework.ErrUnprocessableEntity)
	}
	return n, nil
}
