/** Heuristic: JSON files that are likely task chain documents (VFS path). */
export function isChainLikeVfsPath(path: string): boolean {
  const lower = path.toLowerCase();
  if (!lower.endsWith('.json')) return false;
  const base = lower.split('/').pop() || '';
  return base.includes('chain');
}
