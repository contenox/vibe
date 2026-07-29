import * as path from "node:path";
import * as vscode from "vscode";
import { ToolCallEvent } from "../bridge/protocol";
import { TelemetryLogger } from "../logging/telemetry";

interface DiffContent {
  title: string;
  before: string;
  after: string;
  filePath?: string;
  languageId?: string;
}

export interface StoredDiff {
  id: string;
  title: string;
  left: vscode.Uri;
  right: vscode.Uri;
  fileUri?: vscode.Uri;
}

export interface OpenDiffArgs {
  id?: string;
  title?: string;
  left?: vscode.Uri | string;
  right?: vscode.Uri | string;
  before?: string;
  after?: string;
  filePath?: string;
  languageId?: string;
}

export class DiffStore implements vscode.TextDocumentContentProvider, vscode.Disposable {
  private readonly changeEmitter = new vscode.EventEmitter<vscode.Uri>();
  private readonly contents = new Map<string, string>();
  private sequence = 0;

  public readonly onDidChange = this.changeEmitter.event;

  public constructor(private readonly telemetry: TelemetryLogger) {}

  public registerToolDiff(event: ToolCallEvent): StoredDiff | undefined {
    if (event.diffOld === undefined && event.diffNew === undefined) {
      return undefined;
    }
    return this.register({
      title: toolDiffTitle(event),
      before: event.diffOld ?? "",
      after: event.diffNew ?? "",
      filePath: event.diffPath,
    });
  }

  public register(diff: DiffContent): StoredDiff {
    const id = `${Date.now()}-${++this.sequence}`;
    const fileName = safeFileName(diff.filePath || "contenox-diff.txt");
    const left = vscode.Uri.from({
      scheme: "contenox-diff",
      path: `/${id}/before/${fileName}`,
      query: diff.languageId ? `languageId=${encodeURIComponent(diff.languageId)}` : "",
    });
    const right = vscode.Uri.from({
      scheme: "contenox-diff",
      path: `/${id}/after/${fileName}`,
      query: diff.languageId ? `languageId=${encodeURIComponent(diff.languageId)}` : "",
    });
    this.contents.set(left.toString(), diff.before);
    this.contents.set(right.toString(), diff.after);

    const stored: StoredDiff = {
      id,
      title: diff.title,
      left,
      right,
      fileUri: fileUriFromPath(diff.filePath),
    };
    this.telemetry.event("diff.registered", {
      id,
      title: diff.title,
      beforeChars: diff.before.length,
      afterChars: diff.after.length,
      filePath: diff.filePath,
    });
    return stored;
  }

  public provideTextDocumentContent(uri: vscode.Uri): string {
    return this.contents.get(uri.toString()) ?? "";
  }

  public async open(args: OpenDiffArgs | StoredDiff): Promise<void> {
    const normalized = this.normalizeOpenArgs(args);
    if (!normalized) {
      vscode.window.showWarningMessage("No Contenox diff is available to open.");
      return;
    }
    this.telemetry.event("diff.open", {
      id: normalized.id,
      title: normalized.title,
      leftScheme: normalized.left.scheme,
      rightScheme: normalized.right.scheme,
    });
    await vscode.commands.executeCommand("vscode.diff", normalized.left, normalized.right, normalized.title);
  }

  public dispose(): void {
    this.changeEmitter.dispose();
    this.contents.clear();
  }

  private normalizeOpenArgs(args: OpenDiffArgs | StoredDiff): StoredDiff | undefined {
    if (isStoredDiff(args)) {
      return args;
    }
    if (args.left && args.right) {
      return {
        id: args.id ?? "external",
        title: args.title ?? "Contenox Diff",
        left: uriFromArg(args.left),
        right: uriFromArg(args.right),
        fileUri: fileUriFromPath(args.filePath),
      };
    }
    if (args.before !== undefined || args.after !== undefined) {
      return this.register({
        title: args.title ?? "Contenox Diff",
        before: args.before ?? "",
        after: args.after ?? "",
        filePath: args.filePath,
        languageId: args.languageId,
      });
    }
    return undefined;
  }
}

function isStoredDiff(value: OpenDiffArgs | StoredDiff): value is StoredDiff {
  return value.left instanceof vscode.Uri && value.right instanceof vscode.Uri && typeof value.title === "string";
}

function uriFromArg(value: vscode.Uri | string): vscode.Uri {
  return value instanceof vscode.Uri ? value : vscode.Uri.parse(value);
}

function fileUriFromPath(value: string | undefined): vscode.Uri | undefined {
  if (!value) {
    return undefined;
  }
  const workspace = vscode.workspace.workspaceFolders?.find((folder) => folder.uri.scheme === "file");
  const absolute = path.isAbsolute(value) ? value : workspace ? path.join(workspace.uri.fsPath, value) : undefined;
  return absolute ? vscode.Uri.file(absolute) : undefined;
}

function toolDiffTitle(event: ToolCallEvent): string {
  const file = event.diffPath ? path.basename(event.diffPath) : "";
  const tool = event.title || event.toolName || event.taskId || "Tool diff";
  return file ? `${tool}: ${file}` : tool;
}

function safeFileName(value: string): string {
  const base = path.basename(value).replace(/[^A-Za-z0-9._-]/g, "_");
  return base || "contenox-diff.txt";
}
