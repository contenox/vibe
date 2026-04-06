import type { FileTreeNode } from '@contenox/ui';

import type { FileResponse } from './types';

/** Map a flat directory listing to `FileTree` nodes (no nested children until loaded). */
export function toFileTreeNodes(entries: FileResponse[]): FileTreeNode[] {
  return entries.map(e => ({
    id: e.id,
    name: e.name ?? e.path.split('/').pop() ?? e.path,
    path: e.path,
    isDirectory: e.isDirectory === true,
  }));
}
