import { useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { createEmptyTaskEventState, reduceTaskEventState, TaskEventViewState } from '../lib/taskEvents';
import { TaskEvent } from '../lib/types';

const MAX_RETRIES = 8;

export type TaskEventConnectionState = 'idle' | 'connecting' | 'open' | 'error' | 'closed';

export type UseTaskEventsOptions = {
  enabled?: boolean;
  /** Fires when the EventSource has connected (subscribe before POST to avoid missing early events). */
  onConnectionOpen?: () => void;
};

export type UseTaskEventsResult = {
  state: TaskEventViewState;
  connectionState: TaskEventConnectionState;
  /** Last transport or parse error (not task chain failure — see state.error). */
  connectionError: string | null;
};

/**
 * Subscribes to GET /api/task-events for a request-scoped task run.
 * Resets when requestId or enabled changes; closes when chain_completed / chain_failed arrives.
 */
export function useTaskEvents(
  requestId: string | null,
  options?: UseTaskEventsOptions,
): UseTaskEventsResult {
  const [state, setState] = useState<TaskEventViewState>(createEmptyTaskEventState);
  const [connectionState, setConnectionState] = useState<TaskEventConnectionState>('idle');
  const [connectionError, setConnectionError] = useState<string | null>(null);

  const onOpenRef = useRef(options?.onConnectionOpen);
  onOpenRef.current = options?.onConnectionOpen;

  useEffect(() => {
    if (!requestId) {
      setState(createEmptyTaskEventState());
      setConnectionState('idle');
      setConnectionError(null);
      return;
    }
    if (options?.enabled === false) {
      setState(createEmptyTaskEventState());
      setConnectionState('idle');
      setConnectionError(null);
      return;
    }

    let terminated = false;
    let retryCount = 0;
    let timeoutId: ReturnType<typeof setTimeout>;
    let currentSource: EventSource | undefined;

    setState(createEmptyTaskEventState());
    setConnectionError(null);
    setConnectionState('connecting');

    const connect = () => {
      if (terminated) return;
      currentSource = api.taskEvents(requestId);

      currentSource.onopen = () => {
        if (terminated) return;
        retryCount = 0;
        setConnectionState('open');
        setConnectionError(null);
        try {
          onOpenRef.current?.();
        } catch (e) {
          console.error('task events onConnectionOpen:', e);
        }
      };

      currentSource.onmessage = event => {
        if (terminated) return;
        retryCount = 0;
        try {
          const parsed = JSON.parse(event.data) as TaskEvent;
          setState(prev => reduceTaskEventState(prev, parsed));
          if (parsed.kind === 'chain_completed' || parsed.kind === 'chain_failed') {
            terminated = true;
            setConnectionState('closed');
            currentSource?.close();
          }
        } catch (error) {
          const msg = error instanceof Error ? error.message : String(error);
          setConnectionError(msg);
          console.error('Failed to parse task event:', error);
        }
      };

      currentSource.onerror = () => {
        if (terminated) return;
        currentSource?.close();
        if (retryCount < MAX_RETRIES) {
          const delay = Math.min(1000 * 2 ** retryCount, 30000);
          retryCount++;
          timeoutId = setTimeout(connect, delay);
          return;
        }
        setConnectionState('error');
        setConnectionError('Task event stream unavailable');
      };
    };

    connect();

    return () => {
      terminated = true;
      clearTimeout(timeoutId);
      currentSource?.close();
    };
  }, [requestId, options?.enabled]);

  return { state, connectionState, connectionError };
}
