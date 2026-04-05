import { useEffect, useState } from 'react';
import { api } from '../lib/api';
import { createEmptyTaskEventState, reduceTaskEventState, TaskEventViewState } from '../lib/taskEvents';
import { TaskEvent } from '../lib/types';

const MAX_RETRIES = 5;

export function useTaskEvents(requestId: string | null, options?: { enabled?: boolean }) {
  const [state, setState] = useState<TaskEventViewState>(createEmptyTaskEventState);

  useEffect(() => {
    if (!requestId) {
      setState(createEmptyTaskEventState());
      return;
    }
    if (options?.enabled === false) {
      return;
    }

    let retryCount = 0;
    let timeoutId: ReturnType<typeof setTimeout>;
    let currentSource: EventSource;
    let terminated = false;

    const connect = () => {
      currentSource = api.taskEvents(requestId);

      currentSource.onmessage = event => {
        retryCount = 0;
        try {
          const parsed = JSON.parse(event.data) as TaskEvent;
          setState(prev => reduceTaskEventState(prev, parsed));
          if (parsed.kind === 'chain_completed' || parsed.kind === 'chain_failed') {
            terminated = true;
            currentSource.close();
          }
        } catch (error) {
          console.error('Failed to parse task event:', error);
        }
      };

      currentSource.onerror = () => {
        currentSource.close();
        if (!terminated && retryCount < MAX_RETRIES) {
          const delay = Math.min(1000 * 2 ** retryCount, 30000);
          retryCount++;
          timeoutId = setTimeout(connect, delay);
        }
      };
    };

    setState(createEmptyTaskEventState());
    connect();

    return () => {
      terminated = true;
      clearTimeout(timeoutId);
      currentSource?.close();
    };
  }, [requestId, options?.enabled]);

  return state;
}
