import {
  Table,
  TableCell,
  TableRow,
  Collapsible,
  Badge,
  Span,
  cn,
} from '@contenox/ui';
import { Activity, AlertCircle, CheckCircle2, Settings } from 'lucide-react';
import { useMemo } from 'react';
import { t } from 'i18next';
import type { TaskEvent, CapturedStateUnit } from '../../../../lib/types';

export function ExecutionTimeline({
  events,
  state,
}: {
  events?: TaskEvent[];
  state?: CapturedStateUnit[];
}) {
  if ((!events || events.length === 0) && (!state || state.length === 0)) {
    return null;
  }

  return (
    <div className="mt-4 flex flex-col gap-2 pt-3 border-t border-border/40">
      {events && events.length > 0 && <LiveTaskEvents events={events} />}
      {state && state.length > 0 && (!events || events.length === 0) && (
        <HistoricalState state={state} />
      )}
    </div>
  );
}

function LiveTaskEvents({ events }: { events: TaskEvent[] }) {
  const steps = useMemo(() => {
    const groups: { id: string; events: TaskEvent[] }[] = [];
    let currentId: string | null = null;

    for (const e of events) {
      const stepId = e.task_id || e.task_handler || 'system';
      if (stepId !== currentId) {
        currentId = stepId;
        groups.push({ id: stepId, events: [] });
      }
      groups[groups.length - 1].events.push(e);
    }
    return groups;
  }, [events]);

  return (
    <div className="flex flex-col gap-2 text-sm">
      <div className="flex items-center gap-2 text-muted-foreground font-medium px-1">
        <Activity size={14} />
        <Span>{t('chat.execution_log', 'Execution Log')}</Span>
      </div>
      {steps.map((group, idx) => (
        <StepCollapsible key={`${group.id}-${idx}`} group={group} />
      ))}
    </div>
  );
}

function StepCollapsible({ group }: { group: { id: string; events: TaskEvent[] } }) {
  const events = group.events;

  const isError = events.some((e) => e.kind === 'step_failed' || e.kind === 'chain_failed');
  const isDone = events.some((e) => e.kind === 'step_completed' || e.kind === 'chain_completed');
  const transitionEvent = events.find((e) => !!e.transition);

  let title = group.id;
  if (title === 'system' && events.some(e => e.kind === 'chain_started')) {
    title = 'Initializing Plan';
  } else if (events.some(e => e.kind === 'approval_requested')) {
    title = 'Awaiting Approval';
  }

  const TitleElement = (
    <div className="flex items-center gap-2">
      <span className="flex-shrink-0">
        {isError ? (
          <AlertCircle size={14} className="text-error" />
        ) : isDone ? (
          <CheckCircle2 size={14} className="text-success" />
        ) : (
          <Settings size={14} className="text-muted-foreground animate-spin-slow" />
        )}
      </span>
      <Span className="font-mono text-xs font-semibold">{title}</Span>
      {transitionEvent && transitionEvent.transition && (
        <Badge variant="outline" size="sm" className="text-[10px] py-0 ml-2">
          {transitionEvent.transition}
        </Badge>
      )}
    </div>
  );

  return (
    <Collapsible title={TitleElement} className="bg-background">
      <div className="p-3 font-mono text-[11px] overflow-x-auto whitespace-pre bg-surface-50 dark:bg-dark-surface-50 rounded-b-md">
        {events.map((e, idx) => (
          <div key={idx} className="flex gap-2 mb-1">
            <Span className="text-muted-foreground opacity-50 shrink-0">
              {new Date(e.timestamp).toLocaleTimeString([], { hour12: false })}
            </Span>
            <Span className={cn(e.error ? 'text-error font-medium' : 'text-text dark:text-dark-text')}>
              {e.kind}
              {e.task_handler && e.task_handler !== group.id ? ` [${e.task_handler}]` : ''}
              {e.error ? ` - ${e.error}` : ''}
            </Span>
          </div>
        ))}
      </div>
    </Collapsible>
  );
}

function HistoricalState({ state }: { state: CapturedStateUnit[] }) {
  const formatDuration = (ms: number): string => {
    if (ms < 1000) return `${Math.round(ms)} ms`;
    return `${(ms / 1000).toFixed(2)} s`;
  };

  const TitleElement = (
    <div className="flex items-center gap-2">
      <Activity size={14} />
      <Span className="font-medium text-xs">
        {t('chat.show_state', 'Show State Logs')} ({state.length})
      </Span>
    </div>
  );

  return (
    <Collapsible title={TitleElement} className="mt-1">
      <div className="border border-border rounded-b-md overflow-x-auto">
        <Table
          columns={[
            t('chat.task_id', 'Task'),
            t('chat.task_type', 'Type'),
            t('chat.transition', 'Transition'),
            t('chat.duration', 'Duration'),
            t('chat.error', 'Error'),
          ]}>
          {state.map((unit, index) => (
            <TableRow key={index}>
              <TableCell className="font-mono text-xs">{unit.taskID}</TableCell>
              <TableCell className="text-xs">{unit.taskType}</TableCell>
              <TableCell className="max-w-xs truncate text-xs">{unit.transition || '-'}</TableCell>
              <TableCell className="text-xs">{formatDuration(unit.duration)}</TableCell>
              <TableCell className="text-xs">
                {unit.error?.error ? (
                  <Badge variant="error" size="sm">{unit.error.error}</Badge>
                ) : (
                  '-'
                )}
              </TableCell>
            </TableRow>
          ))}
        </Table>
      </div>
    </Collapsible>
  );
}
