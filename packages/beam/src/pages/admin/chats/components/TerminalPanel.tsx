import { Button, P, Span, Spinner } from '@contenox/ui';
import { RotateCcw, X } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { t } from 'i18next';
import { XTerminal } from '../../../../components/XTerminal';
import { api } from '../../../../lib/api';

const SESSION_KEY = 'beam_terminal_session_id';

export function TerminalPanel({ className }: { className?: string }) {
  const [wsUrl, setWsUrl] = useState<string | null>(null);
  const [initializing, setInitializing] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const sessionIdRef = useRef<string | null>(null);

  const persist = useCallback((sessionId: string | null) => {
    sessionIdRef.current = sessionId;
    try {
      if (sessionId) localStorage.setItem(SESSION_KEY, sessionId);
      else localStorage.removeItem(SESSION_KEY);
    } catch {
      /* quota */
    }
  }, []);

  const createSession = useCallback(async () => {
    setInitializing(true);
    setError(null);
    setWsUrl(null);
    try {
      // Empty cwd → server defaults to project root (parent of .contenox)
      const res = await api.createTerminalSession({ cwd: '' });
      persist(res.id);
      setWsUrl(`/api${res.wsPath}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create terminal session');
    } finally {
      setInitializing(false);
    }
  }, [persist]);

  // On mount: try saved session, fall back to new session
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
        await createSession();
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const handleDisconnect = useCallback(() => {
    persist(null);
    setWsUrl(null);
    createSession();
  }, [persist, createSession]);

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
    await createSession();
  }, [persist, createSession]);

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

  if (error) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 p-4">
        <Span variant="muted" className="text-sm">
          {error}
        </Span>
        <Button variant="secondary" size="sm" onClick={createSession}>
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
        <Button variant="primary" size="sm" onClick={createSession}>
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
          {t('terminal.title', 'Terminal')}
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
