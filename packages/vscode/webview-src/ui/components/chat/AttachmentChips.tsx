import { FileText, Image as ImageIcon, X } from "lucide-react";

// Phase 3 "real context attachment" (vscode-implementation-plan.md §5):
// visible, removable chips for exactly what will be sent as attachments --
// replaces the old invisible/expiring pendingContext side-channel. Purely a
// display component; ChatSurface.tsx owns the attachment list state.
export type ChatAttachmentChip = {
  id: string;
  kind: string;
  name: string;
  description?: string;
};

export function AttachmentChips({
  attachments,
  onRemove,
}: {
  attachments: ChatAttachmentChip[];
  onRemove: (id: string) => void;
}) {
  if (attachments.length === 0) return null;
  return (
    <div className="mb-2 flex flex-wrap gap-1.5" role="list" aria-label="Attached context">
      {attachments.map(attachment => (
        <span
          className="border-surface-300 bg-surface-100 text-text dark:border-dark-surface-600 dark:bg-dark-surface-300 dark:text-dark-text inline-flex max-w-[16rem] items-center gap-1 rounded-full border px-2 py-0.5 text-xs"
          key={attachment.id}
          role="listitem"
          title={attachment.description ?? attachment.name}>
          {attachment.kind === "image" ? (
            <ImageIcon aria-hidden className="h-3 w-3 shrink-0" />
          ) : (
            <FileText aria-hidden className="h-3 w-3 shrink-0" />
          )}
          <span className="truncate">{attachment.name}</span>
          <button
            aria-label={`Remove ${attachment.name}`}
            className="ml-0.5 shrink-0 rounded-full opacity-70 hover:opacity-100"
            onClick={() => onRemove(attachment.id)}
            type="button">
            <X className="h-3 w-3" />
          </button>
        </span>
      ))}
    </div>
  );
}
