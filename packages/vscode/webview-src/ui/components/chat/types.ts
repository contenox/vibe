import type { ReactNode } from "react";

export type ChatRole = "user" | "assistant" | "system" | "tool";

export type PendingApproval = {
  approvalId: string;
  hookName: string;
  toolName: string;
  args: Record<string, unknown>;
  diff: string;
};

export type ChatMessageBaseProps = {
  role: ChatRole;
  /** Shown in the role badge when provided */
  roleLabel: ReactNode;
  /** Main bubble content */
  children: ReactNode;
  /** Optional avatar; default is a letter chip by role */
  avatar?: ReactNode;
  /** Shown next to role badge */
  timestamp?: ReactNode;
  /** Wraps timestamp in Tooltip when set */
  timestampTooltip?: string;
  isLatest?: boolean;
  /** Shown when `isLatest` (e.g. translated “Latest”) */
  latestLabel?: ReactNode;
  /** Extra badge shown next to the role/timestamp badges (e.g. turn state: running/cancelled/failed). */
  statusBadge?: ReactNode;
  /** Ring highlight when latest (default true) */
  highlightLatest?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  /**
   * Whether the per-message Hide/Show collapse affordance is shown at all.
   * Default true, preserving every existing consumer's behavior. Set false
   * for transcript surfaces where plain messages are never collapsible —
   * only thought blocks / tool detail collapse — so no toggle is rendered
   * and the body always renders open.
   */
  collapsible?: boolean;
  /** Error panel below body */
  error?: ReactNode;
  onRetry?: () => void;
  retryLabel?: ReactNode;
  collapseToggleLabel?: { open: ReactNode; closed: ReactNode };
  /** Extra row under bubble (e.g. copy / share) */
  secondaryActions?: ReactNode;
  /** When set, shows a built-in copy control */
  copyText?: string;
  copyLabel?: ReactNode;
  copiedLabel?: ReactNode;
  className?: string;
  /** Accessible label for the message article */
  "aria-label"?: string;
  /** Bubble (chat) vs transcript (workbench / full-width blocks). Default bubble. */
  appearance?: "bubble" | "transcript";
};
