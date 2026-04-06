import {
  useMutation,
  UseMutationResult,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { useMemo } from 'react';
import { api } from '../lib/api';
import { ApiError } from '../lib/fetch';
import { planKeys } from '../lib/queryKeys';
import { renderPlanMarkdown } from '../lib/renderPlanMarkdown';
import type {
  ActivePlanResponse,
  CleanPlansResponse,
  CompilePlanResponse,
  NewPlanResponse,
  NextStepResponse,
  Plan,
  PlanMarkdownResponse,
  PlanStep,
  ReplanResponse,
  RunCompiledActiveResponse,
} from '../lib/types';

export function usePlansList() {
  return useQuery<Plan[]>({
    queryKey: planKeys.list(),
    queryFn: () => api.listPlans(),
  });
}

/** Active plan + steps, or `null` when none (404 from GET /plans/active). */
export function useActivePlan(options?: { enabled?: boolean }) {
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
    enabled: options?.enabled ?? true,
  });
}

/** Live preview via POST /api/plans/compile when the active plan has no cached `compiled_chain_json` yet (e.g. plan mode UI). */
export function useCompilePlanPreview(options: {
  enabled: boolean;
  plan: Plan | undefined;
  steps: PlanStep[] | undefined;
  executorChainId: string;
}) {
  const { enabled, plan, steps, executorChainId } = options;
  const stepsDigest = useMemo(() => {
    if (!steps?.length) return '';
    return JSON.stringify(
      steps.map(s => ({
        ordinal: s.ordinal,
        description: s.description,
        status: s.status,
        execution_result: s.execution_result,
      })),
    );
  }, [steps]);

  const canRun =
    enabled &&
    !!plan &&
    !!steps?.length &&
    executorChainId.trim().length > 0;

  return useQuery({
    queryKey: planKeys.compilePreview(plan?.id ?? '_', executorChainId, stepsDigest),
    queryFn: () => {
      if (!plan || !steps?.length) throw new Error('missing plan/steps');
      return api.compilePlan({
        markdown: renderPlanMarkdown(plan, steps),
        executor_chain_id: executorChainId,
        chain_id: `${plan.id}-beam-compile-preview`,
      });
    },
    enabled: canRun,
    staleTime: 60_000,
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

export function useCompilePlan(): UseMutationResult<
  CompilePlanResponse,
  Error,
  { markdown: string; executor_chain_id: string; chain_id: string; write_path?: string },
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: body => api.compilePlan(body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: planKeys.all });
    },
  });
}

export function useRunCompiledActivePlan(): UseMutationResult<
  RunCompiledActiveResponse,
  Error,
  { executor_chain_id: string; chain_id: string; write_path?: string },
  unknown
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: body => api.runCompiledActivePlan(body),
    onSuccess: () => invalidatePlans(qc),
  });
}
