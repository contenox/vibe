import { artifactsToInlineAttachments } from './inlineAttachments';
import type { ChatContextArtifact, InlineAttachment, TaskEvent } from './types';

export type PendingApproval = {
  approvalId: string;
  hookName: string;
  toolName: string;
  args: Record<string, unknown>;
  diff: string;
};

export type TaskEventViewState = {
  events: TaskEvent[];
  content: string;
  thinking: string;
  status: string;
  error: string | null;
  lastTaskID: string | null;
  /** From chain_started (VFS path or chain id). */
  activeChainId: string | null;
  /**
   * Inline attachments accumulated from `step_completed` / `step_failed`
   * events for this run (Phase 5 of the canvas-vision plan). Mapped from
   * raw widget hints via the same artifact→inline-attachment helper used
   * by user-attached artifacts in Phase 4.
   */
  attachments: InlineAttachment[];
  /** Non-null when execution is paused waiting for HITL approval. */
  pendingApproval: PendingApproval | null;
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
    attachments: [],
    pendingApproval: null,
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

  // Accumulate inline attachments from any event that carries them. Hooks
  // emit hints during a step; the publisher attaches them to that step's
  // step_completed (or step_failed when a hook fired before terminal error).
  if (event.attachments && event.attachments.length > 0) {
    const inline = artifactsToInlineAttachments(
      event.attachments as ChatContextArtifact[],
    );
    if (inline.length > 0) {
      next.attachments = [...state.attachments, ...inline];
    }
  }

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
      next.pendingApproval = null;
      break;
    case 'step_failed':
      next.lastTaskID = event.task_id ?? state.lastTaskID;
      next.status = formatStepStatus(event, 'Failed');
      next.error = event.error ?? 'Task failed';
      next.pendingApproval = null;
      break;
    case 'chain_completed':
      next.status = 'Completed';
      next.pendingApproval = null;
      break;
    case 'chain_failed':
      next.status = 'Failed';
      next.error = event.error ?? 'Task failed';
      next.pendingApproval = null;
      break;
    case 'approval_requested':
      next.status = 'Awaiting approval';
      next.pendingApproval = {
        approvalId: event.approval_id ?? '',
        hookName: event.hook_name ?? '',
        toolName: event.tool_name ?? '',
        args: event.approval_args ?? {},
        diff: event.approval_diff ?? '',
      };
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
