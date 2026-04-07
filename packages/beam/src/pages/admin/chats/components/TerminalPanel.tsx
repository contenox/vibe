import { Button, Span, Spinner } from '@contenox/ui';
import { RotateCcw, X } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { t } from 'i18next';
import { XTerminal } from '../../../../components/XTerminal';
import { api } from '../../../../lib/api';
import { ApiError } from '../../../../lib/fetch';

const SESSION_KEY = 'beam_terminal_session_id';
const DISCONNECT_RECREATE_MS = 350;

export function TerminalPanel({ className }: { className?: string }) {
  const [wsUrl, setWsUrl] = useState<string | null>(null);
  const [initializing, setInitializing] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const sessionIdRef = useRef<string | null>(null);
  const createGenRef = useRef(0);
  const disconnectDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const persist = useCallback((sessionId: string | null) => {
    sessionIdRef.current = sessionId;
    try {
      if (sessionId) localStorage.setItem(SESSION_KEY, sessionId);
      else localStorage.removeItem(SESSION_KEY);
    } catch {
      /* quota */
    }
  }, []);

  const clearDisconnectDebounce = useCallback(() => {
    if (disconnectDebounceRef.current) {
      clearTimeout(disconnectDebounceRef.current);
      disconnectDebounceRef.current = null;
    }
  }, []);

  const createSession = useCallback(async (retryAfterPrune = true) => {
    const gen = ++createGenRef.current;
    setInitializing(true);
    setError(null);
    setWsUrl(null);
    try {
      let res: Awaited<ReturnType<typeof api.createTerminalSession>>;
      try {
        res = await api.createTerminalSession({ cwd: '' });
      } catch (e) {
        const msg = e instanceof Error ? e.message : 'Failed to create terminal session';
        const tooMany =
          e instanceof ApiError &&
          e.status === 422 &&
          (msg.toLowerCase().includes('too many') || msg.toLowerCase().includes('concurrent'));
        if (tooMany && retryAfterPrune) {
          try {
            const list = await api.listTerminalSessions();
            await Promise.all(list.map(s => api.deleteTerminalSession(s.id).catch(() => undefined)));
          } catch {
            /* ignore prune errors */
          }
          res = await api.createTerminalSession({ cwd: '' });
        } else {
          throw e;
        }
      }
      if (gen !== createGenRef.current) return;
      persist(res.id);
      setWsUrl(`/api${res.wsPath}`);
      setError(null);
    } catch (e) {
      if (gen !== createGenRef.current) return;
      const msg = e instanceof Error ? e.message : 'Failed to create terminal session';
      setError(msg);
    } finally {
      if (gen === createGenRef.current) {
        setInitializing(false);
      }
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
      createGenRef.current++;
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(
    () => () => {
      clearDisconnectDebounce();
    },
    [clearDisconnectDebounce]
  );

  const handleDisconnect = useCallback(() => {
    const id = sessionIdRef.current;
    persist(null);
    setWsUrl(null);
    setInitializing(true);
    setError(null);
    clearDisconnectDebounce();
    disconnectDebounceRef.current = setTimeout(() => {
      disconnectDebounceRef.current = null;
      void (async () => {
        if (id) {
          try {
            await api.deleteTerminalSession(id);
          } catch {
            /* session may already be gone */
          }
        }
        await createSession();
      })();
    }, DISCONNECT_RECREATE_MS);
  }, [persist, createSession, clearDisconnectDebounce]);

  const handleRestart = useCallback(async () => {
    clearDisconnectDebounce();
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
  }, [persist, createSession, clearDisconnectDebounce]);

  const handleClose = useCallback(async () => {
    clearDisconnectDebounce();
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
  }, [persist, clearDisconnectDebounce]);

  const handleOpenTerminal = useCallback(() => {
    clearDisconnectDebounce();
    void createSession();
  }, [createSession, clearDisconnectDebounce]);

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
        <Button variant="secondary" size="sm" onClick={handleOpenTerminal}>
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
        <Button variant="primary" size="sm" onClick={handleOpenTerminal}>
          {t('terminal.create', 'Open Terminal')}
        </Button>
      </div>
    );
  }

  return (
    <div className={`flex h-full min-h-0 flex-col ${className ?? ''}`}>
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
