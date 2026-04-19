import {
  Button,
  ButtonGroup,
  InlineNotice,
  InsetPanel,
  NumberInput,
  Select,
  Span,
  Spinner,
  Toolbar,
  ToolbarActions,
  ToolbarItem,
  ToolbarSection,
  Tooltip,
} from '@contenox/ui';
import { FolderOpen, Pencil } from 'lucide-react';
import { t } from 'i18next';
import { useEffect, useRef, useState } from 'react';
import type { ChainDefinition, ChatModeId } from '../../../../lib/types';

interface ChatToolbarProps {
  chainOptions: { value: string; label: string }[];
  selectedChainId: string;
  onChainChange: (id: string) => void;
  chainsLoading: boolean;
  executorChainPreview: ChainDefinition | null | undefined;
  onTokenLimitSave: (limit: number) => void;
  modeOptions: { value: ChatModeId; label: string }[];
  selectedMode: ChatModeId;
  onModeChange: (mode: ChatModeId) => void;
  isProcessing: boolean;
  policyNames: string[];
  activePolicyName: string;
  onPolicyChange: (name: string) => void;
  policyChangePending: boolean;
  policyChangeError: string | null;
  statsLabel: string;
  workspacePanelOpen: boolean;
  onWorkspaceToggle: () => void;
  onOpenMobileWorkspace: () => void;
  onEditChain: () => void;
  isLg: boolean;
}

export function ChatToolbar({
  chainOptions,
  selectedChainId,
  onChainChange,
  chainsLoading,
  executorChainPreview,
  onTokenLimitSave,
  modeOptions,
  selectedMode,
  onModeChange,
  isProcessing,
  policyNames,
  activePolicyName,
  onPolicyChange,
  policyChangePending,
  policyChangeError,
  statsLabel,
  workspacePanelOpen,
  onWorkspaceToggle,
  onOpenMobileWorkspace,
  onEditChain,
}: ChatToolbarProps) {
  const [editingTokenLimit, setEditingTokenLimit] = useState(false);
  const [tokenLimitDraft, setTokenLimitDraft] = useState('');
  const tokenLimitPopoverRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!editingTokenLimit) return;
    const handleMousedown = (e: MouseEvent) => {
      if (tokenLimitPopoverRef.current && !tokenLimitPopoverRef.current.contains(e.target as Node)) {
        setEditingTokenLimit(false);
      }
    };
    const handleKeydown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setEditingTokenLimit(false);
    };
    document.addEventListener('mousedown', handleMousedown);
    document.addEventListener('keydown', handleKeydown);
    return () => {
      document.removeEventListener('mousedown', handleMousedown);
      document.removeEventListener('keydown', handleKeydown);
    };
  }, [editingTokenLimit]);

  return (
    <Toolbar>
      <ToolbarSection>
        <ToolbarItem label={t('chat.task_chain')} tooltip={t('chat.chain_tooltip')}>
          <Select
            options={chainOptions}
            value={selectedChainId}
            onChange={e => onChainChange(e.target.value)}
            className="min-w-[10rem] max-w-full flex-1 sm:max-w-md"
            disabled={chainsLoading}
          />
          {chainsLoading && <Spinner size="sm" />}
          {executorChainPreview && (
            <div className="relative" ref={tokenLimitPopoverRef}>
              <Tooltip content={t('chat.token_limit_tooltip')} position="top">
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  palette="neutral"
                  onClick={() => {
                    setTokenLimitDraft(String(executorChainPreview.token_limit ?? 0));
                    setEditingTokenLimit(v => !v);
                  }}
                >
                  <Span>{executorChainPreview.token_limit ? `${Math.round(executorChainPreview.token_limit / 1000)}k` : '∞'}</Span>
                  <Pencil className="h-2.5 w-2.5" />
                </Button>
              </Tooltip>
              {editingTokenLimit && (
                <InsetPanel tone="default" className="absolute top-full left-0 z-50 mt-1 gap-2 p-3 shadow-md">
                  <Span className="text-xs font-medium">{t('chat.token_limit_label')}</Span>
                  <ButtonGroup>
                    <NumberInput
                      min={0}
                      value={Number(tokenLimitDraft)}
                      onChange={(v: number) => setTokenLimitDraft(String(v))}
                      className="w-28 px-2 py-1 text-xs"
                      autoFocus
                    />
                    <Button
                      size="xs"
                      type="button"
                      onClick={() => {
                        onTokenLimitSave(Number(tokenLimitDraft));
                        setEditingTokenLimit(false);
                      }}
                    >
                      {t('common.save', 'Save')}
                    </Button>
                    <Button
                      size="xs"
                      variant="ghost"
                      type="button"
                      onClick={() => setEditingTokenLimit(false)}
                    >
                      {t('common.cancel', 'Cancel')}
                    </Button>
                  </ButtonGroup>
                </InsetPanel>
              )}
            </div>
          )}
          <Tooltip
            content={
              selectedChainId
                ? t('chat.edit_chain', 'Open this chain in the editor')
                : t('chat.edit_chain_disabled', 'Select a chain to edit')
            }
            position="top"
          >
            <Button
              type="button"
              variant="ghost"
              size="xs"
              disabled={!selectedChainId.trim()}
              onClick={onEditChain}
            >
              <Pencil className="h-3.5 w-3.5" />
            </Button>
          </Tooltip>
        </ToolbarItem>
        <ToolbarItem label={t('chat.mode')} tooltip={t('chat.mode_tooltip')}>
          <Select
            options={modeOptions}
            value={selectedMode}
            onChange={e => onModeChange(e.target.value as ChatModeId)}
            className="min-w-[7rem] max-w-[12rem] shrink-0"
            disabled={isProcessing}
          />
        </ToolbarItem>
        {policyNames.length > 0 && (
          <ToolbarItem
            label={t('chat.hitl_policy', 'Policy')}
            tooltip={t('chat.hitl_policy_tooltip', 'HITL policy applied to this session')}
          >
            <Select
              options={[
                { value: '', label: t('chat.hitl_policy_none', 'None') },
                ...policyNames.map(n => ({ value: n, label: n })),
              ]}
              value={activePolicyName}
              onChange={e => onPolicyChange(e.target.value)}
              className="min-w-[8rem] max-w-[14rem] shrink-0"
              disabled={policyChangePending}
            />
            {policyChangeError && (
              <InlineNotice variant="error" className="text-xs">
                {policyChangeError}
              </InlineNotice>
            )}
          </ToolbarItem>
        )}
      </ToolbarSection>
      <ToolbarActions>
        <Span
          className="text-text-muted dark:text-dark-text-muted shrink-0 text-xs"
          title={statsLabel}>
          {statsLabel}
        </Span>
        <Tooltip content={t('chat.workspace_toggle_tooltip')}>
          <Button
            type="button"
            variant={workspacePanelOpen ? 'secondary' : 'outline'}
            size="sm"
            className="shrink-0"
            onClick={onWorkspaceToggle}
            aria-pressed={workspacePanelOpen}
            aria-label={t('chat.workspace_toggle_aria')}>
            <FolderOpen className="h-4 w-4" />
          </Button>
        </Tooltip>
        {workspacePanelOpen ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="lg:hidden"
            onClick={onOpenMobileWorkspace}>
            {t('chat.workspace_open_mobile')}
          </Button>
        ) : null}
      </ToolbarActions>
    </Toolbar>
  );
}
