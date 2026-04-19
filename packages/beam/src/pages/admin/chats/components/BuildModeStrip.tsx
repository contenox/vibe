import { Button, InsetPanel, Span } from '@contenox/ui';
import { t } from 'i18next';
import type { ChainDefinition } from '../../../../lib/types';

interface BuildModeStripProps {
  selectedChainId: string;
  executorChainPreview: ChainDefinition | null | undefined;
  isProcessing: boolean;
  onRun: () => void;
}

export function BuildModeStrip({
  selectedChainId,
  executorChainPreview,
  isProcessing,
  onRun,
}: BuildModeStripProps) {
  return (
    <InsetPanel
      tone="strip"
      role="region"
      aria-label={t('chat.build_graph_aria_panel')}>
      <div className="flex shrink-0 items-center justify-between gap-2 px-3 py-2">
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <Span variant="body" className="text-sm font-medium">
            {t('chat.build_workflow_title')}
          </Span>
          {selectedChainId.trim() && executorChainPreview && (
            <Span variant="muted" className="truncate text-xs">
              {selectedChainId.split('/').pop()} · {executorChainPreview.tasks.length} {t('chat.build_steps', 'steps')}
            </Span>
          )}
          {selectedChainId.trim() && !executorChainPreview && (
            <Span variant="muted" className="text-xs">
              {selectedChainId.split('/').pop()}
            </Span>
          )}
          {!selectedChainId.trim() && (
            <Span variant="muted" className="text-xs">
              {t('chat.build_no_chain', 'Select a chain above')}
            </Span>
          )}
        </div>
        <Button
          type="button"
          variant="primary"
          size="sm"
          disabled={isProcessing || !selectedChainId.trim()}
          onClick={onRun}>
          {t('chat.build_run_button')}
        </Button>
      </div>
    </InsetPanel>
  );
}
