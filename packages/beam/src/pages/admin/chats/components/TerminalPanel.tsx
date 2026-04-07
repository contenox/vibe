import { Button, P, Span, Spinner } from '@contenox/ui';
import { RotateCcw, X } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { t } from 'i18next';
import { Link } from 'react-router-dom';
import { XTerminal } from '../../../../components/XTerminal';
import { api } from '../../../../lib/api';
import type { Workspace } from '../../../../lib/types';

const SESSION_KEY = 'beam_terminal_session_id';
const WORKSPACE_KEY = 'beam_terminal_workspace_id';

export function TerminalPanel({ className }: { className?: string }) {
  const [wsUrl, setWsUrl] = useState<string | null>(null);
  const [initializing, setInitializing] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [noWorkspaces, setNoWorkspaces] = useState(false);
  const [activeWorkspace, setActiveWorkspace] = useState<Workspace | null>(null);
  const sessionIdRef = useRef<string | null>(null);

  const persist = useCallback((sessionId: string | null, workspaceId?: string | null) => {
    sessionIdRef.current = sessionId;
    try {
      if (sessionId) localStorage.setItem(SESSION_KEY, sessionId);
      else localStorage.removeItem(SESSION_KEY);
      if (workspaceId !== undefined) {
        if (workspaceId) localStorage.setItem(WORKSPACE_KEY, workspaceId);
        else localStorage.removeItem(WORKSPACE_KEY);
      }
    } catch {
      /* quota */
    }
  }, []);

  const createWithWorkspace = useCallback(
    async (ws: Workspace) => {
      setInitializing(true);
      setError(null);
      setWsUrl(null);
      setNoWorkspaces(false);
      try {
        const res = await api.createTerminalSession({ workspaceId: ws.id });
        persist(res.id, ws.id);
        setActiveWorkspace(ws);
        setWsUrl(`/api${res.wsPath}`);
      } catch (e) {
        setError(e instanceof Error ? e.message : 'Failed to create terminal session');
      } finally {
        setInitializing(false);
      }
    },
    [persist],
  );

  const resolveAndCreate = useCallback(async () => {
    setInitializing(true);
    setError(null);
    setNoWorkspaces(false);
    try {
      const workspaces = await api.listWorkspaces();
      if (!workspaces || workspaces.length === 0) {
        setNoWorkspaces(true);
        setInitializing(false);
        return;
      }

      // Prefer last-used workspace
      const savedWsId = (() => {
        try {
          return localStorage.getItem(WORKSPACE_KEY);
        } catch {
          return null;
        }
      })();
      const ws = workspaces.find(w => w.id === savedWsId) ?? workspaces[0];
      await createWithWorkspace(ws);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load workspaces');
      setInitializing(false);
    }
  }, [createWithWorkspace]);

  // On mount: try saved session, fall back to workspace-based create
  useEffect(() => {
    let cancelled = false;
    (async () => {
      const savedId = (() => {
        try {
          return localStorage.getItem(SESSION_KEY);
        } catch {
          return null;
        }
      })();

      if (savedId) {
        try {
          const session = await api.getTerminalSession(savedId);
          if (cancelled) return;
          if (session.status === 'active') {
            sessionIdRef.current = savedId;
            setWsUrl(`/api/terminal/sessions/${savedId}/ws`);
            setInitializing(false);
            return;
          }
        } catch {
          // Session gone
        }
        if (cancelled) return;
        persist(null);
      }

      if (!cancelled) {
        await resolveAndCreate();
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const handleDisconnect = useCallback(() => {
    persist(null);
    setWsUrl(null);
    resolveAndCreate();
  }, [persist, resolveAndCreate]);

  const handleRestart = useCallback(async () => {
    const oldId = sessionIdRef.current;
    persist(null);
    setWsUrl(null);
    if (oldId) {
      try {
        await api.deleteTerminalSession(oldId);
      } catch {
        /* already gone */
      }
    }
    await resolveAndCreate();
  }, [persist, resolveAndCreate]);

  const handleClose = useCallback(async () => {
    const oldId = sessionIdRef.current;
    persist(null);
    setWsUrl(null);
    setInitializing(false);
    if (oldId) {
      try {
        await api.deleteTerminalSession(oldId);
      } catch {
        /* already gone */
      }
    }
  }, [persist]);

  if (initializing) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner size="md" />
      </div>
    );
  }

  if (noWorkspaces) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 p-4 text-center">
        <P variant="muted" className="text-sm">
          {t('terminal.no_workspaces', 'No workspaces configured. Create a workspace to use the terminal.')}
        </P>
        <Link to="/workspaces">
          <Button variant="primary" size="sm">
            {t('terminal.go_to_workspaces', 'Configure Workspaces')}
          </Button>
        </Link>
      </div>
    );
  }

  if (error) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 p-4">
        <Span variant="muted" className="text-sm">
          {error}
        </Span>
        <Button variant="secondary" size="sm" onClick={resolveAndCreate}>
          {t('terminal.retry', 'Retry')}
        </Button>
      </div>
    );
  }

  if (!wsUrl) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 p-4">
        <Span variant="muted" className="text-sm">
          {t('terminal.no_session', 'No terminal session')}
        </Span>
        <Button variant="primary" size="sm" onClick={resolveAndCreate}>
          {t('terminal.create', 'Open Terminal')}
        </Button>
      </div>
    );
  }

  return (
    <div className={`flex h-full flex-col ${className ?? ''}`}>
      {/* Title bar */}
      <div className="border-border bg-surface-100 dark:bg-dark-surface-200 flex shrink-0 items-center justify-between gap-2 border-b px-2 py-1.5">
        <Span variant="muted" className="text-xs font-medium">
          {activeWorkspace
            ? `${t('terminal.title', 'Terminal')} — ${activeWorkspace.name}`
            : t('terminal.title', 'Terminal')}
        </Span>
        <div className="flex items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="xs"
            onClick={handleRestart}
            title={t('terminal.restart', 'Restart')}
          >
            <RotateCcw className="h-3.5 w-3.5" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="xs"
            onClick={handleClose}
            title={t('terminal.close', 'Close')}
          >
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      </div>
      {/* Terminal */}
      <div className="min-h-0 flex-1">
        <XTerminal wsUrl={wsUrl} onDisconnect={handleDisconnect} />
      </div>
    </div>
  );
}
