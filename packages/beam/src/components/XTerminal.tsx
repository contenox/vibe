import '@xterm/xterm/css/xterm.css';

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { forwardRef, useEffect, useImperativeHandle, useRef, useLayoutEffect } from 'react';
import { BEAM_LAYOUT_CHANGED_EVENT } from '../lib/beamLayout';
import { useXtermTheme } from '../lib/xtermTheme';
import { cn } from '../lib/utils';

export interface XTerminalProps {
  /** WebSocket URL path, e.g. "/api/terminal/sessions/{id}/ws" */
  wsUrl: string;
  /** Called when the WebSocket opens successfully. */
  onOpen?: () => void;
  /** Called when the WebSocket closes (session ended or connection lost). */
  onDisconnect?: () => void;
  /**
   * Called when the socket closed before `open` (proxy down, Strict Mode teardown, etc.).
   * Server-side `Attach` may already have deleted the session — parent should create a new one.
   */
  onConnectionFailed?: () => void;
  className?: string;
}

/**
 * Imperative handle exposed via forwardRef so parents can capture terminal
 * state (e.g. for the "attach last output" artifact source in Phase 2 of the
 * Beam canvas-vision plan).
 */
export type XTerminalHandle = {
  /**
   * Read the most recent N lines of terminal content (from the active buffer,
   * not the raw socket stream — so ANSI is already applied and the caller sees
   * exactly what the user saw). Returns null when the terminal is not ready.
   */
  captureRecentOutput: (maxLines?: number) => string | null;
};

export const XTerminal = forwardRef<XTerminalHandle, XTerminalProps>(function XTerminal(
  { wsUrl, onOpen, onDisconnect, onConnectionFailed, className },
  handleRef,
) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const onDisconnectRef = useRef(onDisconnect);
  const onOpenRef = useRef(onOpen);
  const onConnectionFailedRef = useRef(onConnectionFailed);
  /** Set in ws.onopen; read in ws.onclose — ref so we don’t fire onDisconnect when connect never succeeded. */
  const wsOpenedRef = useRef(false);
  const theme = useXtermTheme();
  const themeRef = useRef(theme);
  themeRef.current = theme;

  useLayoutEffect(() => {
    onDisconnectRef.current = onDisconnect;
    onOpenRef.current = onOpen;
    onConnectionFailedRef.current = onConnectionFailed;
  });

  useImperativeHandle(
    handleRef,
    () => ({
      captureRecentOutput: (maxLines = 200) => {
        const term = termRef.current;
        if (!term) return null;
        try {
          const buf = term.buffer.active;
          const total = buf.length;
          if (total === 0) return '';
          const start = Math.max(0, total - maxLines);
          const lines: string[] = [];
          for (let i = start; i < total; i++) {
            const line = buf.getLine(i);
            if (!line) continue;
            // translateToString(true) trims trailing whitespace per line.
            lines.push(line.translateToString(true));
          }
          // Drop leading blank lines so a mostly-empty screen doesn't produce
          // 200 newlines of padding.
          while (lines.length > 0 && lines[0] === '') lines.shift();
          return lines.join('\n');
        } catch {
          return null;
        }
      },
    }),
    [],
  );

  useEffect(() => {
    const t = termRef.current;
    const el = containerRef.current;
    if (t) {
      t.options.theme = theme;
    }
    if (el && theme.background != null) {
      el.style.backgroundColor = theme.background;
    }
  }, [theme]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    let closedByCleanup = false;

    const term = new Terminal({
      cursorBlink: true,
      fontFamily: '"Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 14,
      lineHeight: 1.2,
      theme: themeRef.current,
      allowProposedApi: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container);
    term.options.theme = themeRef.current;

    const focusTerm = () => {
      try {
        term.focus();
      } catch {
        /* ignore */
      }
    };

    const safeFit = () => {
      try {
        if (container.clientWidth < 8 || container.clientHeight < 8) {
          term.resize(80, 24);
          return;
        }
        fit.fit();
      } catch {
        try {
          term.resize(80, 24);
        } catch {
          /* ignore */
        }
      }
    };

    let roTimer: number | null = null;
    const scheduleFit = () => {
      if (roTimer != null) window.clearTimeout(roTimer);
      roTimer = window.setTimeout(() => {
        roTimer = null;
        requestAnimationFrame(safeFit);
      }, 50);
    };

    const onWindowResize = () => scheduleFit();
    const onBeamLayout = () => scheduleFit();

    window.addEventListener('resize', onWindowResize);
    window.addEventListener(BEAM_LAYOUT_CHANGED_EVENT, onBeamLayout);

    const ro = new ResizeObserver(() => {
      scheduleFit();
    });
    ro.observe(container);

    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        safeFit();
        focusTerm();
      });
    });
    termRef.current = term;

    const onContainerPointerDown = () => {
      focusTerm();
    };
    container.addEventListener('pointerdown', onContainerPointerDown);

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${protocol}//${window.location.host}${wsUrl}`;
    /** Populated after the deferred connect runs; null until then. */
    let ws: WebSocket | null = null;
    /**
     * Defer `new WebSocket` so React 18 Strict Mode can cancel the first scheduled connect on teardown.
     * Without this, cleanup closes a CONNECTING socket and the browser logs
     * "WebSocket is closed before the connection is established" for every mount.
     */
    let connectTimer: number | null = window.setTimeout(() => {
      connectTimer = null;
      const socket = new WebSocket(url);
      ws = socket;
      socket.binaryType = 'arraybuffer';

      wsOpenedRef.current = false;

      socket.onopen = () => {
        wsOpenedRef.current = true;
        onOpenRef.current?.();
        safeFit();
        focusTerm();
        socket.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
      };

      socket.onmessage = (event: MessageEvent) => {
        if (event.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(event.data));
        }
      };

      socket.onclose = () => {
        if (wsOpenedRef.current) {
          term.write('\r\n\x1b[90m[session ended]\x1b[0m\r\n');
        }
        if (!closedByCleanup && !wsOpenedRef.current) {
          onConnectionFailedRef.current?.();
        }
        if (!closedByCleanup && wsOpenedRef.current) {
          onDisconnectRef.current?.();
        }
      };
    }, 0);

    const encoder = new TextEncoder();

    const dataDisposable = term.onData((data: string) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(encoder.encode(data));
      }
    });

    const binaryDisposable = term.onBinary((data: string) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        const buf = new Uint8Array(data.length);
        for (let i = 0; i < data.length; i++) {
          buf[i] = data.charCodeAt(i) & 0xff;
        }
        ws.send(buf);
      }
    });

    const resizeDisposable = term.onResize(({ cols, rows }) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }));
      }
    });

    return () => {
      closedByCleanup = true;
      if (connectTimer != null) {
        window.clearTimeout(connectTimer);
        connectTimer = null;
      }
      if (roTimer != null) window.clearTimeout(roTimer);
      window.removeEventListener('resize', onWindowResize);
      window.removeEventListener(BEAM_LAYOUT_CHANGED_EVENT, onBeamLayout);
      container.removeEventListener('pointerdown', onContainerPointerDown);
      ro.disconnect();
      dataDisposable.dispose();
      binaryDisposable.dispose();
      resizeDisposable.dispose();
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close();
      }
      term.dispose();
      termRef.current = null;
    };
  }, [wsUrl]);

  return (
    <div
      ref={containerRef}
      data-xterm-host=""
      className={cn(
        'box-border min-h-0 min-w-0 h-full w-full flex-1 cursor-text overflow-hidden',
        className,
      )}
      style={{
        backgroundColor: theme.background ?? '#f8f9fa',
      }}
    />
  );
});
