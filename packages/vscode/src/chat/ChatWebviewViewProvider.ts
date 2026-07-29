import * as path from "node:path";
import * as vscode from "vscode";
import {
  AcpApprovalRequest,
  AcpApprovalResponse,
  AcpChatClient,
  AcpReplayEvent,
  AcpToolCallEvent,
} from "../acp/AcpChatClient";
import { AcpSessionInfo } from "../acp/types";
import { isApprovalPending, respondApproval } from "../approval/approvalsCli";
import { BridgeProcess } from "../bridge/BridgeProcess";
import { EditorContextAttachment, ToolCallEvent } from "../bridge/protocol";
import {
  resolveActiveFileAttachment,
  resolveFileAttachment,
  resolveSelectionAttachment,
  resolveSymbolAttachment,
  searchWorkspaceFiles,
  searchWorkspaceSymbols,
  toWireAttachments,
} from "../editor/attachments";
import { collectEditorContext, contextSummary } from "../editor/context";
import { DiffStore } from "../editor/diffStore";
import { collectGitChangeContext } from "../editor/gitContext";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";
import {
  ChatHostToWebviewMessage,
  ChatWebviewToHostMessage,
  WireAttachment,
  WireCommand,
  WireMessage,
  WireRuntimeSummary,
  WireSession,
  WireSessionResponse,
  WireSuspendedApproval,
  WireToolCall,
} from "./webviewProtocol";

// Persisted across a full VS Code restart (unlike acpSessions/pendingApprovals,
// which are in-memory only) -- workspaceState is how "reopen and catch up"
// (Phase 5) knows a session was left parked before the extension host that
// asked about it ever existed in this process.
const suspendedStateKey = "contenox.suspendedApprovals";
type SuspendedStateMap = Record<string, WireSuspendedApproval>;

interface PendingApproval {
  request: AcpApprovalRequest;
  resolve: (response: AcpApprovalResponse) => void;
}

// A local cache of one ACP session's title + rendered messages, keyed by ACP
// session id. Sessions themselves are durable (session/new persists
// immediately server-side, see acpsvc/session.go); this cache only avoids
// re-fetching/re-rendering a session's history on every message send within
// this extension-host's lifetime. Rebuilt from session/load on first access
// after a reload (see handleGetSession).
interface AcpSessionRecord {
  title: string;
  createdAt: string;
  updatedAt: string;
  messages: WireMessage[];
}

// Chat send/create/cancel/approval/list/load/delete all run over ACP
// (AcpChatClient) -- see docs/development/internal/vscode-implementation-plan.md
// (Phase 1 "sessions over ACP"). Sessions are durable: session/list,
// session/load and session/delete persist across a webview reload, unlike the
// pre-Phase-1 in-memory-only sessions this replaces. Only the runtime summary
// (provider/model/think/hitl display in the header chip) still reads the old
// BridgeProcess; see RuntimeControlsView.ts for the ACP-backed runtime panel.
export class ChatWebviewViewProvider implements vscode.WebviewViewProvider, vscode.Disposable {
  private view: vscode.WebviewView | undefined;
  private sessionId: string | undefined;
  private readonly queued: ChatHostToWebviewMessage[] = [];
  private readonly pendingApprovals = new Map<string, PendingApproval>();
  private readonly acpSessions = new Map<string, AcpSessionRecord>();
  // Sessions currently parked on an unanswered approval, keyed by ACP
  // session id -- the in-memory mirror of workspaceState's suspendedStateKey
  // (Phase 5 "walk-away"). Read on a cold getSession to know a session needs
  // a pending-approval re-check even though this process never saw it park.
  private readonly suspendedApprovals = new Map<string, WireSuspendedApproval>();
  // Context usage (Phase 2 "context/token usage") -- sourced live from ACP's
  // usage_update, not the old bridge's onContextUsage (which never fires for
  // ACP-driven turns) nor a model-list token-limit guess. Undefined until the
  // first usage_update of this extension-host session; size 0 once one
  // arrives is a legitimate "no configured budget" value (see
  // enginebridge/events.go's UsageUpdated), rendered as an absolute count.
  private lastContextUsed: number | undefined;
  private lastContextSize: number | undefined;
  // available_commands_update (Phase 2 "slash commands"), full-replacement.
  private lastCommands: WireCommand[] = [];

  public constructor(
    private readonly bridge: BridgeProcess,
    private readonly acpClient: AcpChatClient,
    private readonly diffStore: DiffStore,
    private readonly extensionUri: vscode.Uri,
    private readonly output: ContenoxOutput,
    private readonly telemetry: TelemetryLogger,
    private readonly onSessionsChanged: () => void,
    // workspaceState, not globalState: "which session did I leave parked" is
    // meaningful per-workspace, the same scope sessions themselves are keyed
    // to via cwd (acpsvc's resolveSessionWorkspace).
    private readonly workspaceState: vscode.Memento,
  ) {
    for (const [id, approval] of Object.entries(
      this.workspaceState.get<SuspendedStateMap>(suspendedStateKey, {}),
    )) {
      this.suspendedApprovals.set(id, approval);
    }
  }

  public resolveWebviewView(view: vscode.WebviewView): void {
    this.view = view;
    view.webview.options = {
      enableScripts: true,
      localResourceRoots: [vscode.Uri.joinPath(this.extensionUri, "media")],
    };
    view.webview.html = this.renderShell(view.webview);
    view.webview.onDidReceiveMessage((message: ChatWebviewToHostMessage) => {
      void this.handleMessage(message);
    });
  }

  public dispose(): void {
    this.view = undefined;
  }

  public reveal(): void {
    void vscode.commands.executeCommand("contenox.chat.focus");
  }

  public async openChat(): Promise<void> {
    this.reveal();
  }

  public async refreshRuntimeSummary(): Promise<void> {
    await this.pushRuntimeSummary();
  }

  public async showRuntimeSettingsPicker(): Promise<void> {
    await this.handleOpenRuntimeSettings();
  }

  public async openSession(sessionId?: string): Promise<void> {
    this.setActiveSession(sessionId);
    this.reveal();
    if (sessionId) {
      void this.postToWebview({ type: "selectSession", id: sessionId });
    }
  }

  public setActiveSession(sessionId?: string): void {
    if (sessionId) {
      this.sessionId = sessionId;
    }
  }

  public clearActiveSession(sessionId?: string): void {
    if (!sessionId || this.sessionId === sessionId) {
      this.sessionId = undefined;
    }
  }

  public async askSelection(): Promise<void> {
    const editorContext = await collectEditorContext({
      includeSelection: true,
      includeActiveFile: false,
      includeDiagnostics: false,
    });
    if (!editorContext.some((item) => item.kind === "selection")) {
      vscode.window.showInformationMessage("No editor selection is active.");
      return;
    }
    await this.runQuickAction(editorContext, "Explain the selected code.", true);
  }

  public async fixSelection(): Promise<void> {
    const editorContext = await collectEditorContext({
      includeSelection: true,
      includeActiveFile: true,
      includeDiagnostics: false,
    });
    if (!editorContext.some((item) => item.kind === "selection")) {
      vscode.window.showInformationMessage("No editor selection is active.");
      return;
    }
    await this.runQuickAction(editorContext, "Fix the diagnostics in the active file.", true);
  }

  public async addSelectionToChat(): Promise<void> {
    const editorContext = await collectEditorContext({
      includeSelection: true,
      includeActiveFile: false,
      includeDiagnostics: false,
    });
    if (!editorContext.some((item) => item.kind === "selection")) {
      vscode.window.showInformationMessage("No editor selection is active.");
      return;
    }
    await this.runQuickAction(editorContext, "", false);
  }

  public async fixDiagnostics(diagnostics?: readonly vscode.Diagnostic[]): Promise<void> {
    const editorContext = await collectEditorContext({
      includeSelection: true,
      includeActiveFile: true,
      includeDiagnostics: true,
      diagnostics,
    });
    if (!editorContext.some((item) => item.kind === "diagnostics")) {
      vscode.window.showInformationMessage("No diagnostics are available for the active file.");
      return;
    }
    await this.runQuickAction(editorContext, "Fix the diagnostics in the active file.", true);
  }

  public async explainDiagnostics(diagnostics?: readonly vscode.Diagnostic[]): Promise<void> {
    const editorContext = await collectEditorContext({
      includeSelection: true,
      includeActiveFile: true,
      includeDiagnostics: true,
      diagnostics,
    });
    if (!editorContext.some((item) => item.kind === "diagnostics")) {
      vscode.window.showInformationMessage("No diagnostics are available for the active file.");
      return;
    }
    await this.runQuickAction(editorContext, "Explain the active diagnostics.", true);
  }

  public async reviewChanges(): Promise<void> {
    const gitContext = await collectGitChangeContext();
    if (gitContext.length === 0) {
      vscode.window.showInformationMessage("No git changes are available to review.");
      return;
    }
    await this.runQuickAction(gitContext, "Review the current git changes.", true);
  }

  public async draftCommitMessage(): Promise<void> {
    const gitContext = await collectGitChangeContext();
    if (gitContext.length === 0) {
      vscode.window.showInformationMessage("No git changes are available for a commit message.");
      return;
    }
    await this.runQuickAction(gitContext, "Draft a commit message for the current git changes.", true);
  }

  // runQuickAction attaches editor-command context (Ask/Fix Selection,
  // Fix/Explain Diagnostics, Review Changes, Draft Commit Message) as
  // structured, visible attachment chips (Phase 3) rather than the old
  // `pendingContext` TTL side-channel: the attachments travel directly on the
  // composerAction message and land in the webview's attachment chip strip,
  // so the developer sees exactly what was captured before it is ever sent.
  private async runQuickAction(
    context: EditorContextAttachment[],
    content: string,
    submit: boolean,
  ): Promise<void> {
    const attachments = toWireAttachments(context);
    this.telemetry.event("chat.attachments.from_editor_command", { context: contextSummary(context) });
    this.reveal();
    await this.postToWebview({
      type: "composerAction",
      nonce: `${Date.now()}-${Math.random().toString(16).slice(2)}`,
      content,
      submit,
      attachments,
    });
  }

  private async handleMessage(message: ChatWebviewToHostMessage): Promise<void> {
    switch (message.type) {
      case "ready":
        for (const queued of this.queued.splice(0)) {
          void this.postToWebview(queued);
        }
        void this.pushRuntimeSummary();
        return;
      case "getRuntimeSummary":
        return this.handleGetRuntimeSummary(message.requestId);
      case "openRuntimeSettings":
        return this.handleOpenRuntimeSettings();
      case "listSessions":
        return this.handleListSessions(message.requestId);
      case "createSession":
        return this.handleCreateSession(message.requestId, message.title);
      case "getSession":
        return this.handleGetSession(message.requestId, message.id);
      case "renameSession":
        return this.handleRenameSession(message.requestId, message.id, message.title);
      case "deleteSession":
        return this.handleDeleteSession(message.requestId, message.id);
      case "sendMessage":
        return this.handleSendMessage(message.requestId, message.id, message.content, message.attachments);
      case "searchFiles":
        return this.handleSearchFiles(message.requestId, message.query);
      case "searchSymbols":
        return this.handleSearchSymbols(message.requestId, message.query);
      case "attachFile":
        return this.handleAttachFile(message.requestId, message.uri);
      case "attachSymbol":
        return this.handleAttachSymbol(message.requestId, message.uri, message.line, message.name);
      case "attachSelection":
        return this.handleAttachSelection(message.requestId);
      case "attachActiveFile":
        return this.handleAttachActiveFile(message.requestId);
      case "insertAtCursor":
        return this.handleInsertAtCursor(message.requestId, message.code);
      case "applyCodeBlock":
        return this.handleApplyCodeBlock(message.requestId, message.code, message.language, message.hintPath);
      case "cancelTurn":
        this.acpClient.cancelTurn(message.id);
        return;
      case "listTools":
        return this.postResult(message.requestId, true, []);
      case "approvalResponse":
        return this.handleApprovalResponse(message.requestId, message.optionId);
      case "checkSuspended":
        return this.handleCheckSuspended(message.requestId, message.id, message.approvalId);
      case "respondSuspendedApproval":
        return this.handleRespondSuspendedApproval(message.requestId, message.id, message.approvalId, message.verdict);
      case "openDiff":
        await this.diffStore.open({
          title: message.call.title ?? "Contenox Diff",
          before: message.call.diff?.before,
          after: message.call.diff?.after,
          filePath: message.call.diff?.path,
        });
        return;
      case "confirmDelete":
        return this.handleConfirmDelete(message.requestId, message.title);
      case "promptRename":
        return this.handlePromptRename(message.requestId, message.title);
    }
  }

  // --- Phase 3: @file / @symbol pickers, one-click selection/active-file
  // attachments (vscode-implementation-plan.md §5). All resolved here since
  // only the extension host has vscode.workspace/window API; the webview
  // only ever sees the resulting WireAttachment.

  private async handleSearchFiles(requestId: string, query: string): Promise<void> {
    try {
      this.postResult(requestId, true, await searchWorkspaceFiles(query));
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleSearchSymbols(requestId: string, query: string): Promise<void> {
    try {
      this.postResult(requestId, true, await searchWorkspaceSymbols(query));
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleAttachFile(requestId: string, uri: string): Promise<void> {
    try {
      const attachment = await resolveFileAttachment(uri);
      this.telemetry.event("chat.attachments.file", { uri });
      this.postResult(requestId, true, attachment ?? null);
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleAttachSymbol(requestId: string, uri: string, line: number, name: string): Promise<void> {
    try {
      const attachment = await resolveSymbolAttachment(uri, line, name);
      this.telemetry.event("chat.attachments.symbol", { uri, name });
      this.postResult(requestId, true, attachment ?? null);
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private handleAttachSelection(requestId: string): void {
    const attachment = resolveSelectionAttachment();
    this.telemetry.event("chat.attachments.selection", { attached: Boolean(attachment) });
    this.postResult(requestId, true, attachment ?? null);
  }

  private handleAttachActiveFile(requestId: string): void {
    const attachment = resolveActiveFileAttachment();
    this.telemetry.event("chat.attachments.active_file", { attached: Boolean(attachment) });
    this.postResult(requestId, true, attachment ?? null);
  }

  // --- Phase 4: code out of the panel (vscode-implementation-plan.md §5).
  // Pure VS Code API on content the panel already renders -- no protocol work.

  private async handleInsertAtCursor(requestId: string, code: string): Promise<void> {
    const editor = vscode.window.activeTextEditor;
    if (!editor) {
      vscode.window.showWarningMessage("No active editor to insert into.");
      this.postResult(requestId, true, false);
      return;
    }
    const inserted = await editor.edit((editBuilder) => {
      if (editor.selection.isEmpty) {
        editBuilder.insert(editor.selection.active, code);
      } else {
        editBuilder.replace(editor.selection, code);
      }
    });
    this.telemetry.event("chat.codeblock.insert", { inserted });
    this.postResult(requestId, true, inserted);
  }

  private async handleApplyCodeBlock(
    requestId: string,
    code: string,
    language: string | undefined,
    hintPath: string | undefined,
  ): Promise<void> {
    try {
      const target = await this.resolveApplyTarget(hintPath);
      if (!target) {
        this.postResult(requestId, true, { applied: false });
        return;
      }
      const before = await readFileIfExists(target);
      const diffTitle = `Apply code block: ${vscode.workspace.asRelativePath(target, false)}`;
      await this.diffStore.open({ title: diffTitle, before: before ?? "", after: code, filePath: target.fsPath });

      const choice = await vscode.window.showInformationMessage(
        `Apply this code block to ${vscode.workspace.asRelativePath(target, false)}?`,
        "Apply",
        "Cancel",
      );
      if (choice !== "Apply") {
        this.telemetry.event("chat.codeblock.apply", { applied: false, language });
        this.postResult(requestId, true, { applied: false });
        return;
      }

      await applyFullFileEdit(target, code);
      this.telemetry.event("chat.codeblock.apply", { applied: true, language });
      this.postResult(requestId, true, { applied: true, path: vscode.workspace.asRelativePath(target, false) });
    } catch (error) {
      this.output.warn(`[codeblock] apply failed: ${errorMessage(error)}`);
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  // resolveApplyTarget: prefer an explicit filename hint parsed from the code
  // block (resolved against the first workspace folder); fall back to the
  // active editor's file; otherwise ask, since "the block names a file" isn't
  // guaranteed (the coding chain isn't ours to change here -- no Go work).
  private async resolveApplyTarget(hintPath: string | undefined): Promise<vscode.Uri | undefined> {
    const folder = vscode.workspace.workspaceFolders?.[0];
    if (hintPath) {
      const resolved = path.isAbsolute(hintPath)
        ? vscode.Uri.file(hintPath)
        : folder
          ? vscode.Uri.joinPath(folder.uri, hintPath)
          : undefined;
      if (resolved) {
        return resolved;
      }
    }
    const active = vscode.window.activeTextEditor?.document.uri;
    if (active && active.scheme === "file") {
      return active;
    }
    const typed = await vscode.window.showInputBox({
      title: "Apply code block",
      prompt: "Path to apply this code block to (relative to the workspace, or absolute)",
      placeHolder: "src/example.ts",
    });
    if (!typed) {
      return undefined;
    }
    return path.isAbsolute(typed) ? vscode.Uri.file(typed) : folder ? vscode.Uri.joinPath(folder.uri, typed) : vscode.Uri.file(typed);
  }

  private async handleConfirmDelete(requestId: string, title: string): Promise<void> {
    const choice = await vscode.window.showWarningMessage(
      `Delete "${title}"?`,
      { modal: true },
      "Delete",
    );
    this.postResult(requestId, true, choice === "Delete");
  }

  private async handleGetRuntimeSummary(requestId: string): Promise<void> {
    try {
      const summary = await this.loadRuntimeSummary();
      this.postResult(requestId, true, summary);
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleOpenRuntimeSettings(): Promise<void> {
    const summary: WireRuntimeSummary = await this.loadRuntimeSummary().catch(() => ({
      connected: false,
    }));
    const provider = summary.provider || "not set";
    const model = summary.model || "not set";
    const isConfigured = summary.configured ?? (summary.provider && summary.model);

    const picks: any[] = [];

    if (!isConfigured) {
      picks.push({
        label: "$(play) Run Guided Setup (recommended)",
        description: "Choose provider + model, refresh defaults",
        command: "contenox.runSetup",
        isSetup: true,
      });
      // separator
      picks.push({ label: "", kind: -1 as any });
    }

    picks.push(
      {
        label: "Provider",
        description: provider,
        command: "contenox.selectProvider",
      },
      {
        label: "Model",
        description: model,
        command: "contenox.selectChatModel",
      },
      {
        label: "Thinking level",
        description: summary.think || "auto",
        command: "contenox.selectThinkLevel",
      },
      {
        label: "HITL policy",
        description: summary.hitlPolicy || "default",
        command: "contenox.selectHitlPolicy",
      },
      {
        label: "Open full Runtime panel",
        description: "Detailed configuration in sidebar",
        focusSettings: true,
      },
    );

    if (isConfigured) {
      picks.push({
        label: "Run Doctor (diagnostics)",
        description: "Check backends, reachability and issues",
        command: "contenox.doctor",
      });
    }

    const choice = await vscode.window.showQuickPick(picks, {
      title: "Contenox runtime",
      placeHolder: isConfigured ? "Choose a runtime setting to change" : "Setup required — pick an action",
    });
    if (!choice) {
      return;
    }
    if ("focusSettings" in choice && choice.focusSettings) {
      await vscode.commands.executeCommand("contenox.controls.focus");
      return;
    }
    if ("command" in choice && choice.command) {
      await vscode.commands.executeCommand(choice.command);
      await this.pushRuntimeSummary();
    }
  }

  private async pushRuntimeSummary(): Promise<void> {
    try {
      const summary = await this.loadRuntimeSummary();
      await this.postToWebview({ type: "runtimeConfig", summary });
    } catch {
      await this.postToWebview({
        type: "runtimeConfig",
        summary: { connected: false },
      });
    }
  }

  private async loadRuntimeSummary(): Promise<WireRuntimeSummary> {
    const state = await this.bridge.ensureStarted();
    const client = this.bridge.currentClient;
    if (!client) {
      throw new Error("Contenox runtime connection is not available");
    }
    const config = await client.getConfig();
    const health = state.health;
    return {
      provider: config.defaultProvider,
      model: config.defaultModel,
      think: config.defaultThink,
      hitlPolicy: config.hitlPolicyName,
      connected: health.status === "ok",
      configured: health.configured,
      status: health.status,
      // Live ACP usage_update only (Phase 2 "context/token usage") -- no
      // longer guessed from the model's advertised context length, which is
      // what produced the permanently-static "0/4.096 (0%)" chip.
      contextUsed: this.lastContextUsed,
      contextSize: this.lastContextSize,
    };
  }

  private async handlePromptRename(requestId: string, currentTitle: string): Promise<void> {
    const title = await vscode.window.showInputBox({
      title: "Rename session",
      value: currentTitle,
      prompt: "Enter a new session name",
      validateInput: (value) => (value.trim() ? undefined : "Session name cannot be empty"),
    });
    this.postResult(requestId, true, title?.trim() || undefined);
  }

  private async handleListSessions(requestId: string): Promise<void> {
    try {
      const result = await this.acpClient.listSessions();
      this.postResult(requestId, true, result.sessions.map(toWireSession));
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleCreateSession(requestId: string, title: string): Promise<void> {
    try {
      const { id } = await this.acpClient.createSession();
      const record = this.ensureAcpSessionRecord(id);
      record.title = title;
      this.setActiveSession(id);
      this.onSessionsChanged();
      this.postResult(requestId, true, {
        session: this.wireSessionFor(id, record),
        messages: record.messages,
      } satisfies WireSessionResponse);
    } catch (error) {
      this.output.warn(`[acp] createSession failed: ${errorMessage(error)}`);
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleGetSession(requestId: string, id: string): Promise<void> {
    const local = this.acpSessions.get(id);
    if (local) {
      this.setActiveSession(id);
      this.postResult(requestId, true, {
        session: this.wireSessionFor(id, local),
        messages: local.messages,
        suspended: this.suspendedApprovals.get(id) ?? null,
      } satisfies WireSessionResponse);
      return;
    }
    try {
      const loaded = await this.acpClient.loadSession(id);
      const messages = this.wireMessagesFromReplay(id, loaded.events);
      const now = new Date().toISOString();
      const record: AcpSessionRecord = { title: "", createdAt: now, updatedAt: now, messages };
      this.acpSessions.set(id, record);
      this.setActiveSession(id);

      // Phase 5 "catch up on reopen": this is a cold load (no in-memory
      // record existed), which is exactly the shape of "VS Code was closed
      // and reopened". If a prior run of this extension left this session
      // marked suspended (workspaceState survives the restart), find out
      // now, automatically, whether it resolved while nobody was watching --
      // the verdict may have landed via `contenox approvals respond` with
      // VS Code closed the whole time, in which case the durable session
      // history above is already the completed transcript and the marker is
      // just stale. Only when it is genuinely still parked does the panel
      // surface the "still waiting" banner instead of quietly saying nothing.
      const marker = this.suspendedApprovals.get(id);
      if (marker) {
        const caughtUp = await this.checkAndCatchUp(id, marker.approvalId).catch((error: unknown) => {
          this.output.warn(`[walk-away] reopen catch-up check failed: ${errorMessage(error)}`);
          return undefined;
        });
        if (caughtUp?.resolved && caughtUp.messages) {
          record.messages = caughtUp.messages;
          record.updatedAt = new Date().toISOString();
          this.acpSessions.set(id, record);
        }
      }

      this.postResult(requestId, true, {
        session: this.wireSessionFor(id, record),
        messages: record.messages,
        suspended: this.suspendedApprovals.get(id) ?? null,
      } satisfies WireSessionResponse);
    } catch (error) {
      this.output.warn(`[acp] loadSession failed: ${errorMessage(error)}`);
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleRenameSession(requestId: string, id: string, title: string): Promise<void> {
    const local = this.acpSessions.get(id);
    if (local) {
      local.title = title;
      this.postResult(requestId, true, {
        session: this.wireSessionFor(id, local),
        messages: local.messages,
      } satisfies WireSessionResponse);
      return;
    }
    this.postResult(requestId, false, "Renaming sessions is not supported");
  }

  private async handleDeleteSession(requestId: string, id: string): Promise<void> {
    try {
      await this.acpClient.deleteSession(id);
      this.acpSessions.delete(id);
      this.clearActiveSession(id);
      this.onSessionsChanged();
      this.postResult(requestId, true, undefined);
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleSendMessage(
    requestId: string,
    sessionId: string,
    content: string,
    attachments: WireAttachment[],
  ): Promise<void> {
    const record = this.ensureAcpSessionRecord(sessionId);
    const now = new Date().toISOString();
    record.messages.push({
      id: `${sessionId}-${Date.now()}-u`,
      sessionId,
      role: "user",
      content,
      createdAt: now,
    });
    record.updatedAt = now;

    const toolCalls = new Map<string, WireToolCall>();
    // Set when a permission request was shown this turn (Phase 5
    // "walk-away" suspension detection): needed because the RPC itself does
    // NOT stay open past the fast window -- the agent actively cancels it
    // (libacp/clientconn.go's call() sends $/cancel_request when askCtx's
    // context.WithTimeout(parkWindow) fires; localtools.HITLWrapper.askDurable
    // passes exactly that context to the ask() callback). The abort handler
    // below always resolves the RPC as "cancelled" either way -- the only
    // way to tell an auto-cancelled park apart from a real session/cancel is
    // the turn's own stopReason, checked after the await.
    let shownApproval: AcpApprovalRequest | undefined;
    let approvalAutoCancelled = false;

    try {
      const result = await this.acpClient.sendMessage(sessionId, content, attachments, {
        onDelta: (event) => {
          void this.postToWebview({
            type: "delta",
            requestId,
            content: event.content,
            thinking: event.thinking,
          });
        },
        onToolCall: (event) => {
          const wire = this.toWireToolCall(event);
          toolCalls.set(wire.id, wire);
          void this.postToWebview({ type: "toolCall", requestId, call: wire });
        },
        onUsage: (event) => {
          this.lastContextUsed = event.used;
          this.lastContextSize = event.size;
          void this.postToWebview({ type: "usage", used: event.used, size: event.size, cost: event.cost });
        },
        onCommands: (commands) => {
          this.lastCommands = commands.map((command) => ({
            name: command.name,
            description: command.description,
            hint: command.hint,
          }));
          void this.postToWebview({ type: "commands", commands: this.lastCommands });
        },
        onPermissionRequested: (request, signal) =>
          new Promise<AcpApprovalResponse>((resolve) => {
            this.pendingApprovals.set(requestId, { request, resolve });
            shownApproval = request;
            void this.postToWebview({
              type: "approvalRequest",
              requestId,
              request: {
                approvalId: request.approvalId,
                title: request.title,
                toolsName: request.toolsName,
                toolName: request.toolName,
                policyName: request.policyName,
                policyPath: request.policyPath,
                matchedRule: request.matchedRule,
                matchedRuleDetail: request.matchedRuleDetail,
                details: request.details,
                diff:
                  request.diffOld !== undefined || request.diffNew !== undefined
                    ? { before: request.diffOld, after: request.diffNew }
                    : undefined,
                options: request.options,
              },
            });
            // The agent aborts this specific request (via $/cancel_request)
            // both when the turn is really cancelled (session/cancel) AND
            // when the fast park window elapses unanswered (the agent gives
            // up waiting and checkpoints -- see the comment on
            // approvalAutoCancelled above). Either way the client MUST still
            // answer with `cancelled` rather than leave the RPC hanging;
            // which case this was gets disambiguated below via stopReason.
            signal.addEventListener(
              "abort",
              () => {
                if (this.pendingApprovals.delete(requestId)) {
                  approvalAutoCancelled = true;
                  resolve({ outcome: { outcome: "cancelled" } });
                }
              },
              { once: true },
            );
          }),
      });

      // Walk-away (Phase 5): an approval was shown, the agent gave up
      // waiting on it (approvalAutoCancelled), and the *turn* itself did not
      // end via a real cancellation (prompt.go forces stopReason "cancelled"
      // only for an actual session/cancel, never for a suspension -- see
      // mapStopReason's StopSuspended case). That combination is the only
      // client-observable signal that this run parked past the fast window
      // and checkpointed server-side; nothing else distinguishes it from an
      // ordinary end_turn on the wire.
      const suspended =
        shownApproval && approvalAutoCancelled && result.stopReason !== "cancelled"
          ? toWireSuspendedApproval(shownApproval)
          : undefined;
      if (suspended) {
        void this.rememberSuspended(sessionId, suspended);
        // The verdict can land quickly if it's being answered from a
        // terminal in parallel; catch that without waiting for a manual
        // "Check now" click or the next reopen.
        void this.pollCatchUp(sessionId, suspended.approvalId);
      } else {
        void this.forgetSuspended(sessionId);
      }

      // A real failure still rejects the webview's request (top-level error
      // banner). Cancellation is not a failure (vscode-implementation-plan.md
      // Phase 2 "turn states") -- it resolves normally, with whatever content
      // streamed before the cancel and stopReason "cancelled" so the
      // transcript renders a neutral badge instead of an error.
      if (result.failed) {
        this.postResult(requestId, false, result.error ?? "Contenox request failed");
        return;
      }

      const assistantMessage: WireMessage = {
        id: `${sessionId}-${Date.now()}-a`,
        sessionId,
        role: "assistant",
        content: result.content,
        createdAt: new Date().toISOString(),
        toolCalls: toolCalls.size > 0 ? Array.from(toolCalls.values()) : undefined,
        stopReason: result.stopReason,
      };
      record.messages.push(assistantMessage);
      record.updatedAt = assistantMessage.createdAt;

      this.postResult(requestId, true, { messages: record.messages, suspended } satisfies WireSessionResponse);
      this.onSessionsChanged();
      void this.pushRuntimeSummary();
    } catch (error) {
      this.output.warn(`[acp] sendMessage failed: ${errorMessage(error)}`);
      this.postResult(requestId, false, errorMessage(error));
    } finally {
      this.pendingApprovals.delete(requestId);
    }
  }

  private toWireToolCall(event: AcpToolCallEvent): WireToolCall {
    const hasDiff = event.diffOld !== undefined || event.diffNew !== undefined;
    const diff = hasDiff
      ? this.diffStore.registerToolDiff({
          sessionId: "",
          turnId: "",
          toolCallId: event.id,
          title: event.title,
          status: event.status,
          toolName: event.toolName,
          diffPath: event.diffPath,
          diffOld: event.diffOld,
          diffNew: event.diffNew,
        } satisfies ToolCallEvent)
      : undefined;
    return {
      id: event.id,
      title: event.title,
      status: event.status,
      toolName: event.toolName,
      output: event.output,
      error: event.error,
      diff: diff ? { path: event.diffPath, before: event.diffOld, after: event.diffNew } : undefined,
    };
  }

  // handleApprovalResponse answers a live in-panel card while its RPC is
  // still genuinely open -- i.e. strictly inside the fast park window. Once
  // that window elapses the agent has already cancelled the RPC itself (see
  // handleSendMessage's approvalAutoCancelled), so a suspended approval is
  // never resolved through here; it goes through
  // handleRespondSuspendedApproval (the durable ask store) instead.
  private handleApprovalResponse(requestId: string, optionId: string | undefined): void {
    const pending = this.pendingApprovals.get(requestId);
    if (!pending) {
      return;
    }
    this.pendingApprovals.delete(requestId);
    pending.resolve(resolveApprovalOutcome(pending.request, optionId));
  }

  // pollCatchUp re-checks a just-suspended approval a few times with
  // backoff (the resume runs asynchronously server-side) and pushes the
  // caught-up transcript to the webview the moment it lands, without
  // requiring the user to reselect the session. Gives up quietly after the
  // last attempt -- "Check now" (handleCheckSuspended) and the next reopen
  // both still catch it up.
  private async pollCatchUp(sessionId: string, approvalId: string): Promise<void> {
    const delaysMs = [500, 1000, 2000, 4000, 8000];
    for (const delay of delaysMs) {
      await new Promise((resolve) => setTimeout(resolve, delay));
      const result = await this.checkAndCatchUp(sessionId, approvalId).catch((error: unknown) => {
        this.output.warn(`[walk-away] poll catch-up failed: ${errorMessage(error)}`);
        return undefined;
      });
      if (result?.resolved) {
        void this.postToWebview({
          type: "sessionCaughtUp",
          id: sessionId,
          session: this.wireSessionFor(sessionId, this.ensureAcpSessionRecord(sessionId)),
          messages: result.messages,
        });
        return;
      }
    }
  }

  // checkAndCatchUp is the one Phase 5 "walk-away" re-check used by the
  // "Check now" affordance, the post-answer poll above, and a cold
  // getSession's automatic first check: rebind cheaply via session/resume
  // (ResumeSession -- read internal/surfaces/acpsvc/session.go, not guessed:
  // "the client kept its transcript and only needs the server-side session
  // re-bound"), then ask the durable ask store -- not the ACP wire, which has
  // nothing to say about a run nobody is watching -- whether the approval is
  // still pending. Only pays for a full session/load replay once there is
  // actually something new to show.
  private async checkAndCatchUp(
    sessionId: string,
    approvalId: string,
  ): Promise<{ resolved: boolean; messages?: WireMessage[] }> {
    await this.acpClient.resumeSession(sessionId).catch((error: unknown) => {
      this.output.warn(`[walk-away] session/resume failed (continuing to check anyway): ${errorMessage(error)}`);
    });
    const pending = await isApprovalPending(this.extensionUri, approvalId);
    if (pending) {
      return { resolved: false };
    }
    const loaded = await this.acpClient.loadSession(sessionId);
    const messages = this.wireMessagesFromReplay(sessionId, loaded.events);
    const now = new Date().toISOString();
    this.acpSessions.set(sessionId, { title: "", createdAt: now, updatedAt: now, messages });
    void this.forgetSuspended(sessionId);
    return { resolved: true, messages };
  }

  private async handleCheckSuspended(requestId: string, sessionId: string, approvalId: string): Promise<void> {
    try {
      const result = await this.checkAndCatchUp(sessionId, approvalId);
      this.postResult(requestId, true, {
        session: this.wireSessionFor(sessionId, this.ensureAcpSessionRecord(sessionId)),
        messages: result.messages,
        suspended: result.resolved ? null : this.suspendedApprovals.get(sessionId) ?? null,
      } satisfies WireSessionResponse);
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async handleRespondSuspendedApproval(
    requestId: string,
    sessionId: string,
    approvalId: string,
    verdict: "approve" | "deny",
  ): Promise<void> {
    try {
      // No live in-panel RPC to resolve here -- the process that asked is
      // gone (VS Code was closed and reopened). Answer the same durable ask
      // `contenox approvals respond` would, from this process instead.
      await respondApproval(this.extensionUri, approvalId, { kind: verdict });
      const result = await this.checkAndCatchUp(sessionId, approvalId);
      this.postResult(requestId, true, {
        session: this.wireSessionFor(sessionId, this.ensureAcpSessionRecord(sessionId)),
        messages: result.messages,
        suspended: result.resolved ? null : this.suspendedApprovals.get(sessionId) ?? null,
      } satisfies WireSessionResponse);
      this.onSessionsChanged();
    } catch (error) {
      this.postResult(requestId, false, errorMessage(error));
    }
  }

  private async rememberSuspended(sessionId: string, approval: WireSuspendedApproval): Promise<void> {
    this.suspendedApprovals.set(sessionId, approval);
    const map: SuspendedStateMap = {};
    for (const [id, item] of this.suspendedApprovals) {
      map[id] = item;
    }
    await this.workspaceState.update(suspendedStateKey, map);
  }

  private async forgetSuspended(sessionId: string): Promise<void> {
    if (!this.suspendedApprovals.delete(sessionId)) {
      return;
    }
    const map: SuspendedStateMap = {};
    for (const [id, item] of this.suspendedApprovals) {
      map[id] = item;
    }
    await this.workspaceState.update(suspendedStateKey, map);
  }

  private postResult(requestId: string, ok: boolean, value: unknown): void {
    void this.postToWebview(
      ok
        ? { type: "result", requestId, ok: true, value }
        : { type: "result", requestId, ok: false, error: String(value) },
    );
  }

  private async postToWebview(message: ChatHostToWebviewMessage): Promise<void> {
    if (!this.view) {
      this.queued.push(message);
      return;
    }
    await this.view.webview.postMessage(message);
  }

  // wireMessagesFromReplay folds a session/load replay (AcpChatClient's
  // protocol-shaped events) into the webview's WireMessage[] shape: text
  // events become user/assistant messages, and tool call events attach to
  // the most recent assistant message (mirroring how a live turn's tool
  // calls land on the assistant message handleSendMessage appends -- see
  // toolCalls collection there) or, absent one, become a standalone "tool"
  // message so nothing from history is silently dropped.
  private wireMessagesFromReplay(sessionId: string, events: AcpReplayEvent[]): WireMessage[] {
    const messages: WireMessage[] = [];
    let lastAssistant: WireMessage | undefined;
    let seq = 0;
    for (const event of events) {
      seq += 1;
      if (event.kind === "message") {
        const message: WireMessage = {
          id: `${sessionId}-replay-${seq}`,
          sessionId,
          role: event.role,
          content: event.text,
          createdAt: new Date().toISOString(),
        };
        messages.push(message);
        lastAssistant = event.role === "assistant" ? message : undefined;
        continue;
      }
      const wire = this.toWireToolCall(event.event);
      if (lastAssistant) {
        const existing = lastAssistant.toolCalls ?? [];
        const index = existing.findIndex((call) => call.id === wire.id);
        lastAssistant.toolCalls = index >= 0 ? replaceAt(existing, index, wire) : [...existing, wire];
        continue;
      }
      messages.push({
        id: `${sessionId}-replay-${seq}-tool`,
        sessionId,
        role: "tool",
        content: "",
        createdAt: new Date().toISOString(),
        toolCalls: [wire],
      });
    }
    return messages;
  }

  private ensureAcpSessionRecord(sessionId: string): AcpSessionRecord {
    let record = this.acpSessions.get(sessionId);
    if (!record) {
      const now = new Date().toISOString();
      record = { title: "", createdAt: now, updatedAt: now, messages: [] };
      this.acpSessions.set(sessionId, record);
    }
    return record;
  }

  private wireSessionFor(sessionId: string, record: AcpSessionRecord): WireSession {
    return {
      id: sessionId,
      title: record.title,
      createdAt: record.createdAt,
      updatedAt: record.updatedAt,
      lastMessageAt: record.updatedAt,
    };
  }

  private renderShell(webview: vscode.Webview): string {
    const scriptUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "chat", "webview.js"),
    );
    const styleUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "chat", "webview.css"),
    );
    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ${webview.cspSource} data:; style-src ${webview.cspSource} 'unsafe-inline'; font-src ${webview.cspSource}; script-src ${webview.cspSource};">
  <link rel="stylesheet" href="${styleUri.toString()}">
</head>
<body class="vscode-chat-shell">
  <div id="root"></div>
  <script src="${scriptUri.toString()}"></script>
</body>
</html>`;
  }
}

function toWireSuspendedApproval(request: AcpApprovalRequest): WireSuspendedApproval {
  return {
    approvalId: request.approvalId,
    title: request.title,
    toolsName: request.toolsName,
    toolName: request.toolName,
    policyName: request.policyName,
    policyPath: request.policyPath,
    details: request.details,
  };
}

function toWireSession(info: AcpSessionInfo): WireSession {
  const updatedAt = info.updatedAt ?? new Date().toISOString();
  return {
    id: info.sessionId,
    title: info.title || info.sessionId,
    createdAt: updatedAt,
    updatedAt,
    lastMessageAt: info.updatedAt ?? undefined,
  };
}

function replaceAt<T>(items: T[], index: number, value: T): T[] {
  const next = items.slice();
  next[index] = value;
  return next;
}

function resolveApprovalOutcome(request: AcpApprovalRequest, optionId: string | undefined): AcpApprovalResponse {
  if (optionId) {
    const exact = request.options.find((option) => option.id === optionId);
    if (exact) {
      return { outcome: { outcome: "selected", optionId: exact.id } };
    }
  }
  const deny = request.options.find((option) => option.id === "deny" || option.kind.startsWith("reject"));
  if (deny) {
    return { outcome: { outcome: "selected", optionId: deny.id } };
  }
  return { outcome: { outcome: "cancelled" } };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

async function readFileIfExists(uri: vscode.Uri): Promise<string | undefined> {
  try {
    const bytes = await vscode.workspace.fs.readFile(uri);
    return Buffer.from(bytes).toString("utf8");
  } catch {
    return undefined;
  }
}

// applyFullFileEdit writes `content` as the target file's entire contents,
// via a WorkspaceEdit (undoable, and reuses an already-open dirty editor)
// rather than a raw fs write -- keeps "Apply" reviewable/undoable like any
// other editor change, matching this feature's diff-first framing.
async function applyFullFileEdit(uri: vscode.Uri, content: string): Promise<void> {
  const existing = await readFileIfExists(uri);
  if (existing === undefined) {
    await vscode.workspace.fs.writeFile(uri, Buffer.from(content, "utf8"));
    await vscode.window.showTextDocument(uri);
    return;
  }
  const document = await vscode.workspace.openTextDocument(uri);
  const fullRange = new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length));
  const edit = new vscode.WorkspaceEdit();
  edit.replace(uri, fullRange, content);
  await vscode.workspace.applyEdit(edit);
  await vscode.window.showTextDocument(document);
}
