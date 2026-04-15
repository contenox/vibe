import type { ChainDefinition, ChainTask } from '../types';
import type { DiagnosticSeverity } from './index';

/**
 * A single lint rule. `check` returns null when the rule does not fire for
 * `task`, or a short human-readable diagnostic message when it does. Pure
 * function — must not read any external state, so the same engine works in
 * the browser, Node, or a future server-side validator.
 */
export interface LintRule {
  id: string;
  severity: DiagnosticSeverity;
  check: (task: ChainTask, chain: ChainDefinition) => string | null;
}

/**
 * Returns true when the task's `execute_config.hooks` allowlist effectively
 * exposes the named hook. Honours wildcard (`"*"`) and the negation form
 * (`"!hook_name"`). When `hooks` is undefined or null the backend exposes
 * every registered hook by default — same as `["*"]` — so we treat that as
 * exposing the hook too.
 */
function exposesHook(task: ChainTask, hookName: string): boolean {
  const hooks = task.execute_config?.hooks;
  if (hooks == null) return true; // absent ⇒ all hooks (matches Go LLMExecutionConfig docs)
  // Negation always wins.
  if (hooks.includes(`!${hookName}`)) return false;
  if (hooks.includes(hookName)) return true;
  if (hooks.includes('*')) return true;
  return false;
}

function getPolicy(task: ChainTask, hookName: string): Record<string, string> | undefined {
  return task.execute_config?.hook_policies?.[hookName];
}

/**
 * Only `chat_completion` tasks make LLM calls; rules about retry / compaction
 * apply only to them. `prompt_to_*` handlers also call the LLM but go through
 * a different code path and have their own (less-frequent) failure modes; we
 * skip them in the initial rule set to keep the lint focused.
 */
function isChatCompletion(task: ChainTask): boolean {
  return task.handler === 'chat_completion';
}

export const ruleHookPoliciesMissingForLocalShell: LintRule = {
  id: 'hook_policies_missing_for_local_shell',
  severity: 'warning',
  check: (task) => {
    if (!exposesHook(task, 'local_shell')) return null;
    const p = getPolicy(task, 'local_shell');
    const allowed = p?.['_allowed_commands']?.trim();
    if (allowed) return null;
    return 'Task exposes local_shell but has no hook_policies.local_shell._allowed_commands. local_shell denies every call without an allowlist.';
  },
};

export const ruleHookPoliciesMissingForLocalFs: LintRule = {
  id: 'hook_policies_missing_for_local_fs',
  severity: 'warning',
  check: (task) => {
    if (!exposesHook(task, 'local_fs')) return null;
    const p = getPolicy(task, 'local_fs');
    const allowed = p?.['_allowed_dir']?.trim();
    if (allowed) return null;
    return 'Task exposes local_fs but has no hook_policies.local_fs._allowed_dir. Read paths default to deny without an allowed root.';
  },
};

export const ruleChatCompletionNoRetryPolicy: LintRule = {
  id: 'chat_completion_no_retry_policy',
  severity: 'info',
  check: (task) => {
    if (!isChatCompletion(task)) return null;
    if (task.execute_config?.retry_policy) return null;
    return 'chat_completion task has no retry_policy. Transient provider errors (429 / 5xx) will surface immediately and fail the run.';
  },
};

export const ruleChatCompletionNoCompactPolicy: LintRule = {
  id: 'chat_completion_no_compact_policy',
  severity: 'info',
  check: (task) => {
    if (!isChatCompletion(task)) return null;
    if (task.execute_config?.compact_policy) return null;
    if (!task.execute_config?.shift) {
      // Without shift, an overflowing chat will hard-fail. Surface as info.
      return 'chat_completion task has neither compact_policy nor shift:true. Long sessions / large tool outputs will overflow context with no recovery.';
    }
    return null;
  },
};

export const DEFAULT_RULES: LintRule[] = [
  ruleHookPoliciesMissingForLocalShell,
  ruleHookPoliciesMissingForLocalFs,
  ruleChatCompletionNoRetryPolicy,
  ruleChatCompletionNoCompactPolicy,
];
