import { MonoLogList, MonoLogListItem, Span } from '@contenox/ui';
import { t } from 'i18next';
import { TaskEvent } from '../../../../lib/types';

const TAIL = 40;

function formatKind(kind: TaskEvent['kind']): string {
  return kind;
}

/**
 * Read-only tail of the task-event stream for the current request (same payload as GET /api/task-events).
 */
export function TaskEventFeed({ events }: { events: TaskEvent[] }) {
  if (!events.length) return null;
  const tail = events.length > TAIL ? events.slice(-TAIL) : events;
  const omitted = events.length - tail.length;

  return (
    <div className="flex flex-col gap-1">
      {omitted > 0 ? (
        <Span variant="muted" className="text-[10px]">
          {t('chat.task_events_omitted', { count: omitted })}
        </Span>
      ) : null}
      <MonoLogList>
        {tail.map((e, i) => (
          <MonoLogListItem key={`${e.timestamp}-${e.kind}-${i}`}>
            <span className="text-secondary-600 dark:text-dark-secondary-400">{formatKind(e.kind)}</span>
            {e.task_id ? (
              <span className="text-text-muted dark:text-dark-text-muted ml-1 truncate">
                {e.task_id}
              </span>
            ) : null}
            {e.error ? (
              <span className="text-error-600 dark:text-dark-error-100 ml-1 block">{e.error}</span>
            ) : null}
          </MonoLogListItem>
        ))}
      </MonoLogList>
    </div>
  );
}
