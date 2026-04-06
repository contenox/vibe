import { forwardRef, useCallback, useState } from "react";
import { ChevronRight, File, Folder, FolderOpen } from "lucide-react";
import { cn } from "../utils";

export interface FileTreeNode {
  /** Unique id for this node. */
  id: string;
  /** Display name. */
  name: string;
  /** Full path (used for context, not rendering). */
  path?: string;
  /** Whether this node is a directory. */
  isDirectory?: boolean;
  /** Nested children (only for directories). */
  children?: FileTreeNode[];
}

export interface FileTreeProps extends React.HTMLAttributes<HTMLDivElement> {
  /** The tree data. */
  nodes: FileTreeNode[];
  /** Currently selected node id. */
  selectedId?: string | null;
  /** Called when a file or folder is clicked. */
  onNodeSelect?: (node: FileTreeNode) => void;
  /**
   * `expand` (default): directory row toggles open/closed and fires `onNodeSelect`.
   * `navigate`: directory row only calls `onNodeSelect` (e.g. change cwd); use the chevron to expand/collapse when children exist.
   */
  directoryClickMode?: "expand" | "navigate";
  /** Set of node ids that are initially expanded. Defaults to all directories expanded. */
  defaultExpanded?: Set<string>;
  /** Depth indentation in px. */
  indent?: number;
}

export const FileTree = forwardRef<HTMLDivElement, FileTreeProps>(
  function FileTree(
    {
      className,
      nodes,
      selectedId,
      onNodeSelect,
      directoryClickMode = "expand",
      defaultExpanded,
      indent = 16,
      ...props
    },
    ref,
  ) {
    return (
      <div
        ref={ref}
        role="tree"
        className={cn("text-sm select-none", className)}
        {...props}
      >
        {nodes.map((node) => (
          <FileTreeItem
            key={node.id}
            node={node}
            depth={0}
            indent={indent}
            selectedId={selectedId}
            onNodeSelect={onNodeSelect}
            directoryClickMode={directoryClickMode}
            defaultExpanded={defaultExpanded}
          />
        ))}
      </div>
    );
  },
);

/* ------------------------------------------------------------------ */

interface FileTreeItemProps {
  node: FileTreeNode;
  depth: number;
  indent: number;
  selectedId?: string | null;
  onNodeSelect?: (node: FileTreeNode) => void;
  directoryClickMode: "expand" | "navigate";
  defaultExpanded?: Set<string>;
}

function FileTreeItem({
  node,
  depth,
  indent,
  selectedId,
  onNodeSelect,
  directoryClickMode,
  defaultExpanded,
}: FileTreeItemProps) {
  const [expanded, setExpanded] = useState(
    () => defaultExpanded?.has(node.id) ?? (node.isDirectory === true),
  );

  const isSelected = selectedId === node.id;

  const toggleExpand = useCallback(() => {
    setExpanded((v) => !v);
  }, []);

  const handleRowClick = useCallback(() => {
    if (node.isDirectory && directoryClickMode === "expand") {
      setExpanded((v) => !v);
    }
    onNodeSelect?.(node);
  }, [node, onNodeSelect, directoryClickMode]);

  const handleChevronClick = useCallback(
    (e: React.MouseEvent) => {
      e.stopPropagation();
      toggleExpand();
    },
    [toggleExpand],
  );

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        handleRowClick();
      }
      if (node.isDirectory) {
        if (e.key === "ArrowRight" && !expanded) {
          e.preventDefault();
          setExpanded(true);
        }
        if (e.key === "ArrowLeft" && expanded) {
          e.preventDefault();
          setExpanded(false);
        }
      }
    },
    [handleRowClick, node.isDirectory, expanded],
  );

  const rowShellClass = cn(
    "flex w-full items-center gap-1.5 rounded px-2 py-1 text-left",
    "text-text dark:text-dark-text",
    "hover:bg-surface-100 dark:hover:bg-dark-surface-200",
    isSelected &&
      "bg-primary-50 text-primary-700 dark:bg-dark-primary-900 dark:text-dark-primary-300",
  );

  return (
    <div role="treeitem" aria-expanded={node.isDirectory ? expanded : undefined}>
      {node.isDirectory && directoryClickMode === "navigate" ? (
        <div className={rowShellClass} style={{ paddingLeft: depth * indent + 8 }}>
          <button
            type="button"
            className="text-text-muted dark:text-dark-text-muted hover:bg-surface-200 dark:hover:bg-dark-surface-300 inline-flex shrink-0 items-center justify-center rounded p-0.5"
            onClick={handleChevronClick}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse" : "Expand"}
          >
            <ChevronRight
              className={cn(
                "h-3.5 w-3.5 transition-transform",
                expanded && "rotate-90",
              )}
            />
          </button>
          <button
            type="button"
            onClick={() => onNodeSelect?.(node)}
            className="flex min-w-0 flex-1 items-center gap-1.5 rounded py-0.5 text-left hover:bg-transparent"
          >
            {expanded ? (
              <FolderOpen className="h-4 w-4 shrink-0 text-warning dark:text-dark-warning" />
            ) : (
              <Folder className="h-4 w-4 shrink-0 text-warning dark:text-dark-warning" />
            )}
            <span className="truncate font-mono text-xs">{node.name}</span>
          </button>
        </div>
      ) : (
        <button
          type="button"
          onClick={handleRowClick}
          onKeyDown={handleKeyDown}
          className={rowShellClass}
          style={{ paddingLeft: depth * indent + 8 }}
        >
          {node.isDirectory ? (
            <>
              <ChevronRight
                className={cn(
                  "h-3.5 w-3.5 shrink-0 transition-transform",
                  "text-text-muted dark:text-dark-text-muted",
                  expanded && "rotate-90",
                )}
              />
              {expanded ? (
                <FolderOpen className="h-4 w-4 shrink-0 text-warning dark:text-dark-warning" />
              ) : (
                <Folder className="h-4 w-4 shrink-0 text-warning dark:text-dark-warning" />
              )}
            </>
          ) : (
            <>
              <span className="w-3.5 shrink-0" />
              <File className="h-4 w-4 shrink-0 text-text-muted dark:text-dark-text-muted" />
            </>
          )}
          <span className="truncate font-mono text-xs">{node.name}</span>
        </button>
      )}

      {node.isDirectory && expanded && node.children && (
        <div role="group">
          {node.children.map((child) => (
            <FileTreeItem
              key={child.id}
              node={child}
              depth={depth + 1}
              indent={indent}
              selectedId={selectedId}
              onNodeSelect={onNodeSelect}
              directoryClickMode={directoryClickMode}
              defaultExpanded={defaultExpanded}
            />
          ))}
        </div>
      )}
    </div>
  );
}
