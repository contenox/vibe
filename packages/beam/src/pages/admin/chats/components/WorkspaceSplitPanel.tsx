import Editor, { type OnMount } from '@monaco-editor/react';
import { Button, FileTree, type FileTreeNode, InlineNotice, Panel, Span, Spinner } from '@contenox/ui';
import { ChevronRight, Save } from 'lucide-react';
import { t } from 'i18next';
import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useState,
} from 'react';
import { useListFiles, useUpdateFile } from '../../../../hooks/useFiles';
import { cn } from '../../../../lib/utils';
import type { ChatContextPayload, FileResponse } from '../../../../lib/types';
import { useMonacoAppTheme } from '../../../../lib/monacoAppTheme';
import { toFileTreeNodes } from '../../../../lib/vfsFileTree';
import {
  buildWorkspaceChatContext,
  readWorkspaceFileText,
} from '../../../../lib/workspaceFileContext';

export type WorkspaceSplitHandle = {
  buildChatContext: () => ChatContextPayload | undefined;
};

function monacoLanguageForPath(p: string): string {
  const ext = p.split('.').pop()?.toLowerCase() ?? '';
  const map: Record<string, string> = {
    md: 'markdown',
    mdx: 'markdown',
    json: 'json',
    ts: 'typescript',
    tsx: 'typescript',
    js: 'javascript',
    jsx: 'javascript',
    mjs: 'javascript',
    cjs: 'javascript',
    css: 'css',
    scss: 'scss',
    html: 'html',
    go: 'go',
    py: 'python',
    rs: 'rust',
    yml: 'yaml',
    yaml: 'yaml',
    toml: 'ini',
    sh: 'shell',
    sql: 'sql',
  };
  return map[ext] ?? 'plaintext';
}

type Props = {
  className?: string;
};

const WorkspaceSplitPanel = forwardRef<WorkspaceSplitHandle, Props>(function WorkspaceSplitPanel(
  { className },
  ref,
) {
  const [currentDir, setCurrentDir] = useState('');
  const [selectedFileId, setSelectedFileId] = useState<string | null>(null);
  const [editorText, setEditorText] = useState('');
  const [savedSnapshot, setSavedSnapshot] = useState('');
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [isTextFile, setIsTextFile] = useState(true);

  const listPath = currentDir || undefined;
  const { data: entries = [], isLoading, error: listError } = useListFiles(listPath);
  const updateFile = useUpdateFile();
  const monacoTheme = useMonacoAppTheme();

  const sortedEntries = useMemo(() => {
    const copy = [...entries];
    copy.sort((a, b) => {
      const ad = a.isDirectory ? 0 : 1;
      const bd = b.isDirectory ? 0 : 1;
      if (ad !== bd) return ad - bd;
      return a.path.localeCompare(b.path);
    });
    return copy;
  }, [entries]);

  const selectedPath = selectedFileId ?? '';

  useEffect(() => {
    if (!selectedFileId) {
      setEditorText('');
      setSavedSnapshot('');
      setLoadError(null);
      setIsTextFile(true);
      return;
    }

    let cancelled = false;
    setLoadError(null);
    setIsTextFile(true);

    (async () => {
      try {
        const text = await readWorkspaceFileText(selectedFileId);
        if (cancelled) return;
        setEditorText(text);
        setSavedSnapshot(text);
      } catch (e) {
        if (cancelled) return;
        const msg = e instanceof Error ? e.message : String(e);
        if (msg === 'FILE_BINARY' || msg.includes('BINARY')) {
          setIsTextFile(false);
          setEditorText('');
          setSavedSnapshot('');
          setLoadError(t('chat.workspace_binary_file'));
        } else if (msg === 'FILE_TOO_LARGE') {
          setIsTextFile(false);
          setLoadError(t('chat.workspace_file_too_large'));
        } else {
          setLoadError(msg || t('chat.workspace_load_error'));
        }
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [selectedFileId]);

  const dirty = isTextFile && selectedFileId !== null && editorText !== savedSnapshot;

  const selectedEntry = entries.find(e => e.id === selectedFileId);
  const canAttachContext = Boolean(
    selectedFileId &&
      isTextFile &&
      !loadError &&
      selectedPath &&
      !(selectedEntry?.isDirectory ?? false),
  );

  useImperativeHandle(
    ref,
    () => ({
      buildChatContext: () => {
        if (!canAttachContext) return undefined;
        return buildWorkspaceChatContext(selectedPath, editorText, true);
      },
    }),
    [canAttachContext, selectedPath, editorText],
  );

  const breadcrumbParts = useMemo(() => {
    if (!currentDir) return [];
    return currentDir.split('/').filter(Boolean);
  }, [currentDir]);

  const navigateUp = useCallback(() => {
    const parts = currentDir.split('/').filter(Boolean);
    parts.pop();
    setCurrentDir(parts.join('/'));
    setSelectedFileId(null);
  }, [currentDir]);

  const onEntryClick = useCallback((entry: FileResponse) => {
    setSaveError(null);
    if (entry.isDirectory) {
      setCurrentDir(entry.path);
      setSelectedFileId(null);
      return;
    }
    setSelectedFileId(entry.id);
  }, []);

  const fileTreeNodes = useMemo(() => toFileTreeNodes(sortedEntries), [sortedEntries]);

  const onTreeNodeSelect = useCallback(
    (node: FileTreeNode) => {
      const entry = sortedEntries.find(e => e.id === node.id);
      if (entry) onEntryClick(entry);
    },
    [sortedEntries, onEntryClick],
  );

  const handleSave = useCallback(() => {
    if (!selectedFileId || !dirty) return;
    setSaveError(null);
    const name = selectedPath.split('/').pop() ?? 'file.txt';
    const blob = new Blob([editorText], { type: 'text/plain;charset=utf-8' });
    const formData = new FormData();
    formData.append('file', blob, name);
    updateFile.mutate(
      { id: selectedFileId, formData },
      {
        onSuccess: () => {
          setSavedSnapshot(editorText);
        },
        onError: err => {
          setSaveError(err.message || t('chat.workspace_save_error'));
        },
      },
    );
  }, [selectedFileId, dirty, editorText, selectedPath, updateFile, t]);

  const handleEditorMount: OnMount = editor => {
    requestAnimationFrame(() => editor.layout());
  };

  return (
    <div
      className={cn(
        'border-border bg-surface-50 dark:bg-dark-surface-100 flex min-h-0 w-full min-w-0 shrink-0 flex-col border-l',
        className,
      )}>
      <div className="border-border flex shrink-0 flex-col gap-1 border-b px-3 py-2">
        <Span variant="body" className="text-text dark:text-dark-text font-medium">
          {t('chat.workspace_title')}
        </Span>
        <div className="text-text-muted dark:text-dark-text-muted flex min-w-0 flex-wrap items-center gap-0.5 text-xs">
          <button
            type="button"
            className="hover:text-text shrink-0 rounded px-1 py-0.5 underline-offset-2 hover:underline"
            onClick={() => {
              setCurrentDir('');
              setSelectedFileId(null);
            }}>
            {t('chat.workspace_root')}
          </button>
          {breadcrumbParts.map((part, i) => {
            const prefix = breadcrumbParts.slice(0, i + 1).join('/');
            return (
              <span key={prefix} className="flex min-w-0 items-center gap-0.5">
                <ChevronRight className="h-3 w-3 shrink-0 opacity-50" />
                <button
                  type="button"
                  className="hover:text-text truncate rounded px-1 py-0.5 underline-offset-2 hover:underline"
                  onClick={() => {
                    setCurrentDir(prefix);
                    setSelectedFileId(null);
                  }}>
                  {part}
                </button>
              </span>
            );
          })}
          {currentDir ? (
            <button
              type="button"
              className="hover:text-text ml-1 shrink-0 text-[10px] uppercase tracking-wide"
              onClick={navigateUp}>
              {t('chat.workspace_up')}
            </button>
          ) : null}
        </div>
      </div>

      <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2 p-2">
        {listError ? (
          <InlineNotice variant="error">{listError.message}</InlineNotice>
        ) : null}

        <Panel variant="bordered" className="min-h-[120px] max-h-[40%] shrink-0 overflow-auto p-2">
          {isLoading ? (
            <div className="flex justify-center py-6">
              <Spinner size="md" />
            </div>
          ) : sortedEntries.length === 0 ? (
            <Span variant="muted" className="text-sm">
              {t('chat.workspace_empty_dir')}
            </Span>
          ) : (
            <FileTree
              nodes={fileTreeNodes}
              selectedId={selectedFileId}
              onNodeSelect={onTreeNodeSelect}
              directoryClickMode="navigate"
              className="py-0.5"
            />
          )}
        </Panel>

        {loadError && !isTextFile ? (
          <InlineNotice variant="warning">{loadError}</InlineNotice>
        ) : loadError ? (
          <InlineNotice variant="error">{loadError}</InlineNotice>
        ) : null}

        {saveError ? <InlineNotice variant="error">{saveError}</InlineNotice> : null}

        <div className="border-border bg-surface-100/50 dark:bg-dark-surface-200/40 flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden rounded-lg border">
          {selectedFileId && isTextFile ? (
            <>
              <div className="border-border bg-surface-100 dark:bg-dark-surface-200 flex shrink-0 items-center justify-between gap-2 border-b px-2 py-2">
                <Span variant="muted" className="truncate font-mono text-xs">
                  {selectedPath}
                </Span>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  disabled={!dirty || updateFile.isPending}
                  isLoading={updateFile.isPending}
                  onClick={handleSave}>
                  <Save className="mr-1 h-3.5 w-3.5" />
                  {t('chat.workspace_save')}
                </Button>
              </div>
              <div className="relative min-h-[200px] flex-1 overflow-hidden">
                <div className="absolute inset-0 min-h-[200px]">
                  <Editor
                    height="100%"
                    theme={monacoTheme}
                    language={monacoLanguageForPath(selectedPath)}
                    value={editorText}
                    onChange={v => setEditorText(v ?? '')}
                    onMount={handleEditorMount}
                    loading={
                      <div className="bg-surface-100 dark:bg-dark-surface-300 flex h-full min-h-[200px] items-center justify-center">
                        <Spinner size="md" />
                      </div>
                    }
                    options={{
                      automaticLayout: true,
                      wordWrap: 'on',
                      minimap: { enabled: false },
                      tabSize: 2,
                      fontSize: 14,
                      lineHeight: 22,
                      padding: { top: 12, bottom: 12 },
                      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
                      fontLigatures: false,
                      renderWhitespace: 'selection',
                      scrollBeyondLastLine: false,
                      smoothScrolling: true,
                      cursorBlinking: 'smooth',
                      cursorSmoothCaretAnimation: 'on',
                    }}
                  />
                </div>
              </div>
              {canAttachContext ? (
                <div className="border-border bg-surface-50 dark:bg-dark-surface-300/50 text-text-secondary dark:text-dark-secondary-300 border-t px-2 py-2 text-xs leading-snug">
                  {t('chat.workspace_context_hint')}
                </div>
              ) : null}
            </>
          ) : (
            <div className="text-text-muted dark:text-dark-text-muted flex flex-1 items-center justify-center p-4 text-center text-sm">
              {t('chat.workspace_select_file')}
            </div>
          )}
        </div>
      </div>
    </div>
  );
});

export default WorkspaceSplitPanel;
