import { createContext, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode, createElement } from 'react';

import type { ChatContextArtifact } from '../types';

/**
 * An [ArtifactSource] is any UI surface that can contribute state to the next
 * turn. The canonical examples are:
 *
 *   - the currently-open file in the workspace editor (kind: `open_file`);
 *   - the last attached terminal output (kind: `terminal_output`);
 *   - a specific plan step the user invoked via `/plan N` (kind: `plan_step`).
 *
 * Sources are registered with the [ArtifactRegistry] on mount and unregistered
 * on unmount. The ChatPage iterates the registry at send time and concatenates
 * non-null collect() results into ChatContextPayload.artifacts.
 *
 * A source is a pure function of its current UI state — there is no "commit"
 * step. If the user's input is stale the composer's pending indicator reflects
 * the last committed snapshot, but `collect()` is called fresh on send.
 */
export interface ArtifactSource {
  /** Stable id. Two sources sharing an id replace each other. */
  readonly id: string;
  /** Human-readable label for the composer's pending-attachments indicator. */
  readonly label: string;
  /**
   * Current artifact. Null/undefined means "nothing to attach right now" —
   * the source is registered but currently inert (e.g. no file selected).
   */
  collect(): ChatContextArtifact | null | undefined;
}

/**
 * Mutable registry of [ArtifactSource]s. A single instance is provided to a
 * ChatPage via [ArtifactRegistryProvider]. Use [useArtifactSource] to register
 * a source from a component; use [useArtifactRegistry] to read at send time.
 */
export class ArtifactRegistry {
  private sources = new Map<string, ArtifactSource>();
  private listeners = new Set<() => void>();
  /**
   * Cached array for [useSyncExternalStore] via [useArtifactSources]. Must be
   * referentially stable across reads when registration is unchanged — a fresh
   * array every [list] call would make React think the store updated every
   * render (maximum update depth exceeded).
   */
  private listSnapshot: ArtifactSource[] = [];

  private rebuildListSnapshot() {
    this.listSnapshot = Array.from(this.sources.values());
  }

  /**
   * Register a source. Returns an unregister function so callers can wire it
   * into useEffect cleanup without tracking ids themselves.
   */
  register(source: ArtifactSource): () => void {
    this.sources.set(source.id, source);
    this.rebuildListSnapshot();
    this.emit();
    return () => {
      if (this.sources.get(source.id) === source) {
        this.sources.delete(source.id);
        this.rebuildListSnapshot();
        this.emit();
      }
    };
  }

  /** All currently-registered sources in registration order. */
  list(): ArtifactSource[] {
    return this.listSnapshot;
  }

  /**
   * Collect every source's current artifact. Sources returning null/undefined
   * are skipped. Order is registration order.
   */
  collect(): ChatContextArtifact[] {
    return this.collectWithSources().map((p) => p.artifact);
  }

  /**
   * Like [collect] but returns each artifact paired with its source. Lets
   * callers downstream of one collect() call partition by source identity
   * (e.g. "render slash-armed artifacts inline; do not render sticky ones
   * because their owning panel already shows them").
   *
   * Important: each source's `collect()` is invoked exactly once per call,
   * because one-shot sources may unregister themselves as a side-effect.
   */
  collectWithSources(): Array<{ source: ArtifactSource; artifact: ChatContextArtifact }> {
    const out: Array<{ source: ArtifactSource; artifact: ChatContextArtifact }> = [];
    for (const source of this.sources.values()) {
      const artifact = source.collect();
      if (artifact) out.push({ source, artifact });
    }
    return out;
  }

  /** Subscribe to registration/unregistration events. */
  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  private emit() {
    for (const listener of this.listeners) listener();
  }
}

const ArtifactRegistryContext = createContext<ArtifactRegistry | null>(null);

/**
 * Provides an [ArtifactRegistry] to descendants. Each ChatPage owns one
 * instance; admin pages that don't participate in the canvas vision don't
 * need it.
 */
export function ArtifactRegistryProvider({ children }: { children: ReactNode }) {
  const registry = useMemo(() => new ArtifactRegistry(), []);
  return createElement(ArtifactRegistryContext.Provider, { value: registry }, children);
}

/**
 * Read the registry. Throws when used outside an [ArtifactRegistryProvider]
 * so misconfigured mounts fail loudly instead of silently skipping context.
 */
export function useArtifactRegistry(): ArtifactRegistry {
  const reg = useContext(ArtifactRegistryContext);
  if (!reg) {
    throw new Error('useArtifactRegistry must be used inside an ArtifactRegistryProvider');
  }
  return reg;
}

/**
 * Register `source` with the active registry for the lifetime of the calling
 * component. Pass `null` to skip registration (e.g. when the surface is
 * conditionally rendered). The source identity is compared by reference, so
 * re-render-stable objects avoid register/unregister churn.
 */
export function useArtifactSource(source: ArtifactSource | null) {
  const registry = useArtifactRegistry();
  useEffect(() => {
    if (!source) return undefined;
    return registry.register(source);
  }, [registry, source]);
}

/**
 * Snapshot of the currently-registered sources, re-rendering when the set
 * changes. Use in the composer to render the pending-attachments indicator.
 */
export function useArtifactSources(): ArtifactSource[] {
  const registry = useArtifactRegistry();
  return useSyncExternalStore(
    (cb) => registry.subscribe(cb),
    () => registry.list(),
    () => [],
  );
}
