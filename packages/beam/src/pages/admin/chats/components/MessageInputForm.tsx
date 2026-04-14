import { ChatComposer, Tooltip, Badge } from '@contenox/ui';
import { t } from 'i18next';
import { FormEvent, useCallback, useMemo, useRef, useState } from 'react';

import {
  parseSlashInvocation,
  useSlashCommandRegistry,
  type SlashCommandContext,
} from '../../../../lib/slashCommands';
import {
  useArtifactRegistry,
  useArtifactSources,
  type ArtifactSource,
} from '../../../../lib/artifacts';
import type { ChatContextArtifact } from '../../../../lib/types';

type MessageInputFormProps = {
  value: string;
  onChange: (value: string) => void;
  onSubmit: (e: FormEvent) => void;
  placeholder?: string;
  isPending: boolean;
  buttonLabel?: string;
  /** Omit or leave empty to hide the composer heading (preferred on chat thread pages). */
  title?: string;
  className?: string;
  variant?: 'default' | 'compact' | 'workbench';
  maxLength?: number;
  canSubmit?: boolean;
  /** When true, submit is allowed with an empty message (e.g. chat mode build). */
  allowEmptyMessage?: boolean;
};

/**
 * Inline notice shown below the composer when a slash command emits info or
 * an error. Persists until the next user keystroke clears it — lightweight
 * toast replacement with no library dependency.
 */
type SlashNotice = { level: 'info' | 'error'; message: string } | null;

export const MessageInputForm = ({
  value,
  onChange,
  onSubmit,
  placeholder = t('chat.input_placeholder'),
  isPending,
  buttonLabel = t('chat.send_button'),
  title = '',
  className,
  variant = 'default',
  maxLength,
  canSubmit,
  allowEmptyMessage = false,
}: MessageInputFormProps) => {
  const effectiveMax = maxLength ?? (variant === 'workbench' ? 8000 : 4000);
  const slashRegistry = useSlashCommandRegistry();
  const artifactRegistry = useArtifactRegistry();
  const sources = useArtifactSources();
  const [notice, setNotice] = useState<SlashNotice>(null);
  /** Armed one-shot sources keyed by sourceId so slash commands can dedupe. */
  const armedUnregistersRef = useRef(new Map<string, () => void>());

  /**
   * Arm a one-shot artifact source via the registry. If the same sourceId was
   * armed earlier, replace its artifact (later arming wins). The source
   * returns the artifact exactly once, then unregisters itself so the next
   * turn is clean.
   */
  const armArtifact = useCallback(
    (sourceId: string, label: string, artifact: ChatContextArtifact) => {
      const existing = armedUnregistersRef.current.get(sourceId);
      if (existing) {
        existing();
        armedUnregistersRef.current.delete(sourceId);
      }
      const source: ArtifactSource = {
        id: sourceId,
        label,
        collect: () => {
          queueMicrotask(() => {
            const un = armedUnregistersRef.current.get(sourceId);
            if (un) {
              un();
              armedUnregistersRef.current.delete(sourceId);
            }
          });
          return artifact;
        },
      };
      const unregister = artifactRegistry.register(source);
      armedUnregistersRef.current.set(sourceId, unregister);
    },
    [artifactRegistry],
  );

  const notify = useCallback((level: 'info' | 'error', message: string) => {
    setNotice({ level, message });
  }, []);

  const handleChange = useCallback(
    (next: string) => {
      if (notice) setNotice(null);
      onChange(next);
    },
    [notice, onChange],
  );

  /**
   * Intercept submit: if the first line is a slash command, dispatch it and
   * submit only the remaining body. Unknown commands surface a friendly
   * error without sending.
   */
  const handleSubmit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      const inv = parseSlashInvocation(value);
      if (!inv) {
        onSubmit(e);
        return;
      }
      const cmd = slashRegistry.get(inv.trigger, inv.name);
      if (!cmd) {
        setNotice({
          level: 'error',
          message: `Unknown ${inv.trigger === '@' ? 'mention' : 'command'}: ${inv.trigger}${inv.name}. Try /help.`,
        });
        return;
      }
      const ctx: SlashCommandContext = {
        commandName: inv.name,
        rawArgs: inv.rawArgs,
        armArtifact,
        notify,
      };
      try {
        await cmd.execute(ctx);
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        setNotice({ level: 'error', message: `${inv.trigger}${inv.name}: ${msg}` });
        return;
      }
      // Replace the composer's contents with the trailing body (if any). If
      // the command produced no body, clear the composer. In either case, do
      // NOT auto-submit — the user reviews the pending-attachments pills and
      // presses Enter again to send.
      onChange(inv.body);
    },
    [armArtifact, notify, onChange, onSubmit, slashRegistry, value],
  );

  /**
   * Mention-armed sources (id starting with `mention:`) are shown as pending
   * pills above the composer so the user can see what will attach to the
   * next send. Sticky sources (workspace `open_file`, etc.) are omitted —
   * they have their own indicators in their owning panels. The legacy
   * `slash:` prefix is also accepted for back-compat with any unmigrated
   * caller.
   */
  const pendingPills = useMemo(
    () => sources.filter((s) => s.id.startsWith('mention:') || s.id.startsWith('slash:')),
    [sources],
  );

  return (
    <div className={className}>
      {pendingPills.length > 0 && (
        <div className="mb-2 flex flex-wrap items-center gap-1.5">
          {pendingPills.map((s) => (
            <span
              key={s.id}
              className="bg-primary/10 text-primary inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs"
              title={t('chat.pending_attachment', 'Will attach to next message')}
            >
              📎 {s.label}
            </span>
          ))}
        </div>
      )}
      <ChatComposer
        value={value}
        onChange={handleChange}
        onSubmit={handleSubmit}
        placeholder={placeholder}
        isPending={isPending}
        submitLabel={buttonLabel}
        pendingLabel={t('chat.sending_button')}
        title={title}
        variant={variant}
        maxLength={effectiveMax}
        canSubmit={canSubmit}
        allowEmptyMessage={allowEmptyMessage}
        charCountTooltip={
          variant === 'default' || variant === 'workbench' ? t('chat.char_count_tooltip') : undefined
        }
        footerStart={
          variant === 'default' ? (
            <>
              <Tooltip content={t('chat.enter_to_send')}>
                <Badge variant="outline" size="sm">
                  ⏎ {t('chat.send')}
                </Badge>
              </Tooltip>
              <Tooltip content={t('chat.shift_enter_newline')}>
                <Badge variant="outline" size="sm">
                  ⇧ + ⏎ {t('chat.new_line')}
                </Badge>
              </Tooltip>
              <Tooltip content={t('chat.slash_hint', 'Type / for actions')}>
                <Badge variant="outline" size="sm">
                  /
                </Badge>
              </Tooltip>
              <Tooltip content={t('chat.mention_hint', 'Type @ to mention context (file, plan, terminal)')}>
                <Badge variant="outline" size="sm">
                  @
                </Badge>
              </Tooltip>
            </>
          ) : undefined
        }
      />
      {notice && (
        <pre
          className={
            notice.level === 'error'
              ? 'text-destructive mt-2 whitespace-pre-wrap font-mono text-xs'
              : 'text-text-muted mt-2 whitespace-pre-wrap font-mono text-xs'
          }
        >
          {notice.message}
        </pre>
      )}
    </div>
  );
};
