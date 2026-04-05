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
});
