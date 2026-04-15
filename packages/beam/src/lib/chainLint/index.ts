/**
 * Pure-function lint pass over a [ChainDefinition]. Designed to be reusable:
 * the result is plain data, so the same engine can power UI badges (today),
 * a server-side validator (later), or a CLI command without UI coupling.
 *
 * Severity tiers:
 *   - 'warning' — an issue likely to bite at runtime (e.g. local_shell allowed
 *     in hooks but no allowlist policy → the chain-explore failure mode).
 *   - 'info'    — a soft nudge; chain still works, but adopting the suggestion
 *     improves robustness or clarity.
 *
 * No 'error' tier by design — the lint never blocks a save (the founder
 * picked "soft warnings only" for validation strictness).
 */

import type { ChainDefinition, ChainTask } from '../types';
import { DEFAULT_RULES, type LintRule } from './rules';

export type DiagnosticSeverity = 'warning' | 'info';

export interface Diagnostic {
  /** Stable id, e.g. `hook_policies_missing_for_local_shell`. */
  ruleId: string;
  severity: DiagnosticSeverity;
  /** Task id the diagnostic applies to. Empty string for chain-level findings. */
  taskId: string;
  /** Short, human-readable. Surfaces verbatim in tooltips. */
  message: string;
}

export type { LintRule } from './rules';

/**
 * Run every registered rule against the chain. Order is rule-registration
 * order followed by task order — stable so the UI can render the list
 * deterministically without sorting.
 */
export function lintChain(
  chain: ChainDefinition | null | undefined,
  rules: LintRule[] = DEFAULT_RULES,
): Diagnostic[] {
  if (!chain || !chain.tasks) return [];
  const out: Diagnostic[] = [];
  for (const rule of rules) {
    for (const task of chain.tasks) {
      const found = rule.check(task, chain);
      if (found) {
        out.push({
          ruleId: rule.id,
          severity: rule.severity,
          taskId: task.id,
          message: found,
        });
      }
    }
  }
  return out;
}

/**
 * Group diagnostics by `taskId` for convenient per-node rendering.
 * Empty taskId entries land under the `''` key (chain-level diagnostics).
 */
export function diagnosticsByTask(diagnostics: Diagnostic[]): Map<string, Diagnostic[]> {
  const m = new Map<string, Diagnostic[]>();
  for (const d of diagnostics) {
    const list = m.get(d.taskId) ?? [];
    list.push(d);
    m.set(d.taskId, list);
  }
  return m;
}

/** Counts by severity — for the summary panel above the DAG. */
export function diagnosticCounts(diagnostics: Diagnostic[]): {
  warnings: number;
  infos: number;
} {
  let warnings = 0;
  let infos = 0;
  for (const d of diagnostics) {
    if (d.severity === 'warning') warnings++;
    else infos++;
  }
  return { warnings, infos };
}

/** Re-export ChainTask so consumers can write rules without two imports. */
export type { ChainTask };
