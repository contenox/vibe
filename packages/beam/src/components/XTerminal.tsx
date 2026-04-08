import '@xterm/xterm/css/xterm.css';

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { useEffect, useRef, useLayoutEffect } from 'react';
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
  className?: string;
}

export function XTerminal({ wsUrl, onOpen, onDisconnect, className }: XTerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const onDisconnectRef = useRef(onDisconnect);
  const onOpenRef = useRef(onOpen);
  const theme = useXtermTheme();
  const themeRef = useRef(theme);
  themeRef.current = theme;

  useLayoutEffect(() => {
    onDisconnectRef.current = onDisconnect;
    onOpenRef.current = onOpen;
  });

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
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
      onOpenRef.current?.();
      safeFit();
      focusTerm();
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    };

    ws.onmessage = (event: MessageEvent) => {
      if (event.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(event.data));
      }
    };

    ws.onclose = () => {
      term.write('\r\n\x1b[90m[session ended]\x1b[0m\r\n');
      if (!closedByCleanup) {
        onDisconnectRef.current?.();
      }
    };

    const encoder = new TextEncoder();

    const dataDisposable = term.onData((data: string) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(encoder.encode(data));
      }
    });

    const binaryDisposable = term.onBinary((data: string) => {
      if (ws.readyState === WebSocket.OPEN) {
        const buf = new Uint8Array(data.length);
        for (let i = 0; i < data.length; i++) {
          buf[i] = data.charCodeAt(i) & 0xff;
        }
        ws.send(buf);
      }
    });

    const resizeDisposable = term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }));
      }
    });

    return () => {
      closedByCleanup = true;
      if (roTimer != null) window.clearTimeout(roTimer);
      window.removeEventListener('resize', onWindowResize);
      window.removeEventListener(BEAM_LAYOUT_CHANGED_EVENT, onBeamLayout);
      container.removeEventListener('pointerdown', onContainerPointerDown);
      ro.disconnect();
      dataDisposable.dispose();
      binaryDisposable.dispose();
      resizeDisposable.dispose();
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
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
}
