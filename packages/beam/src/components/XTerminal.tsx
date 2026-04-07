import '@xterm/xterm/css/xterm.css';

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { useEffect, useRef } from 'react';
import { useXtermTheme } from '../lib/xtermTheme';
import { cn } from '../lib/utils';

export interface XTerminalProps {
  /** WebSocket URL path, e.g. "/api/terminal/sessions/{id}/ws" */
  wsUrl: string;
  /** Called when the WebSocket closes (session ended or connection lost). */
  onDisconnect?: () => void;
  className?: string;
}

export function XTerminal({ wsUrl, onDisconnect, className }: XTerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const theme = useXtermTheme();

  // Update theme in-place without recreating terminal
  useEffect(() => {
    if (termRef.current) {
      termRef.current.options.theme = theme;
    }
  }, [theme]);

  // Main effect: create terminal, connect WebSocket, wire I/O
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    // ── Terminal instance ──────────────────────────────────────────
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: 'var(--font-mono)',
      fontSize: 14,
      lineHeight: 1.2,
      theme,
      allowProposedApi: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container);

    // Defer initial fit so the container has its final size
    requestAnimationFrame(() => {
      try {
        fit.fit();
      } catch {
        /* container may not be visible yet */
      }
    });
    termRef.current = term;

    // ── WebSocket ─────────────────────────────────────────────────
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${protocol}//${window.location.host}${wsUrl}`;
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
      // Send initial terminal dimensions
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    };

    ws.onmessage = (event: MessageEvent) => {
      if (event.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(event.data));
      }
    };

    ws.onclose = () => {
      term.write('\r\n\x1b[90m[session ended]\x1b[0m\r\n');
      onDisconnect?.();
    };

    // ── Terminal input → WebSocket ────────────────────────────────
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

    // ── Resize: FitAddon → terminal.onResize → WebSocket ─────────
    const resizeDisposable = term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }));
      }
    });

    const ro = new ResizeObserver(() => {
      requestAnimationFrame(() => {
        try {
          fit.fit();
        } catch {
          /* container hidden */
        }
      });
    });
    ro.observe(container);

    // ── Cleanup ───────────────────────────────────────────────────
    return () => {
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
  }, [wsUrl]); // Recreate only when wsUrl changes; theme handled separately

  return (
    <div
      ref={containerRef}
      className={cn('h-full w-full min-h-0 overflow-hidden', className)}
    />
  );
}
