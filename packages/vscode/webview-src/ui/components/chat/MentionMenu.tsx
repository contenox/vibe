// "@" mention picker (vscode-implementation-plan.md Phase 3): lists
// workspace files and symbols matching the query typed after the last "@" in
// the composer. Live results come from the host (vscode.workspace.findFiles /
// vscode.executeWorkspaceSymbolProvider) -- this component only renders them.
export type ChatMentionFilePick = { uri: string; name: string; description: string };
export type ChatMentionSymbolPick = { uri: string; name: string; description: string; line: number };

export function MentionMenu({
  files,
  symbols,
  onSelectFile,
  onSelectSymbol,
}: {
  files: ChatMentionFilePick[];
  symbols: ChatMentionSymbolPick[];
  onSelectFile: (pick: ChatMentionFilePick) => void;
  onSelectSymbol: (pick: ChatMentionSymbolPick) => void;
}) {
  if (files.length === 0 && symbols.length === 0) return null;
  return (
    <div
      aria-label="Attach file or symbol"
      className="border-surface-200 dark:border-dark-surface-700 bg-surface-50 dark:bg-dark-surface-200 mb-2 max-h-48 overflow-y-auto rounded-md border shadow-sm"
      role="listbox">
      {files.map(pick => (
        <button
          className="hover:bg-surface-100 dark:hover:bg-dark-surface-300 flex w-full flex-col items-start gap-0.5 px-3 py-1.5 text-left"
          key={`file-${pick.uri}`}
          onClick={() => onSelectFile(pick)}
          role="option"
          type="button">
          <span className="text-text dark:text-dark-text text-sm font-medium">{pick.name}</span>
          <span className="text-text-muted dark:text-dark-text-muted text-xs">{pick.description}</span>
        </button>
      ))}
      {symbols.map(pick => (
        <button
          className="hover:bg-surface-100 dark:hover:bg-dark-surface-300 flex w-full flex-col items-start gap-0.5 px-3 py-1.5 text-left"
          key={`symbol-${pick.uri}-${pick.line}`}
          onClick={() => onSelectSymbol(pick)}
          role="option"
          type="button">
          <span className="text-text dark:text-dark-text text-sm font-medium">@{pick.name}</span>
          <span className="text-text-muted dark:text-dark-text-muted text-xs">{pick.description}</span>
        </button>
      ))}
    </div>
  );
}
