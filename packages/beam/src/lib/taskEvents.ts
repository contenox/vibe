import { TaskEvent } from './types';

export type TaskEventViewState = {
  events: TaskEvent[];
  content: string;
  thinking: string;
  status: string;
  error: string | null;
  lastTaskID: string | null;
  /** From chain_started (VFS path or chain id). */
  activeChainId: string | null;
};

export function createEmptyTaskEventState(): TaskEventViewState {
  return {
    events: [],
    content: '',
    thinking: '',
    status: '',
    error: null,
    lastTaskID: null,
    activeChainId: null,
  };
}

export function reduceTaskEventState(
  state: TaskEventViewState,
  event: TaskEvent,
): TaskEventViewState {
  const next: TaskEventViewState = {
    ...state,
    events: [...state.events, event],
  };

  switch (event.kind) {
    case 'chain_started':
      if (event.chain_id) {
        next.activeChainId = event.chain_id;
      }
      next.status = event.chain_id ? `Running ${event.chain_id}` : 'Running';
      break;
    case 'step_started':
      next.lastTaskID = event.task_id ?? state.lastTaskID;
      next.status = formatStepStatus(event, 'Running');
      break;
    case 'step_chunk':
      next.lastTaskID = event.task_id ?? state.lastTaskID;
      next.status = formatStepStatus(event, 'Streaming');
      if (event.content) {
        next.content += event.content;
      }
      if (event.thinking) {
        next.thinking += event.thinking;
      }
      break;
    case 'step_completed':
      next.lastTaskID = event.task_id ?? state.lastTaskID;
      next.status = formatStepStatus(event, 'Completed');
      break;
    case 'step_failed':
      next.lastTaskID = event.task_id ?? state.lastTaskID;
      next.status = formatStepStatus(event, 'Failed');
      next.error = event.error ?? 'Task failed';
      break;
    case 'chain_completed':
      next.status = 'Completed';
      break;
    case 'chain_failed':
      next.status = 'Failed';
      next.error = event.error ?? 'Task failed';
      break;
  }

  return next;
}

function formatStepStatus(event: TaskEvent, fallback: string): string {
  const step = event.task_id || event.task_handler;
  if (!step) {
    return fallback;
  }
  return `${fallback}: ${step}`;
}

export function createTaskEventRequestId(): string {
  if (typeof globalThis !== 'undefined' && globalThis.crypto?.randomUUID) {
    return globalThis.crypto.randomUUID();
  }
  return `req-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

export function formatTaskOutput(output: unknown): string {
  if (typeof output === 'string') {
    return output;
  }
  if (output == null) {
    return '';
  }
  try {
    return JSON.stringify(output, null, 2);
  } catch {
    return String(output);
  }
}
