import type { ChainDefinition } from './types';

/** Parses active-plan `compiled_chain_json` into a ChainDefinition for workflow visualization. */
export function parseCompiledChainJSON(raw: string | undefined | null): ChainDefinition | null {
  if (!raw?.trim()) return null;
  try {
    const v = JSON.parse(raw) as unknown;
    if (!v || typeof v !== 'object') return null;
    const c = v as Partial<ChainDefinition>;
    if (!Array.isArray(c.tasks) || c.tasks.length === 0) return null;
    if (typeof (c as { id?: unknown }).id !== 'string') return null;
    return v as ChainDefinition;
  } catch {
    return null;
  }
}
