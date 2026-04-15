import { describe, expect, it } from 'vitest';

import type { ChainDefinition, ChainTask } from '../types';
import { diagnosticCounts, diagnosticsByTask, lintChain } from './index';

function chatTask(overrides: Partial<ChainTask> = {}): ChainTask {
  return {
    id: overrides.id ?? 'chat',
    description: overrides.description ?? '',
    handler: 'chat_completion',
    prompt_template: '',
    transition: { branches: [] },
    execute_config: {
      model: '{{var:model}}',
      provider: '{{var:provider}}',
      retry_policy: { max_attempts: 3 },
      compact_policy: { trigger_fraction: 0.85 },
      hooks: ['*', '!plan_manager'],
      hook_policies: {
        local_shell: { _allowed_commands: 'ls,cat' },
        local_fs: { _allowed_dir: '.' },
      },
    },
    ...overrides,
  };
}

function chain(...tasks: ChainTask[]): ChainDefinition {
  return { id: 'test', description: '', tasks };
}

describe('lintChain', () => {
  it('returns no diagnostics for a fully-configured chain', () => {
    const result = lintChain(chain(chatTask()));
    expect(result).toEqual([]);
  });

  it('warns when local_shell is exposed without allowlist', () => {
    const t = chatTask();
    t.execute_config!.hook_policies = {
      ...t.execute_config!.hook_policies,
      local_shell: {},
    };
    const result = lintChain(chain(t));
    expect(result).toHaveLength(1);
    expect(result[0]).toMatchObject({
      ruleId: 'hook_policies_missing_for_local_shell',
      severity: 'warning',
      taskId: 'chat',
    });
  });

  it('warns when wildcard hooks expose local_shell without allowlist', () => {
    const t = chatTask({ id: 'wild' });
    t.execute_config!.hooks = ['*'];
    t.execute_config!.hook_policies = {}; // wipe both policies
    const result = lintChain(chain(t));
    const ruleIds = result.map((d) => d.ruleId);
    expect(ruleIds).toContain('hook_policies_missing_for_local_shell');
    expect(ruleIds).toContain('hook_policies_missing_for_local_fs');
  });

  it('does NOT warn when local_shell is excluded via negation', () => {
    const t = chatTask({ id: 'no_shell' });
    t.execute_config!.hooks = ['*', '!local_shell'];
    delete t.execute_config!.hook_policies!.local_shell;
    const result = lintChain(chain(t));
    expect(
      result.find((d) => d.ruleId === 'hook_policies_missing_for_local_shell'),
    ).toBeUndefined();
  });

  it('does NOT warn when hooks list is empty (no exposure)', () => {
    const t = chatTask({ id: 'empty' });
    t.execute_config!.hooks = [];
    t.execute_config!.hook_policies = {};
    const result = lintChain(chain(t));
    const ruleIds = result.map((d) => d.ruleId);
    expect(ruleIds).not.toContain('hook_policies_missing_for_local_shell');
    expect(ruleIds).not.toContain('hook_policies_missing_for_local_fs');
  });

  it('treats absent hooks as exposing every hook (matches Go default)', () => {
    const t = chatTask({ id: 'absent' });
    delete t.execute_config!.hooks;
    t.execute_config!.hook_policies = {};
    const result = lintChain(chain(t));
    expect(
      result.find((d) => d.ruleId === 'hook_policies_missing_for_local_shell'),
    ).toBeDefined();
  });

  it('infos chat_completion missing retry_policy', () => {
    const t = chatTask({ id: 'noretry' });
    delete t.execute_config!.retry_policy;
    const result = lintChain(chain(t));
    const r = result.find((d) => d.ruleId === 'chat_completion_no_retry_policy');
    expect(r).toBeDefined();
    expect(r!.severity).toBe('info');
  });

  it('infos chat_completion missing compact_policy AND shift', () => {
    const t = chatTask({ id: 'nocompact' });
    delete t.execute_config!.compact_policy;
    delete t.execute_config!.shift;
    const result = lintChain(chain(t));
    expect(
      result.find((d) => d.ruleId === 'chat_completion_no_compact_policy'),
    ).toBeDefined();
  });

  it('does NOT info compact when shift:true is set as a fallback', () => {
    const t = chatTask({ id: 'shifty' });
    delete t.execute_config!.compact_policy;
    t.execute_config!.shift = true;
    const result = lintChain(chain(t));
    expect(
      result.find((d) => d.ruleId === 'chat_completion_no_compact_policy'),
    ).toBeUndefined();
  });

  it('skips non-chat handlers for retry / compact rules', () => {
    const t: ChainTask = {
      id: 'tools',
      description: '',
      handler: 'execute_tool_calls',
      prompt_template: '',
      transition: { branches: [] },
    };
    const result = lintChain(chain(t));
    const ruleIds = result.map((d) => d.ruleId);
    expect(ruleIds).not.toContain('chat_completion_no_retry_policy');
    expect(ruleIds).not.toContain('chat_completion_no_compact_policy');
  });

  it('returns [] for null / empty chain', () => {
    expect(lintChain(null)).toEqual([]);
    expect(lintChain({ id: 'x', description: '', tasks: [] })).toEqual([]);
  });
});

describe('diagnosticsByTask', () => {
  it('groups by taskId', () => {
    const a = chatTask({ id: 'a' });
    delete a.execute_config!.retry_policy;
    const b = chatTask({ id: 'b' });
    b.execute_config!.hook_policies = { local_shell: {} };
    const grouped = diagnosticsByTask(lintChain(chain(a, b)));
    expect(grouped.get('a')?.[0].ruleId).toBe('chat_completion_no_retry_policy');
    expect(grouped.get('b')?.[0].ruleId).toBe('hook_policies_missing_for_local_shell');
  });
});

describe('diagnosticCounts', () => {
  it('separates warnings from infos', () => {
    const t = chatTask();
    delete t.execute_config!.retry_policy;
    // Wipe local_shell only; keep local_fs valid so we get exactly 1 warning.
    t.execute_config!.hook_policies = {
      local_shell: {},
      local_fs: { _allowed_dir: '.' },
    };
    const counts = diagnosticCounts(lintChain(chain(t)));
    expect(counts.warnings).toBe(1);
    expect(counts.infos).toBe(1);
  });
});
