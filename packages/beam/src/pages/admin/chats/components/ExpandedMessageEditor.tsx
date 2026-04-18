import Editor, { type OnMount } from '@monaco-editor/react';
import { Button, Dialog } from '@contenox/ui';
import { t } from 'i18next';
import { useCallback, useEffect, useRef } from 'react';

import { defineBeamMonacoThemes, useMonacoAppTheme } from '../../../../lib/monacoAppTheme';

type ExpandedMessageEditorProps = {
  open: boolean;
  onClose: () => void;
  value: string;
  onChange: (value: string) => void;
  /** Send the current draft (same as main composer submit). */
  onSend: () => void;
  isPending: boolean;
  canSubmit: boolean;
};

export function ExpandedMessageEditor({
  open,
  onClose,
  value,
  onChange,
  onSend,
  isPending,
  canSubmit,
}: ExpandedMessageEditorProps) {
  const monacoTheme = useMonacoAppTheme();
  const monacoRef = useRef<Parameters<OnMount>[1] | null>(null);

  useEffect(() => {
    monacoRef.current?.editor.setTheme(monacoTheme);
  }, [monacoTheme]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  const handleMount: OnMount = (editor, monaco) => {
    monacoRef.current = monaco;
    editor.focus();
  };

  const handleSend = useCallback(() => {
    if (!canSubmit || isPending) return;
    onSend();
    onClose();
  }, [canSubmit, isPending, onSend, onClose]);

  if (!open) return null;

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t('chat.expand_editor_title')}
      className="flex max-h-[min(92vh,900px)] w-[min(96vw,1200px)] max-w-[min(96vw,1200px)] flex-col">
      <div className="flex min-h-[min(70vh,640px)] flex-1 flex-col gap-4">
        <div className="border-surface-200 dark:border-dark-surface-600 min-h-[min(60vh,560px)] flex-1 overflow-hidden rounded-lg border">
          <Editor
            height="560px"
            beforeMount={defineBeamMonacoThemes}
            theme={monacoTheme}
            defaultLanguage="markdown"
            value={value}
            onChange={v => onChange(v ?? '')}
            onMount={handleMount}
            options={{
              automaticLayout: true,
              wordWrap: 'on',
              minimap: { enabled: true },
              tabSize: 2,
              padding: { top: 12, bottom: 12 },
              fontFamily: '"Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
              scrollBeyondLastLine: false,
              lineNumbers: 'on',
            }}
          />
        </div>
        <p className="text-text-muted dark:text-dark-secondary-400 text-xs">
          {t('chat.expand_editor_hint')}
        </p>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose} disabled={isPending}>
            {t('chat.expand_editor_close')}
          </Button>
          <Button type="button" variant="primary" onClick={handleSend} disabled={!canSubmit || isPending}>
            {t('chat.expand_editor_send')}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
