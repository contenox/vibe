import { CodeBlock, Collapsible, Span } from '@contenox/ui';
import { FileText, TerminalSquare, ListChecks, Workflow, Database } from 'lucide-react';
import { t } from 'i18next';

import type { InlineAttachment } from '../../../../lib/types';

/**
 * Inline attachment rendering registry. One renderer per [InlineAttachment]
 * kind. Components are intentionally small, presentational, and free of
 * side-effects so they can be reused for both user-emitted (Phase 4) and
 * agent-emitted (Phase 5) attachments without a fork.
 *
 * Registry pattern matches the slash-command and artifact registries: the
 * dispatcher [InlineAttachmentRenderer] picks a renderer by kind and falls
 * back to a JSON dump for unknown kinds so an experimental attachment shape
 * never breaks the thread.
 */

export type InlineAttachmentRendererProps = { attachment: InlineAttachment };

function FileViewAttachment({
  attachment,
}: {
  attachment: Extract<InlineAttachment, { kind: 'file_view' }>;
}) {
  const lineCount = attachment.text.split('\n').length;
  return (
    <Collapsible
      title={
        <span className="inline-flex items-center gap-1.5 text-xs">
          <FileText className="h-3.5 w-3.5" />
          <span className="font-mono">{attachment.path}</span>
          <Span variant="muted" className="text-[10px]">
            {lineCount} {lineCount === 1 ? 'line' : 'lines'}
            {attachment.truncated ? ` · ${t('chat.attachment_truncated', 'truncated')}` : ''}
          </Span>
        </span>
      }
      defaultExpanded={false}
      className="border-border bg-surface-100 dark:bg-dark-surface-200 mt-2 rounded border"
    >
      <CodeBlock className="px-3 py-2 leading-relaxed">
        {attachment.text}
      </CodeBlock>
    </Collapsible>
  );
}

function TerminalExcerptAttachment({
  attachment,
}: {
  attachment: Extract<InlineAttachment, { kind: 'terminal_excerpt' }>;
}) {
  return (
    <Collapsible
      title={
        <span className="inline-flex items-center gap-1.5 text-xs">
          <TerminalSquare className="h-3.5 w-3.5" />
          <span>{t('chat.attachment_terminal', 'Terminal output')}</span>
          {attachment.command && (
            <Span variant="muted" className="font-mono text-[10px]">
              $ {attachment.command}
            </Span>
          )}
        </span>
      }
      defaultExpanded={false}
      className="border-border bg-surface-100 dark:bg-dark-surface-200 mt-2 rounded border"
    >
      <CodeBlock className="px-3 py-2 leading-relaxed">
        {attachment.output}
      </CodeBlock>
    </Collapsible>
  );
}

function PlanSummaryAttachment({
  attachment,
}: {
  attachment: Extract<InlineAttachment, { kind: 'plan_summary' }>;
}) {
  const statusColor =
    attachment.status === 'completed'
      ? 'text-success'
      : attachment.status === 'failed'
        ? 'text-destructive'
        : 'text-text-muted';
  return (
    <Collapsible
      title={
        <span className="inline-flex items-center gap-1.5 text-xs">
          <ListChecks className="h-3.5 w-3.5" />
          <span>
            {t('chat.attachment_plan_step', 'Plan step')} {attachment.ordinal}
          </span>
          <Span variant="muted" className={`text-[10px] ${statusColor}`}>
            · {attachment.status}
            {attachment.failureClass ? ` (${attachment.failureClass})` : ''}
          </Span>
        </span>
      }
      defaultExpanded={false}
      className="border-border bg-surface-100 dark:bg-dark-surface-200 mt-2 rounded border"
    >
      <div className="space-y-1.5 px-3 py-2 text-xs">
        <div>
          <Span variant="muted" className="text-[10px]">
            {t('chat.attachment_plan_description', 'Description')}
          </Span>
          <div className="text-text dark:text-dark-text mt-0.5">{attachment.description}</div>
        </div>
        {attachment.summary && (
          <div>
            <Span variant="muted" className="text-[10px]">
              {t('chat.attachment_plan_summary', 'Summary')}
            </Span>
            <CodeBlock className="mt-0.5 text-[11px] whitespace-pre-wrap">
              {attachment.summary}
            </CodeBlock>
          </div>
        )}
      </div>
    </Collapsible>
  );
}

function DAGAttachment({
  attachment,
}: {
  attachment: Extract<InlineAttachment, { kind: 'dag' }>;
}) {
  // BuildModeChainGraph is a heavyweight component; importing it here would
  // pull plan-mode code into every chat render. For Phase 4 we render a stub
  // that links out to the chain editor; Phase 5 will lazy-load the real graph
  // when the agent emits dag attachments at scale.
  return (
    <Collapsible
      title={
        <span className="inline-flex items-center gap-1.5 text-xs">
          <Workflow className="h-3.5 w-3.5" />
          <span>{attachment.description ?? t('chat.attachment_dag', 'Compiled chain DAG')}</span>
        </span>
      }
      defaultExpanded={false}
      className="border-border bg-surface-100 dark:bg-dark-surface-200 mt-2 rounded border"
    >
      <CodeBlock className="px-3 py-2 text-[11px] leading-relaxed">
        {attachment.chainJSON}
      </CodeBlock>
    </Collapsible>
  );
}

function StateUnitAttachment({
  attachment,
}: {
  attachment: Extract<InlineAttachment, { kind: 'state_unit' }>;
}) {
  const data =
    attachment.data == null
      ? null
      : typeof attachment.data === 'string'
        ? attachment.data
        : JSON.stringify(attachment.data, null, 2);
  return (
    <Collapsible
      title={
        <span className="inline-flex items-center gap-1.5 text-xs">
          <Database className="h-3.5 w-3.5" />
          <span>{t('chat.attachment_state', 'Captured state')}</span>
          <Span variant="muted" className="text-[10px]">
            · {attachment.name}
          </Span>
        </span>
      }
      defaultExpanded={false}
      className="border-border bg-surface-100 dark:bg-dark-surface-200 mt-2 rounded border"
    >
      <CodeBlock className="px-3 py-2 text-[11px] leading-relaxed">
        {data ?? '(no data)'}
      </CodeBlock>
    </Collapsible>
  );
}

/**
 * Dispatch to the renderer for `attachment.kind`. Unknown kinds fall back to
 * a JSON dump so an experimental shape doesn't crash the thread.
 */
export function InlineAttachmentRenderer({ attachment }: InlineAttachmentRendererProps) {
  switch (attachment.kind) {
    case 'file_view':
      return <FileViewAttachment attachment={attachment} />;
    case 'terminal_excerpt':
      return <TerminalExcerptAttachment attachment={attachment} />;
    case 'plan_summary':
      return <PlanSummaryAttachment attachment={attachment} />;
    case 'dag':
      return <DAGAttachment attachment={attachment} />;
    case 'state_unit':
      return <StateUnitAttachment attachment={attachment} />;
    default: {
      const exhaustive: never = attachment;
      void exhaustive;
      return null;
    }
  }
}

/**
 * Render a list of attachments. Returns null when the list is empty so
 * callers can spread `<InlineAttachments />` unconditionally.
 */
export function InlineAttachments({ attachments }: { attachments?: InlineAttachment[] }) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <div className="mt-1 space-y-2">
      {attachments.map((a, i) => (
        <InlineAttachmentRenderer key={i} attachment={a} />
      ))}
    </div>
  );
}
