/**
 * In-memory terminal session for the current tab. Survives React remounts
 * (Strict Mode double-mount, workspace layout branch swaps, parent re-mounts) so we
 * do not repeat POST /api/terminal/sessions, GET session, and WebSocket setup
 * on every mount — that churn looked like the terminal "closing and reopening".
 *
 * localStorage still persists the id across reloads; this layer only dedupes
 * within the SPA session. Cleared when the user closes the terminal or we
 * explicitly drop the session.
 */
export type SharedTerminalSession = { sessionId: string; wsUrl: string };

let shared: SharedTerminalSession | null = null;

let inflightCreate: Promise<SharedTerminalSession> | null = null;

export function getSharedTerminalSession(): SharedTerminalSession | null {
  return shared;
}

export function setSharedTerminalSession(next: SharedTerminalSession | null): void {
  shared = next;
}

/**
 * Returns the cached session if present; otherwise runs `create` once and caches
 * the result. Concurrent callers await the same in-flight promise.
 */
export function reuseOrCreateTerminalSession(
  create: () => Promise<SharedTerminalSession>,
): Promise<SharedTerminalSession> {
  if (shared) return Promise.resolve(shared);
  if (!inflightCreate) {
    inflightCreate = create()
      .then((s) => {
        shared = s;
        return s;
      })
      .finally(() => {
        inflightCreate = null;
      });
  }
  return inflightCreate;
}
