import { describe, expect, it } from 'vitest';
import { createEmptyTaskEventState, reduceTaskEventState } from './taskEvents';

describe('reduceTaskEventState', () => {
  it('accumulates chunk content and thinking', () => {
    let state = createEmptyTaskEventState();
    state = reduceTaskEventState(state, {
      kind: 'step_started',
      timestamp: new Date().toISOString(),
      task_id: 'step-1',
    });
    state = reduceTaskEventState(state, {
      kind: 'step_chunk',
      timestamp: new Date().toISOString(),
      task_id: 'step-1',
      content: 'Hello',
      thinking: 'Plan',
    });
    state = reduceTaskEventState(state, {
      kind: 'step_chunk',
      timestamp: new Date().toISOString(),
      task_id: 'step-1',
      content: ' world',
      thinking: ' more',
    });

    expect(state.content).toBe('Hello world');
    expect(state.thinking).toBe('Plan more');
    expect(state.status).toBe('Streaming: step-1');
  });

  it('captures terminal failure state', () => {
    const state = reduceTaskEventState(createEmptyTaskEventState(), {
      kind: 'chain_failed',
      timestamp: new Date().toISOString(),
      error: 'boom',
    });

    expect(state.status).toBe('Failed');
    expect(state.error).toBe('boom');
  });

  it('records active chain id from chain_started', () => {
    const state = reduceTaskEventState(createEmptyTaskEventState(), {
      kind: 'chain_started',
      timestamp: new Date().toISOString(),
      chain_id: 'default-chain.json',
    });
    expect(state.activeChainId).toBe('default-chain.json');
  });

  it('accumulates inline attachments from event.attachments (mapping artifact kinds)', () => {
    let state = createEmptyTaskEventState();
    state = reduceTaskEventState(state, {
      kind: 'step_completed',
      timestamp: new Date().toISOString(),
      task_id: 'tools',
      // Wire format uses artifact-kind vocabulary; reduceTaskEventState
      // maps it to inline-attachment kind via artifactsToInlineAttachments.
      attachments: [
        { kind: 'file_excerpt', payload: { path: 'README.md', text: '# hi\n' } },
      ],
    });
    expect(state.attachments).toHaveLength(1);
    expect(state.attachments[0]).toMatchObject({ kind: 'file_view', path: 'README.md' });

    state = reduceTaskEventState(state, {
      kind: 'step_completed',
      timestamp: new Date().toISOString(),
      task_id: 'tools-2',
      attachments: [{ kind: 'terminal_output', payload: { output: 'a\n' } }],
    });
    expect(state.attachments).toHaveLength(2);
    expect(state.attachments[1].kind).toBe('terminal_excerpt');
  });

  it('drops attachments with unknown kinds gracefully', () => {
    const state = reduceTaskEventState(createEmptyTaskEventState(), {
      kind: 'step_completed',
      timestamp: new Date().toISOString(),
      attachments: [{ kind: 'mystery_kind', payload: { x: 1 } }],
    });
    expect(state.attachments).toHaveLength(0);
  });
});
