import {
  useMutation,
  UseMutationResult,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { api } from '../lib/api';
import { ApiError } from '../lib/fetch';
import { planKeys } from '../lib/queryKeys';
import type {
  ActivePlanResponse,
  CleanPlansResponse,
  NewPlanResponse,
  NextStepResponse,
  Plan,
  PlanMarkdownResponse,
  ReplanResponse,
} from '../lib/types';

export function usePlansList() {
  return useQuery<Plan[]>({
    queryKey: planKeys.list(),
    queryFn: () => api.listPlans(),
  });
}

/** Active plan + steps, or `null` when none (404 from GET /plans/active). */
export function useActivePlan() {
  return useQuery<ActivePlanResponse | null>({
    queryKey: planKeys.active(),
    queryFn: async () => {
      try {
        return await api.getActivePlan();
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          return null;
        }
        throw e;
      }
    },
  });
}

function invalidatePlans(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: planKeys.list() });
  qc.invalidateQueries({ queryKey: planKeys.active() });
}

export function useCreatePlan(): UseMutationResult<
  NewPlanResponse,
  Error,
  { goal: string; planner_chain_id: string },
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: body => api.createPlan(body),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function usePlanNext(): UseMutationResult<
  NextStepResponse,
  Error,
  { executor_chain_id: string; with_shell?: boolean; with_auto?: boolean },
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: body => api.planNext(body),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function usePlanReplan(): UseMutationResult<
  ReplanResponse,
  Error,
  { planner_chain_id: string },
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: body => api.planReplan(body),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function useRetryPlanStep(): UseMutationResult<
  PlanMarkdownResponse,
  Error,
  number,
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ordinal => api.retryPlanStep(ordinal),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function useSkipPlanStep(): UseMutationResult<
  PlanMarkdownResponse,
  Error,
  number,
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ordinal => api.skipPlanStep(ordinal),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function useActivatePlan(): UseMutationResult<string, Error, string, unknown> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: name => api.activatePlan(name),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function useDeletePlan(): UseMutationResult<string, Error, string, unknown> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: name => api.deletePlan(name),
    onSuccess: () => invalidatePlans(qc),
  });
}

export function useCleanPlans(): UseMutationResult<CleanPlansResponse, Error, void, unknown> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.cleanPlans(),
    onSuccess: () => invalidatePlans(qc),
  });
}
