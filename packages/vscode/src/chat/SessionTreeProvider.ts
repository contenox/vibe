import * as vscode from "vscode";
import { AcpChatClient } from "../acp/AcpChatClient";
import { AcpSessionInfo } from "../acp/types";

type SessionTreeNode =
  | { kind: "session"; session: AcpSessionInfo }
  | { kind: "message"; label: string; description?: string };

// Sessions over ACP (vscode-implementation-plan.md Phase 1 §"sessions over
// ACP"): backed by session/list instead of the bespoke bridge, so sessions
// created via session/new are durable and show up here across a webview
// reload, not just for the extension-host lifetime that minted them.
export class SessionTreeProvider implements vscode.TreeDataProvider<SessionTreeNode>, vscode.Disposable {
  private readonly changeEmitter = new vscode.EventEmitter<SessionTreeNode | undefined | null | void>();
  public readonly onDidChangeTreeData = this.changeEmitter.event;

  public constructor(private readonly acpClient: AcpChatClient) {}

  public refresh(): void {
    this.changeEmitter.fire();
  }

  public async getChildren(element?: SessionTreeNode): Promise<SessionTreeNode[]> {
    if (element) {
      return [];
    }
    try {
      const result = await this.acpClient.listSessions();
      if (result.sessions.length === 0) {
        return [{ kind: "message", label: "No sessions yet. Start chatting in the Chat view." }];
      }
      return result.sessions.map((session) => ({ kind: "session" as const, session }));
    } catch (error) {
      return [{ kind: "message", label: "Contenox runtime unavailable", description: errorMessage(error) }];
    }
  }

  public getTreeItem(element: SessionTreeNode): vscode.TreeItem {
    if (element.kind === "message") {
      const item = new vscode.TreeItem(element.label, vscode.TreeItemCollapsibleState.None);
      item.description = element.description;
      return item;
    }

    const { session } = element;
    const item = new vscode.TreeItem(session.title || session.sessionId, vscode.TreeItemCollapsibleState.None);
    item.id = session.sessionId;
    item.description = sessionDescription(session);
    item.tooltip = `${sessionTooltip(session)}\n\nClick to resume in Chat.`;
    item.contextValue = "contenoxSession";
    item.iconPath = new vscode.ThemeIcon("comment-discussion");
    item.command = {
      command: "contenox.openSession",
      title: "Resume in Chat",
      arguments: [session.sessionId],
    };
    return item;
  }

  public dispose(): void {
    this.changeEmitter.dispose();
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function sessionDescription(session: AcpSessionInfo): string {
  return relativeTime(session.updatedAt ?? undefined) ?? "";
}

function sessionTooltip(session: AcpSessionInfo): string {
  const lines = [session.title || session.sessionId, `ID: ${session.sessionId}`, `Cwd: ${session.cwd}`];
  if (session.updatedAt) {
    lines.push(`Updated: ${session.updatedAt}`);
  }
  return lines.join("\n");
}

function relativeTime(value: string | undefined): string | undefined {
  if (!value) {
    return undefined;
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return undefined;
  }
  const diffMs = Date.now() - date.getTime();
  const abs = Math.abs(diffMs);
  const minute = 60 * 1000;
  const hour = 60 * minute;
  const day = 24 * hour;
  if (abs < minute) {
    return "just now";
  }
  if (abs < hour) {
    const minutes = Math.round(abs / minute);
    return `${minutes}m ago`;
  }
  if (abs < day) {
    const hours = Math.round(abs / hour);
    return `${hours}h ago`;
  }
  const days = Math.round(abs / day);
  return `${days}d ago`;
}
