import type { Plan, PlanStep } from './types';

function stepMarker(status: PlanStep['status']): string {
  switch (status) {
    case 'completed':
      return 'x';
    case 'failed':
      return '!';
    case 'skipped':
      return '-';
    default:
      return ' ';
  }
}

/** Mirrors `planservice.renderMarkdown` for POST /api/plans/compile. */
export function renderPlanMarkdown(plan: Plan, steps: PlanStep[]): string {
  let sb = `# Plan: ${plan.name}\n\n`;
  sb += `**Goal:** ${plan.goal}\n\n`;
  sb += `**Status:** ${plan.status}\n\n`;
  sb += '## Steps\n\n';
  for (const st of steps) {
    sb += `- [${stepMarker(st.status)}] ${st.ordinal}. ${st.description}\n`;
    const result = st.execution_result?.trim();
    if (result) {
      for (const line of result.split('\n')) {
        sb += `  > ${line}\n`;
      }
    }
  }
  return sb;
}
