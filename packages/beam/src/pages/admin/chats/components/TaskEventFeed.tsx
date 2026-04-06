import { Span } from '@contenox/ui';
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
      <ul className="max-h-48 space-y-1 overflow-y-auto border border-dashed border-surface-300 px-2 py-1.5 font-mono text-[10px] dark:border-dark-surface-600">
        {tail.map((e, i) => (
          <li
            key={`${e.timestamp}-${e.kind}-${i}`}
            className="text-text dark:text-dark-text border-surface-200 border-b border-dotted pb-0.5 last:border-0 dark:border-dark-surface-600">
            <span className="text-secondary-600 dark:text-dark-secondary-400">{formatKind(e.kind)}</span>
            {e.task_id ? (
              <span className="text-text-muted dark:text-dark-text-muted ml-1 truncate">
                {e.task_id}
              </span>
            ) : null}
            {e.error ? (
              <span className="text-error-600 dark:text-dark-error-100 ml-1 block">{e.error}</span>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}
