import Editor, { type OnMount } from '@monaco-editor/react';
import { Button, FileTree, type FileTreeNode, InlineNotice, Panel, Span, Spinner, Tabs } from '@contenox/ui';
import { ChevronRight, Pin, PinOff, Save, TerminalSquare, FolderOpen } from 'lucide-react';
import { t } from 'i18next';
import {
  forwardRef,
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from 'react';

const TerminalPanel = lazy(() =>
  import('./TerminalPanel').then(m => ({ default: m.TerminalPanel })),
);
import { useListFiles, useUpdateFile } from '../../../../hooks/useFiles';
import { cn } from '../../../../lib/utils';
import type { ChatContextPayload, FileResponse } from '../../../../lib/types';
import { BEAM_LAYOUT_CHANGED_EVENT } from '../../../../lib/beamLayout';
import { defineBeamMonacoThemes, useMonacoAppTheme } from '../../../../lib/monacoAppTheme';
import { toFileTreeNodes } from '../../../../lib/vfsFileTree';
import {
  buildOpenFileArtifact,
  readWorkspaceFileText,
} from '../../../../lib/workspaceFileContext';
import { useArtifactSource, type ArtifactSource } from '../../../../lib/artifacts';

export type WorkspaceSplitHandle = {
  buildChatContext: () => ChatContextPayload | undefined;
};

function monacoLanguageForPath(p: string): string {
  const ext = p.split('.').pop()?.toLowerCase() ?? '';
  const map: Record<string, string> = {
    // Web
    html: 'html',
    htm: 'html',
    css: 'css',
    scss: 'scss',
    less: 'less',
    // JavaScript / TypeScript
    js: 'javascript',
    jsx: 'javascript',
    mjs: 'javascript',
    cjs: 'javascript',
    ts: 'typescript',
    tsx: 'typescript',
    mts: 'typescript',
    cts: 'typescript',
    // Data / Config
    json: 'json',
    jsonc: 'json',
    json5: 'json',
    yml: 'yaml',
    yaml: 'yaml',
    toml: 'ini',
    ini: 'ini',
    env: 'ini',
    xml: 'xml',
    svg: 'xml',
    csv: 'plaintext',
    // Markdown
    md: 'markdown',
    mdx: 'markdown',
    // Systems
    go: 'go',
    rs: 'rust',
    c: 'c',
    h: 'c',
    cpp: 'cpp',
    cc: 'cpp',
    cxx: 'cpp',
    hpp: 'cpp',
    cs: 'csharp',
    java: 'java',
    kt: 'kotlin',
    kts: 'kotlin',
    swift: 'swift',
    // Scripting
    py: 'python',
    pyi: 'python',
    rb: 'ruby',
    php: 'php',
    pl: 'perl',
    lua: 'lua',
    r: 'r',
    R: 'r',
    // Shell
    sh: 'shell',
    bash: 'shell',
    zsh: 'shell',
    fish: 'shell',
    ps1: 'powershell',
    bat: 'bat',
    cmd: 'bat',
    // Database
    sql: 'sql',
    // DevOps / Infra
    dockerfile: 'dockerfile',
    tf: 'hcl',
    hcl: 'hcl',
    proto: 'protobuf',
    graphql: 'graphql',
    gql: 'graphql',
    // Misc
    makefile: 'plaintext',
    mk: 'plaintext',
    gitignore: 'plaintext',
    dockerignore: 'plaintext',
    mod: 'go',
    sum: 'plaintext',
    lock: 'json',
    txt: 'plaintext',
    log: 'plaintext',
    diff: 'plaintext',
    patch: 'plaintext',
  };
  // Handle extensionless filenames like Dockerfile, Makefile
  const basename = p.split('/').pop()?.toLowerCase() ?? '';
  if (ext === '' || !map[ext]) {
    const nameMap: Record<string, string> = {
      dockerfile: 'dockerfile',
      makefile: 'plaintext',
      gemfile: 'ruby',
      rakefile: 'ruby',
      vagrantfile: 'ruby',
    };
    return nameMap[basename] ?? map[ext] ?? 'plaintext';
  }
  return map[ext] ?? 'plaintext';
}

type Props = {
  className?: string;
};

/**
 * localStorage key for the workspace-global set of pinned file paths. Pinned
 * files attach themselves as `open_file` artifacts to every chat turn while
 * the workspace panel is mounted. Workspace-global rather than per-chat so a
 * pin survives tab/session switches; clear by unpinning.
 */
const PINNED_FILES_STORAGE_KEY = 'beam_workspace_pinned_paths';

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
  const [workspaceTab, setWorkspaceTab] = useState<'files' | 'terminal'>(() => {
    try {
      return (localStorage.getItem('beam_workspace_tab') as 'files' | 'terminal') || 'files';
    } catch {
      return 'files';
    }
  });

  const switchTab = useCallback((tab: 'files' | 'terminal') => {
    setWorkspaceTab(tab);
    try { localStorage.setItem('beam_workspace_tab', tab); } catch { /* quota */ }
  }, []);

  const listPath = currentDir || undefined;
  const { data: entries = [], isLoading, error: listError } = useListFiles(listPath);
  const updateFile = useUpdateFile();
  const monacoTheme = useMonacoAppTheme();
  const monacoApiRef = useRef<Parameters<OnMount>[1] | null>(null);

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
      // Legacy path: superseded by the sticky open_file ArtifactSource below.
      // Kept so external callers that still invoke the handle see a no-op
      // instead of breaking; remove once no consumers remain.
      buildChatContext: () => undefined,
    }),
    [],
  );

  /**
   * Sticky open_file source — opt-in. Earlier this auto-attached the open
   * file to every turn; the user pushed back ("sticky should be opt-in"),
   * so registration now requires an explicit pin from the editor header.
   *
   * Pin state is workspace-global and persisted to localStorage as a string
   * set so unrelated chat sessions can share pinned files without coordinating.
   *
   * collect() reads fresh editor text/path via refs, so unsaved edits land
   * in context.
   */
  const editorTextRef = useRef(editorText);
  editorTextRef.current = editorText;
  const selectedPathRef = useRef(selectedPath);
  selectedPathRef.current = selectedPath;
  const [pinnedPaths, setPinnedPaths] = useState<Set<string>>(() => {
    if (typeof window === 'undefined') return new Set();
    try {
      const raw = window.localStorage.getItem(PINNED_FILES_STORAGE_KEY);
      if (!raw) return new Set();
      const arr = JSON.parse(raw);
      return Array.isArray(arr) ? new Set(arr.filter((s): s is string => typeof s === 'string')) : new Set();
    } catch {
      return new Set();
    }
  });
  const isPinned = !!selectedPath && pinnedPaths.has(selectedPath);
  const togglePin = useCallback(() => {
    if (!selectedPath) return;
    setPinnedPaths((prev) => {
      const next = new Set(prev);
      if (next.has(selectedPath)) next.delete(selectedPath);
      else next.add(selectedPath);
      try {
        window.localStorage.setItem(
          PINNED_FILES_STORAGE_KEY,
          JSON.stringify(Array.from(next)),
        );
      } catch {
        /* quota / disabled storage */
      }
      return next;
    });
  }, [selectedPath]);
  const openFileSource = useMemo<ArtifactSource | null>(() => {
    if (!canAttachContext || !isPinned) return null;
    return {
      id: 'workspace:open_file',
      label: t('chat.open_file_artifact_label', 'Pinned file attached as context'),
      collect: () => {
        const path = selectedPathRef.current;
        if (!path) return null;
        return buildOpenFileArtifact(path, editorTextRef.current);
      },
    };
  }, [canAttachContext, isPinned]);
  useArtifactSource(openFileSource);

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

  useEffect(() => {
    monacoApiRef.current?.editor.setTheme(monacoTheme);
  }, [monacoTheme]);

  useEffect(() => {
    if (workspaceTab === 'terminal') {
      queueMicrotask(() => window.dispatchEvent(new CustomEvent(BEAM_LAYOUT_CHANGED_EVENT)));
    }
  }, [workspaceTab]);

  const handleEditorMount: OnMount = (editor, monaco) => {
    monacoApiRef.current = monaco;
    requestAnimationFrame(() => editor.layout());
  };

  return (
    <div
      className={cn(
        'border-border bg-surface-50 dark:bg-dark-surface-100 flex min-h-0 w-full min-w-0 shrink-0 flex-col border-l',
        className,
      )}>
      {/* Tab bar */}
      <Tabs
        tabs={[
          { id: 'files' as const, label: <><FolderOpen className="h-3.5 w-3.5" /> {t('chat.workspace_tab_files', 'Files')}</> },
          { id: 'terminal' as const, label: <><TerminalSquare className="h-3.5 w-3.5" /> {t('chat.workspace_tab_terminal', 'Terminal')}</> },
        ]}
        activeTab={workspaceTab}
        onTabChange={switchTab}
        className="border-border shrink-0 border-b"
      />

      {/* Terminal tab — always mounted so the session survives tab switches */}
      <div className={cn(
        'min-h-0 w-full min-w-0 flex-1 flex-col overflow-hidden',
        workspaceTab === 'terminal' ? 'flex' : 'hidden',
      )}>
        <Suspense
          fallback={
            <div className="flex flex-1 items-center justify-center">
              <Spinner size="md" />
            </div>
          }>
          <TerminalPanel className="min-h-0 flex-1" />
        </Suspense>
      </div>

      {/* Files tab */}
      <div className={cn(
        'min-h-0 min-w-0 flex-1 flex-col',
        workspaceTab === 'files' ? 'flex' : 'hidden',
      )}>
      {/* Breadcrumbs */}
      <div className="border-border flex shrink-0 flex-col gap-1 px-3 py-1.5">
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
          <InlineNotice variant="warning" onDismiss={() => setLoadError(null)}>{loadError}</InlineNotice>
        ) : loadError ? (
          <InlineNotice variant="error" onDismiss={() => setLoadError(null)}>{loadError}</InlineNotice>
        ) : null}

        {saveError ? <InlineNotice variant="error" onDismiss={() => setSaveError(null)}>{saveError}</InlineNotice> : null}

        <div className="border-border bg-surface-100/50 dark:bg-dark-surface-200/40 flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden rounded-lg border">
          {selectedFileId && isTextFile ? (
            <>
              <div className="border-border bg-surface-100 dark:bg-dark-surface-200 flex shrink-0 items-center justify-between gap-2 border-b px-2 py-2">
                <div className="flex min-w-0 items-center gap-1.5">
                  <Span variant="muted" className="truncate font-mono text-xs">
                    {selectedPath}
                  </Span>
                  {isPinned && (
                    <span
                      className="bg-primary/10 text-primary inline-flex items-center gap-0.5 rounded px-1 py-0.5 text-[10px]"
                      title={t(
                        'chat.workspace_pin_active',
                        'Pinned: this file attaches as context on every message',
                      )}
                    >
                      <Pin className="h-2.5 w-2.5" />
                      {t('chat.workspace_pin_label', 'pinned')}
                    </span>
                  )}
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    type="button"
                    variant="ghost"
                    size="xs"
                    onClick={togglePin}
                    title={
                      isPinned
                        ? t('chat.workspace_unpin', 'Unpin from chat context')
                        : t(
                            'chat.workspace_pin',
                            'Pin file: attach as context to every message in this chat',
                          )
                    }
                  >
                    {isPinned ? (
                      <PinOff className="h-3.5 w-3.5" />
                    ) : (
                      <Pin className="h-3.5 w-3.5" />
                    )}
                  </Button>
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
              </div>
              <div className="relative min-h-[200px] flex-1 overflow-hidden">
                <div className="absolute inset-0 min-h-[200px]">
                  <Editor
                    height="100%"
                    beforeMount={defineBeamMonacoThemes}
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
                      fontFamily: '"Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
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
                <div className="border-border border-t bg-surface-100 dark:bg-dark-surface-200 text-text-muted dark:text-dark-text-muted px-2 py-2 text-xs leading-snug">
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
    </div>
  );
});

export default WorkspaceSplitPanel;
