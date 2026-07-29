import * as vscode from "vscode";
import { EditorContextAttachment } from "../bridge/protocol";

export interface EditorContextOptions {
  includeSelection: boolean;
  includeActiveFile: boolean;
  includeDiagnostics: boolean;
  diagnostics?: readonly vscode.Diagnostic[];
}

export async function collectEditorContext(options: EditorContextOptions): Promise<EditorContextAttachment[]> {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    return [];
  }
  const out: EditorContextAttachment[] = [];
  if (options.includeSelection && !editor.selection.isEmpty) {
    out.push({
      kind: "selection",
      uri: editor.document.uri.toString(),
      languageId: editor.document.languageId,
      content: editor.document.getText(editor.selection),
    });
  }
  if (options.includeActiveFile) {
    out.push({
      kind: "active_file",
      uri: editor.document.uri.toString(),
      languageId: editor.document.languageId,
      content: editor.document.getText(),
    });
  }
  if (options.includeDiagnostics) {
    const diagnostics = options.diagnostics ?? vscode.languages.getDiagnostics(editor.document.uri);
    if (diagnostics.length > 0) {
      out.push({
        kind: "diagnostics",
        uri: editor.document.uri.toString(),
        languageId: editor.document.languageId,
        content: formatDiagnostics(diagnostics),
      });
    }
  }
  return out;
}

export function activeSelectionText(): string | undefined {
  const editor = vscode.window.activeTextEditor;
  if (!editor || editor.selection.isEmpty) {
    return undefined;
  }
  return editor.document.getText(editor.selection);
}

export function activeDiagnostics(): readonly vscode.Diagnostic[] {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    return [];
  }
  return vscode.languages.getDiagnostics(editor.document.uri);
}

export function formatDiagnostics(diagnostics: readonly vscode.Diagnostic[]): string {
  return diagnostics
    .slice(0, 100)
    .map((diagnostic) => {
      const start = diagnostic.range.start;
      const end = diagnostic.range.end;
      const severity = vscode.DiagnosticSeverity[diagnostic.severity] ?? "Diagnostic";
      const source = diagnostic.source ? ` ${diagnostic.source}` : "";
      return `${severity}${source} at ${start.line + 1}:${start.character + 1}-${end.line + 1}:${end.character + 1} - ${diagnostic.message}`;
    })
    .join("\n");
}

export function contextSummary(context: readonly EditorContextAttachment[]): Record<string, number> {
  return context.reduce<Record<string, number>>((acc, item) => {
    acc[item.kind] = (acc[item.kind] ?? 0) + 1;
    return acc;
  }, {});
}
