import {
  Badge,
  Button,
  EmptyState,
  SidePanelBody,
  SidePanelColumn,
  SidePanelHeader,
  SidePanelRailButton,
  Span,
  Tooltip,
} from '@contenox/ui';
import { PanelRightClose, PanelRightOpen } from 'lucide-react';
import { t } from 'i18next';
import type { CapturedStateUnit, TaskEvent } from '../../../../lib/types';
import { StateVisualizer } from './StateVisualizer';
import { TaskEventFeed } from './TaskEventFeed';

interface ChatRunLogProps {
  open: boolean;
  onToggle: () => void;
  isProcessing: boolean;
  events: TaskEvent[];
  state: CapturedStateUnit[];
}

export function ChatRunLog({ open, onToggle, isProcessing, events, state }: ChatRunLogProps) {
  const count = isProcessing ? events.length : state.length;
  const hasData = count > 0;

  if (!open) {
    return (
      <Tooltip content={t('chat.show_run_log')} position="left">
        <SidePanelRailButton onClick={onToggle} aria-label={t('chat.show_run_log')}>
          <PanelRightOpen className="h-4 w-4" />
          {hasData ? (
            <Badge variant="success" size="sm" className="mt-1">
              {count}
            </Badge>
          ) : null}
        </SidePanelRailButton>
      </Tooltip>
    );
  }

  return (
    <SidePanelColumn>
      <SidePanelHeader>
        <div className="flex min-w-0 items-center gap-2">
          <Span variant="body" className="text-text dark:text-dark-text truncate font-medium">
            {t('chat.run_log')}
          </Span>
          <Badge variant={hasData ? 'success' : 'secondary'} size="sm">
            {count}
          </Badge>
        </div>
        <Tooltip content={t('chat.hide_run_log')} position="left">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="shrink-0"
            onClick={onToggle}
            aria-label={t('chat.hide_run_log')}>
            <PanelRightClose className="h-4 w-4" />
          </Button>
        </Tooltip>
      </SidePanelHeader>
      <SidePanelBody>
        {events.length > 0 ? (
          <div className="min-h-0 flex-1 overflow-auto">
            <Span variant="muted" className="mb-1 block text-xs font-medium">
              {t('chat.task_events_feed_title')}
            </Span>
            <TaskEventFeed events={events} />
          </div>
        ) : null}
        {state.length > 0 ? (
          <div className="min-h-0 flex-1 overflow-auto">
            <Span variant="muted" className="mb-1 block text-xs font-medium">
              {t('chat.captured_state_title')}
            </Span>
            <StateVisualizer state={state} />
          </div>
        ) : events.length === 0 ? (
          <EmptyState
            title={t('chat.no_state_data')}
            description={t('chat.state_will_appear_here')}
            icon="📊"
            orientation="vertical"
            iconSize="md"
            className="text-text dark:text-dark-text-muted h-full"
          />
        ) : null}
      </SidePanelBody>
    </SidePanelColumn>
  );
}
