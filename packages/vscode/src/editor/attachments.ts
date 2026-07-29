// Host-side resolution for Phase 3 "real context attachment"
// (vscode-implementation-plan.md §5): everything here needs vscode.workspace
// / vscode.window API the webview cannot reach directly, so the webview asks
// for these over the wire (webviewProtocol's searchFiles/searchSymbols/
// attachFile/attachSymbol/attachSelection/attachActiveFile) and gets back a
// fully-hydrated, frozen WireAttachment -- "what you attached" is exactly
// what gets sent later, independent of what the editor looks like at send
// time (see AcpChatClient.buildPromptBlocks).
import * as path from "node:path";
import * as vscode from "vscode";
import { EditorContextAttachment } from "../bridge/protocol";
import {
  WireAttachment,
  WireAttachmentKind,
  WireFilePick,
  WireSymbolPick,
} from "../chat/webviewProtocol";

// Generous but bounded -- keeps a single huge file from silently blowing the
// prompt token budget (vscode-implementation-plan.md Phase 1's "must stay
// usable against Ollama and other local models" cost concern applies here
// too, not just to the chain doctrine).
const maxAttachmentChars = 50_000;
const symbolContextLines = 20;

let attachmentSeq = 0;
function nextAttachmentId(): string {
  attachmentSeq += 1;
  return `att-${Date.now()}-${attachmentSeq}`;
}

export function truncateAttachmentText(value: string, max = maxAttachmentChars): string {
  if (value.length <= max) {
    return value;
  }
  return `${value.slice(0, max)}\n\n[Contenox truncated this attachment at ${max} characters.]`;
}

export async function searchWorkspaceFiles(query: string, maxResults = 20): Promise<WireFilePick[]> {
  const uris = await vscode.workspace.findFiles("**/*", "**/{node_modules,.git,dist,out,.vscode-test}/**", 400);
  const needle = query.trim().toLowerCase();
  const scored = uris
    .map((uri) => ({ uri, rel: workspaceRelativePath(uri) }))
    .filter((entry) => !needle || entry.rel.toLowerCase().includes(needle))
    .slice(0, maxResults);
  return scored.map(({ uri, rel }) => ({
    uri: uri.toString(),
    name: path.basename(rel),
    description: rel,
  }));
}

export async function searchWorkspaceSymbols(query: string, maxResults = 20): Promise<WireSymbolPick[]> {
  const symbols = await vscode.commands.executeCommand<vscode.SymbolInformation[] | undefined>(
    "vscode.executeWorkspaceSymbolProvider",
    query,
  );
  if (!symbols) {
    return [];
  }
  return symbols.slice(0, maxResults).map((symbol) => {
    const rel = workspaceRelativePath(symbol.location.uri);
    const line = symbol.location.range.start.line;
    const label = symbol.containerName ? `${symbol.containerName}.${symbol.name}` : symbol.name;
    return {
      uri: symbol.location.uri.toString(),
      name: label,
      description: `${rel}:${line + 1}`,
      line,
    };
  });
}

export async function resolveFileAttachment(uriString: string): Promise<WireAttachment | undefined> {
  const uri = vscode.Uri.parse(uriString);
  try {
    const doc = await vscode.workspace.openTextDocument(uri);
    const rel = workspaceRelativePath(uri);
    return {
      id: nextAttachmentId(),
      kind: "file",
      name: path.basename(rel),
      description: rel,
      uri: uri.toString(),
      languageId: doc.languageId,
      text: truncateAttachmentText(doc.getText()),
    };
  } catch {
    return undefined;
  }
}

export async function resolveSymbolAttachment(
  uriString: string,
  line: number,
  name: string,
): Promise<WireAttachment | undefined> {
  const uri = vscode.Uri.parse(uriString);
  try {
    const doc = await vscode.workspace.openTextDocument(uri);
    const start = Math.max(0, line - symbolContextLines);
    const end = Math.min(doc.lineCount - 1, line + symbolContextLines);
    const range = new vscode.Range(start, 0, end, doc.lineAt(end).text.length);
    const rel = workspaceRelativePath(uri);
    return {
      id: nextAttachmentId(),
      kind: "symbol",
      name,
      description: `${rel}:${line + 1}`,
      uri: uri.with({ fragment: `L${line + 1}` }).toString(),
      languageId: doc.languageId,
      text: truncateAttachmentText(doc.getText(range)),
    };
  } catch {
    return undefined;
  }
}

export function resolveSelectionAttachment(): WireAttachment | undefined {
  const editor = vscode.window.activeTextEditor;
  if (!editor || editor.selection.isEmpty) {
    return undefined;
  }
  const rel = workspaceRelativePath(editor.document.uri);
  const start = editor.selection.start.line + 1;
  const end = editor.selection.end.line + 1;
  const lineRange = start === end ? `${start}` : `${start}-${end}`;
  return {
    id: nextAttachmentId(),
    kind: "selection",
    name: `${path.basename(rel)}:${lineRange}`,
    description: `${rel}:${lineRange}`,
    uri: editor.document.uri.with({ fragment: `L${lineRange}` }).toString(),
    languageId: editor.document.languageId,
    text: truncateAttachmentText(editor.document.getText(editor.selection)),
  };
}

export function resolveActiveFileAttachment(): WireAttachment | undefined {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    return undefined;
  }
  const rel = workspaceRelativePath(editor.document.uri);
  return {
    id: nextAttachmentId(),
    kind: "active_file",
    name: path.basename(rel),
    description: rel,
    uri: editor.document.uri.toString(),
    languageId: editor.document.languageId,
    text: truncateAttachmentText(editor.document.getText()),
  };
}

// toWireAttachments adapts the legacy EditorContextAttachment[] shape (still
// produced by collectEditorContext/collectGitChangeContext for the editor
// commands: Ask/Fix Selection, Fix/Explain Diagnostics, Review Changes, Draft
// Commit Message) into structured, chip-rendered WireAttachment[] -- these
// commands must attach structured context now, not paste text into the
// composer (vscode-implementation-plan.md Phase 3).
export function toWireAttachments(context: readonly EditorContextAttachment[]): WireAttachment[] {
  return context.map((item) => {
    const kind = mapLegacyKind(item.kind);
    const rel = item.uri ? workspaceRelativePath(vscode.Uri.parse(item.uri)) : undefined;
    return {
      id: nextAttachmentId(),
      kind,
      name: legacyAttachmentName(kind, rel),
      description: rel,
      uri: item.uri,
      languageId: item.languageId,
      text: truncateAttachmentText(item.content),
    } satisfies WireAttachment;
  });
}

function legacyAttachmentName(kind: WireAttachmentKind, rel: string | undefined): string {
  switch (kind) {
    case "diagnostics":
      return rel ? `Diagnostics: ${path.basename(rel)}` : "Diagnostics";
    case "git_diff":
      return "Git changes";
    case "active_file":
      return rel ? path.basename(rel) : "Active file";
    case "selection":
      return rel ? `Selection: ${path.basename(rel)}` : "Selection";
    default:
      return rel ? path.basename(rel) : kind;
  }
}

function mapLegacyKind(kind: string): WireAttachmentKind {
  switch (kind) {
    case "selection":
      return "selection";
    case "active_file":
      return "active_file";
    case "diagnostics":
      return "diagnostics";
    case "git_changes":
      return "git_diff";
    default:
      return "active_file";
  }
}

function workspaceRelativePath(uri: vscode.Uri): string {
  const folder = vscode.workspace.getWorkspaceFolder(uri);
  if (folder) {
    return path.relative(folder.uri.fsPath, uri.fsPath) || path.basename(uri.fsPath);
  }
  return uri.fsPath || uri.toString();
}
