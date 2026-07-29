// The operator inbox (vscode-implementation-plan.md Phase 6): where
// walk-away lands when no panel is open. A tree of the durable ask queue
// (internal/services/hitlservice -- `contenox approvals`) and unsupervised
// mission reports (internal/services/operatorinbox -- `contenox inbox`,
// the same store `contenox inbox` and the terminal UI's enginebridge/inbox.go
// read), answerable without a live turn. Backed by CLI shell-outs
// (approvalsCli.ts) since neither store has a JSON mode or an ACP method --
// see that file's header for why that's a deliberate choice, not a stopgap.
import * as vscode from "vscode";
import {
  ackInboxItem,
  InboxRow,
  listInboxUnacked,
  listPendingApprovals,
  PendingAsk,
  respondApproval,
} from "../approval/approvalsCli";
import { ContenoxOutput } from "../logging/output";

export type InboxTreeNode =
  | { kind: "group"; id: "approvals" | "inbox"; label: string; count: number }
  | { kind: "approval"; ask: PendingAsk }
  | { kind: "inboxItem"; item: InboxRow }
  | { kind: "message"; label: string; description?: string };

export class InboxTreeProvider implements vscode.TreeDataProvider<InboxTreeNode>, vscode.Disposable {
  private readonly changeEmitter = new vscode.EventEmitter<InboxTreeNode | undefined | null | void>();
  public readonly onDidChangeTreeData = this.changeEmitter.event;
  private approvals: PendingAsk[] = [];
  private inboxItems: InboxRow[] = [];
  private lastError: string | undefined;
  private pollTimer: ReturnType<typeof setInterval> | undefined;
  private view: vscode.TreeView<InboxTreeNode> | undefined;

  public constructor(
    private readonly extensionUri: vscode.Uri,
    private readonly output: ContenoxOutput,
  ) {}

  public attachView(view: vscode.TreeView<InboxTreeNode>): void {
    this.view = view;
    this.updateBadge();
  }

  // startPolling refreshes the tree on an interval so a reopened panel's
  // badge and tree reflect asks answered from a terminal while VS Code
  // itself was closed (Phase 5/6's whole premise: the CLI is a peer of the
  // panel, not something only the panel need know about).
  public startPolling(intervalMs = 20_000): void {
    void this.refresh();
    this.pollTimer = setInterval(() => void this.refresh(), intervalMs);
  }

  public async refresh(): Promise<void> {
    try {
      const [approvals, inboxItems] = await Promise.all([
        listPendingApprovals(this.extensionUri),
        listInboxUnacked(this.extensionUri),
      ]);
      this.approvals = approvals;
      this.inboxItems = inboxItems;
      this.lastError = undefined;
    } catch (error) {
      this.lastError = error instanceof Error ? error.message : String(error);
      this.output.warn(`[inbox] refresh failed: ${this.lastError}`);
    }
    this.updateBadge();
    this.changeEmitter.fire();
  }

  private updateBadge(): void {
    if (!this.view) return;
    const count = this.approvals.length + this.inboxItems.length;
    this.view.badge =
      count > 0 ? { value: count, tooltip: `${count} pending in the Contenox inbox` } : undefined;
  }

  public async approve(ask: PendingAsk): Promise<void> {
    await respondApproval(this.extensionUri, ask.id, { kind: "approve" });
    await this.refresh();
  }

  public async deny(ask: PendingAsk): Promise<void> {
    await respondApproval(this.extensionUri, ask.id, { kind: "deny" });
    await this.refresh();
  }

  public async answer(ask: PendingAsk, text: string): Promise<void> {
    await respondApproval(this.extensionUri, ask.id, { kind: "answer", text });
    await this.refresh();
  }

  public async ack(item: InboxRow): Promise<void> {
    await ackInboxItem(this.extensionUri, item.id);
    await this.refresh();
  }

  public getChildren(element?: InboxTreeNode): InboxTreeNode[] {
    if (!element) {
      if (this.lastError) {
        return [{ kind: "message", label: "Contenox runtime unavailable", description: this.lastError }];
      }
      return [
        { kind: "group", id: "approvals", label: "Pending Approvals", count: this.approvals.length },
        { kind: "group", id: "inbox", label: "Operator Inbox", count: this.inboxItems.length },
      ];
    }
    if (element.kind !== "group") {
      return [];
    }
    if (element.id === "approvals") {
      return this.approvals.length
        ? this.approvals.map((ask) => ({ kind: "approval" as const, ask }))
        : [{ kind: "message", label: "Nothing parked. Gated calls and mission questions land here." }];
    }
    return this.inboxItems.length
      ? this.inboxItems.map((item) => ({ kind: "inboxItem" as const, item }))
      : [{ kind: "message", label: "Nothing unread. Reports with no live session land here." }];
  }

  public getTreeItem(element: InboxTreeNode): vscode.TreeItem {
    if (element.kind === "message") {
      const item = new vscode.TreeItem(element.label, vscode.TreeItemCollapsibleState.None);
      item.description = element.description;
      return item;
    }
    if (element.kind === "group") {
      const item = new vscode.TreeItem(
        `${element.label} (${element.count})`,
        element.count > 0 ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.Collapsed,
      );
      item.contextValue = "contenoxInboxGroup";
      item.iconPath = new vscode.ThemeIcon(element.id === "approvals" ? "unverified" : "inbox");
      return item;
    }
    if (element.kind === "approval") {
      const { ask } = element;
      const item = new vscode.TreeItem(ask.summary || ask.tool || ask.id, vscode.TreeItemCollapsibleState.None);
      item.id = `approval:${ask.id}`;
      item.description = ask.kind === "question" ? "question" : ask.tool;
      item.tooltip = `${ask.kind === "question" ? "Question" : "Permission ask"}: ${ask.tool}\nID: ${ask.id}\nMission: ${ask.mission || "-"}\n\nSafe to leave parked -- answer from here, the Contenox panel, or 'contenox approvals respond'.`;
      item.contextValue = ask.kind === "question" ? "contenoxInboxQuestion" : "contenoxInboxApproval";
      item.iconPath = new vscode.ThemeIcon(ask.kind === "question" ? "question" : "shield");
      return item;
    }
    const { item: row } = element;
    const treeItem = new vscode.TreeItem(row.summary || row.kind || row.id, vscode.TreeItemCollapsibleState.None);
    treeItem.id = `inbox:${row.id}`;
    treeItem.description = row.reason;
    treeItem.tooltip = `${row.kind}: ${row.summary}\nMission: ${row.mission}\nReason: ${row.reason}`;
    treeItem.contextValue = "contenoxInboxReport";
    treeItem.iconPath = new vscode.ThemeIcon("mail");
    return treeItem;
  }

  public dispose(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = undefined;
    }
    this.changeEmitter.dispose();
  }
}
