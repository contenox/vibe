import * as fs from "node:fs";
import { spawn } from "node:child_process";
import * as vscode from "vscode";
import { AcpChatClient } from "./acp/AcpChatClient";
import { loadAcpSdk } from "./acp/sdk";
import {
  registerAutocomplete,
  testAutocompleteAtCursor,
} from "./autocomplete/provider";
import { AutocompleteStatus } from "./autocomplete/status";
import { bridgeCommandArgs, BridgeProcess } from "./bridge/BridgeProcess";
import { SessionMessage, SessionResult } from "./bridge/protocol";
import { ChatWebviewViewProvider } from "./chat/ChatWebviewViewProvider";
import {
  SessionDocumentProvider,
  sessionDocumentScheme,
} from "./chat/SessionDocumentProvider";
import { InboxTreeNode, InboxTreeProvider } from "./chat/InboxTreeProvider";
import { SessionTreeProvider } from "./chat/SessionTreeProvider";
import { registerDiagnosticCodeActions } from "./codeActions/diagnostics";
import { registerApprovalTool } from "./approval/nativeTool";
import { RuntimeControlsViewProvider } from "./config/RuntimeControlsView";
import {
  selectAutocompleteModel,
  selectAutocompleteProvider,
  selectChatModel,
  selectHitlPolicy,
  selectProvider,
  selectThinkLevel,
} from "./config/selectors";
import { readBridgeSettings } from "./config/settings";
import { DiffStore, OpenDiffArgs, StoredDiff } from "./editor/diffStore";
import {
  registerLanguageModelProvider,
  testLanguageModelProvider,
} from "./lm/provider";
import { ContenoxOutput } from "./logging/output";
import { TelemetryLogger } from "./logging/telemetry";
import {
  MCPServerProviderRegistration,
  registerMCPServerProvider,
  showMCPServers,
} from "./mcp/provider";
import { setDiagnosticsContext } from "./status/contextKeys";
import { ContenoxStatusBar } from "./status/statusBar";

// Minimal test-only surface, returned as `activate()`'s exports (read via
// `vscode.Extension.activate()`'s resolved value / `.exports`). Exists so the
// visual suite can delete every session it creates in teardown
// (AcpChatClient.deleteSession) without going through the
// contenox.deleteSession command's modal confirmation dialog, which would
// hang a headless run. Not part of the extension's public API.
export interface ContenoxTestApi {
  deleteSessionForTest(sessionId: string): Promise<void>;
  // Exposes the exact ContentBlock[] built for the most recent session/prompt
  // call, so the visual suite can assert Phase 3's outgoing wire shape (e.g.
  // a resource_link block for an @-attached file) rather than only the
  // webview DOM (vscode-implementation-plan.md Phase 3 acceptance gate).
  lastPromptBlocksForTest(): unknown;
}

export function activate(context: vscode.ExtensionContext): ContenoxTestApi {
  const output = new ContenoxOutput();
  // Load the ACP SDK: the chat panel's send path drives `contenox acp`
  // directly through it (AcpChatClient below) -- see
  // docs/development/internal/vscode-implementation-plan.md (Phase 1) and
  // acp-client-ts-spike.md.
  void loadAcpSdk()
    .then((acp) => output.info(`[acp] sdk loaded (protocol v${acp.PROTOCOL_VERSION})`))
    .catch((error) => output.warn(`[acp] sdk load failed: ${errorMessage(error)}`));
  const telemetry = new TelemetryLogger(extensionVersion(context));
  const status = new ContenoxStatusBar();
  const autocompleteStatus = new AutocompleteStatus();
  const bridge = new BridgeProcess(
    output,
    status,
    extensionVersion(context),
    context.extensionUri,
    telemetry,
  );
  const acpChatClient = new AcpChatClient(context.extensionUri, extensionVersion(context), output);
  let hasShownSetupPrompt = false;
  const sessions = new SessionTreeProvider(acpChatClient);
  const inbox = new InboxTreeProvider(context.extensionUri, output);
  const sessionDocuments = new SessionDocumentProvider(telemetry);
  const diffStore = new DiffStore(telemetry);
  let chatWebview!: ChatWebviewViewProvider;
  const onWorkspaceDataChanged = () => {
    sessions.refresh();
    void chatWebview.refreshRuntimeSummary();
  };
  chatWebview = new ChatWebviewViewProvider(
    bridge,
    acpChatClient,
    diffStore,
    context.extensionUri,
    output,
    telemetry,
    onWorkspaceDataChanged,
    context.workspaceState,
  );
  const runtimeControls = new RuntimeControlsViewProvider(acpChatClient, telemetry, onWorkspaceDataChanged);
  const mcpProvider = registerMCPServerProvider(bridge, telemetry);
  telemetry.event(
    "extension.activated",
    collectExtensionRuntimeInfo(context, bridge, telemetry),
  );

  context.subscriptions.push(
    output,
    telemetry,
    status,
    autocompleteStatus,
    bridge,
    acpChatClient,
    chatWebview,
    diffStore,
    sessions,
    runtimeControls,
    sessionDocuments,
    vscode.workspace.registerTextDocumentContentProvider(
      "contenox-diff",
      diffStore,
    ),
    vscode.workspace.registerTextDocumentContentProvider(
      sessionDocumentScheme(),
      sessionDocuments,
    ),
    registerLanguageModelProvider(bridge, output, telemetry),
    mcpProvider,
    registerAutocomplete(bridge, output, telemetry),
    registerDiagnosticCodeActions(telemetry),
    registerApprovalTool(telemetry),
  );
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(
      "contenox.controls",
      runtimeControls,
    ),
  );
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider("contenox.chat", chatWebview),
  );
  context.subscriptions.push(
    vscode.window.registerTreeDataProvider("contenox.sessions", sessions),
  );
  // Phase 6 "operator inbox": activity-bar badge + tree of pending
  // approvals/questions and unsupervised mission reports, answerable
  // without a live turn. Polling (not push) since the backing stores are
  // CLI-shelled, not a live subscription (see InboxTreeProvider's header).
  const inboxView = vscode.window.createTreeView("contenox.inbox", {
    treeDataProvider: inbox,
  });
  inbox.attachView(inboxView);
  inbox.startPolling();
  context.subscriptions.push(inboxView, inbox);
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((event) => {
      if (
        event.affectsConfiguration("contenox.autocomplete") ||
        event.affectsConfiguration("contenox.autocompleteProvider") ||
        event.affectsConfiguration("contenox.autocompleteModel")
      ) {
        autocompleteStatus.update();
      }
    }),
  );
  context.subscriptions.push(
    vscode.commands.registerCommand("contenox.openChat", () =>
      chatWebview.openChat(),
    ),
    // Opens the quick runtime settings picker (also triggered from the chat header chip).
    vscode.commands.registerCommand("contenox.openRuntimeSettings", () =>
      chatWebview.showRuntimeSettingsPicker(),
    ),
    vscode.commands.registerCommand("contenox.openWalkthrough", () =>
      openWalkthrough(telemetry),
    ),
    vscode.commands.registerCommand(
      "contenox.internal.setupComplete",
      () => undefined,
    ),
    vscode.commands.registerCommand("contenox.askSelection", () =>
      chatWebview.askSelection(),
    ),
    vscode.commands.registerCommand("contenox.fixSelection", () =>
      chatWebview.fixSelection(),
    ),
    vscode.commands.registerCommand("contenox.addSelectionToChat", () =>
      chatWebview.addSelectionToChat(),
    ),
    vscode.commands.registerCommand(
      "contenox.fixDiagnostics",
      (diagnostics?: readonly vscode.Diagnostic[]) =>
        chatWebview.fixDiagnostics(diagnostics),
    ),
    vscode.commands.registerCommand(
      "contenox.explainDiagnostics",
      (diagnostics?: readonly vscode.Diagnostic[]) =>
        chatWebview.explainDiagnostics(diagnostics),
    ),
    vscode.commands.registerCommand("contenox.reviewChanges", () =>
      chatWebview.reviewChanges(),
    ),
    vscode.commands.registerCommand("contenox.draftCommitMessage", () =>
      chatWebview.draftCommitMessage(),
    ),
    vscode.commands.registerCommand("contenox.refreshSessions", () =>
      sessions.refresh(),
    ),
    vscode.commands.registerCommand("contenox.openSession", (arg?: unknown) =>
      openSession(acpChatClient, chatWebview, sessions, output, telemetry, arg),
    ),
    vscode.commands.registerCommand("contenox.openSessionTranscript", (arg?: unknown) =>
      openSessionTranscript(acpChatClient, sessionDocuments, output, telemetry, arg),
    ),
    vscode.commands.registerCommand("contenox.deleteSession", (arg?: unknown) =>
      deleteSession(acpChatClient, chatWebview, sessions, output, telemetry, arg),
    ),
    vscode.commands.registerCommand("contenox.showStatus", () =>
      showStatus(bridge, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.showExtensionRuntimeInfo", () =>
      showExtensionRuntimeInfo(context, bridge, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.restartRuntime", () =>
      restartRuntime(bridge, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.restartBridge", () =>
      restartRuntime(bridge, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.runSetup", () =>
      runSetup(bridge, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.selectProvider", () =>
      runConfigSelector("select_provider", () => selectProvider(bridge), sessions, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.selectChatModel", () =>
      runConfigSelector("select_chat_model", () => selectChatModel(bridge), sessions, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.selectAutocompleteProvider", () =>
      selectAutocompleteProvider(bridge),
    ),
    vscode.commands.registerCommand("contenox.selectAutocompleteModel", () =>
      selectAutocompleteModel(bridge),
    ),
    vscode.commands.registerCommand("contenox.selectHitlPolicy", () =>
      runConfigSelector("select_hitl_policy", () => selectHitlPolicy(bridge), sessions, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.selectThinkLevel", () =>
      runConfigSelector("select_think_level", () => selectThinkLevel(bridge), sessions, chatWebview, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.triggerAutocomplete", () =>
      vscode.commands.executeCommand("editor.action.inlineSuggest.trigger"),
    ),
    vscode.commands.registerCommand("contenox.testAutocompleteAtCursor", () =>
      testAutocompleteAtCursor(bridge, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.enableAutocomplete", () =>
      setAutocompleteEnabled(true, autocompleteStatus),
    ),
    vscode.commands.registerCommand("contenox.disableAutocomplete", () =>
      setAutocompleteEnabled(false, autocompleteStatus),
    ),
    vscode.commands.registerCommand("contenox.toggleAutocomplete", () =>
      toggleAutocomplete(autocompleteStatus),
    ),
    vscode.commands.registerCommand("contenox.acceptAutocomplete", () =>
      output.info("Contenox autocomplete accepted"),
    ),
    vscode.commands.registerCommand("contenox.showOutput", () => output.show()),
    vscode.commands.registerCommand("contenox.showTelemetryLog", () =>
      telemetry.show(),
    ),
    vscode.commands.registerCommand("contenox.clearTelemetryLog", () =>
      telemetry.clear(),
    ),
    vscode.commands.registerCommand("contenox.testLanguageModelProvider", () =>
      testLanguageModelProvider(output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.showMCPServers", () =>
      showMCPServers(bridge, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.refreshMCPServers", () =>
      refreshMCPServers(mcpProvider),
    ),
    vscode.commands.registerCommand(
      "contenox.openToolDiff",
      (arg?: OpenDiffArgs | StoredDiff) => openToolDiff(diffStore, arg),
    ),
    vscode.commands.registerCommand("contenox.doctor", () =>
      runDoctor(bridge, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.refreshDefaults", async () => {
      await ensureInitUpdate(bridge, output, telemetry);
      vscode.window.showInformationMessage("Contenox defaults refreshed via init --update.");
      // Restart bridge so new chains/policies take effect immediately
      try {
        await bridge.restart();
        await chatWebview.refreshRuntimeSummary();
      } catch (e) {
        output.warn(`refreshDefaults restart warning: ${errorMessage(e)}`);
      }
    }),
  );
  context.subscriptions.push(
    vscode.commands.registerCommand("contenox.inbox.refresh", () => inbox.refresh()),
    vscode.commands.registerCommand("contenox.inbox.approve", (node?: InboxTreeNode) =>
      inboxRespond(inbox, node, "approve", output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.inbox.deny", (node?: InboxTreeNode) =>
      inboxRespond(inbox, node, "deny", output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.inbox.answer", (node?: InboxTreeNode) =>
      inboxAnswer(inbox, node, output, telemetry),
    ),
    vscode.commands.registerCommand("contenox.inbox.ack", (node?: InboxTreeNode) =>
      inboxAck(inbox, node, output, telemetry),
    ),
  );
  updateDiagnosticsContext();
  context.subscriptions.push(
    vscode.window.onDidChangeActiveTextEditor(() => updateDiagnosticsContext()),
    vscode.languages.onDidChangeDiagnostics((event) => {
      const active = vscode.window.activeTextEditor?.document.uri;
      if (
        !active ||
        event.uris.some((uri) => uri.toString() === active.toString())
      ) {
        updateDiagnosticsContext();
      }
    }),
  );

  if (readBridgeSettings().startOnActivation) {
    void bridge
      .ensureStarted()
      .then(async (state) => {
        chatWebview.refreshRuntimeSummary();
        if (!state.health.configured && !hasShownSetupPrompt) {
          hasShownSetupPrompt = true;
          // Proactive but non-intrusive for new store users (addresses "no way to setup")
          const remoteNote = vscode.env.remoteName ? ` (on ${vscode.env.remoteName})` : "";
          const choice = await vscode.window.showInformationMessage(
            `Contenox runtime needs setup${remoteNote} (choose provider + model).`,
            "Run Guided Setup",
            "Show Status",
            "Open Walkthrough",
          );
          if (choice === "Run Guided Setup") {
            vscode.commands.executeCommand("contenox.runSetup");
          } else if (choice === "Show Status") {
            vscode.commands.executeCommand("contenox.showStatus");
          } else if (choice === "Open Walkthrough") {
            vscode.commands.executeCommand("contenox.openWalkthrough");
          }
        }
      })
      .catch((error) => {
        output.warn(errorMessage(error));
      });
  }

  return {
    deleteSessionForTest: (sessionId: string) => acpChatClient.deleteSession(sessionId),
    lastPromptBlocksForTest: () => acpChatClient.getLastPromptBlocksForTest(),
  };
}

function updateDiagnosticsContext(): void {
  const active = vscode.window.activeTextEditor?.document.uri;
  const hasDiagnostics = Boolean(
    active && vscode.languages.getDiagnostics(active).length > 0,
  );
  void setDiagnosticsContext(hasDiagnostics);
}

function openWalkthrough(telemetry: TelemetryLogger): Thenable<unknown> {
  telemetry.event("command.open_walkthrough");
  return vscode.commands.executeCommand(
    "workbench.action.openWalkthrough",
    "contenox.contenox-runtime#getStarted",
    false,
  );
}

export function deactivate(): void {
  // VS Code disposes extension subscriptions after deactivate returns.
}

async function showStatus(
  bridge: BridgeProcess,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  try {
    await bridge.ensureStarted();
    const health = await bridge.refreshHealth();
    const provider = health.defaultProvider || "no provider";
    const model = health.defaultModel || "no model";
    telemetry.event("command.show_status", {
      status: health.status,
      configured: health.configured,
      provider,
      model,
    });
    vscode.window.showInformationMessage(
      `Contenox ${health.status}: ${provider} / ${model}. Use "Contenox: Doctor" for detailed checks.`,
    );
  } catch (error) {
    telemetry.error("command.show_status.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function showExtensionRuntimeInfo(
  context: vscode.ExtensionContext,
  bridge: BridgeProcess,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  const info = collectExtensionRuntimeInfo(context, bridge, telemetry);
  telemetry.event("command.show_extension_runtime_info", info);
  output.info(`Contenox runtime info:\n${JSON.stringify(info, null, 2)}`);
  output.show();
  vscode.window.showInformationMessage(
    `Contenox ${info.extensionVersion} loaded from ${info.extensionPath}. Full details are in the Contenox output.`,
  );
}

async function restartRuntime(
  bridge: BridgeProcess,
  chatWebview: ChatWebviewViewProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  try {
    telemetry.event("command.restart_runtime");
    const state = await bridge.restart();
    await chatWebview.refreshRuntimeSummary();
    const provider = state.health.defaultProvider || "no provider";
    const model = state.health.defaultModel || "no model";
    vscode.window.showInformationMessage(
      `Contenox runtime restarted: ${provider} / ${model}`,
    );
  } catch (error) {
    telemetry.error("command.restart_runtime.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

/**
 * Runs `contenox init --update` (non-destructive refresh of default chains/policies/workspace files).
 * Streams output to the Contenox channel. Safe to call on fresh or existing installs.
 */
async function ensureInitUpdate(bridge: BridgeProcess, output: ContenoxOutput, telemetry: TelemetryLogger): Promise<void> {
  const binary = bridge.commandBinaryPath();
  const settings = readBridgeSettings();
  const cwd = bridge.commandCwd();
  const args: string[] = [];
  if (settings.dataDir) {
    args.push("--data-dir", settings.dataDir);
  }
  args.push("init", "--update");

  telemetry.event("setup.ensure_init_update", { binary, args });

  output.info(`Refreshing Contenox defaults (init --update): ${binary} ${args.join(" ")}`);

  return new Promise((resolve) => {
    const child = spawn(binary, args, {
      cwd: cwd || undefined,
      env: { ...process.env, NO_COLOR: "1" },
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });

    child.stdout.on("data", (chunk: Buffer) => {
      const text = chunk.toString("utf8").trimEnd();
      if (text) output.info(`[init --update] ${text}`);
    });
    child.stderr.on("data", (chunk: Buffer) => {
      const text = chunk.toString("utf8").trimEnd();
      if (text) output.warn(`[init --update] ${text}`);
    });
    child.on("error", (err) => {
      output.warn(`init --update spawn error: ${err.message}`);
      // non-fatal for guided flow
      resolve();
    });
    child.on("close", (code) => {
      output.info(`init --update completed (code ${code})`);
      resolve();
    });
  });
}

function runSetup(
  bridge: BridgeProcess,
  chatWebview: ChatWebviewViewProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): void {
  const settings = readBridgeSettings();
  const binary = bridge.commandBinaryPath();
  const args = bridgeCommandArgs(settings.dataDir, "setup");
  telemetry.event("command.run_setup", {
    binary,
    args,
    cwd: bridge.commandCwd(),
    dataDir: settings.dataDir,
  });

  // First ensure defaults are fresh (init --update is safe + addresses stale chains/policies)
  void ensureInitUpdate(bridge, output, telemetry).finally(() => {
    const terminal = vscode.window.createTerminal({
      name: "Contenox Setup",
      cwd: bridge.commandCwd(),
    });
    const closeSubscription = vscode.window.onDidCloseTerminal((closed) => {
      if (closed !== terminal) {
        return;
      }
      closeSubscription.dispose();
      telemetry.event("command.run_setup.terminal_closed", {
        exitCode: terminal.exitStatus?.code,
        reason: terminal.exitStatus?.reason,
      });
      void vscode.commands.executeCommand("contenox.internal.setupComplete");
      void bridge
        .restart()
        .then(async (state) => {
          await chatWebview.refreshRuntimeSummary();
          vscode.window.showInformationMessage(
            `Contenox setup finished. Runtime refreshed: ${state.health.defaultProvider || "no provider"} / ${state.health.defaultModel || "no model"}`,
          );
        })
        .catch((error) => {
          telemetry.error("command.run_setup.refresh_failed", error);
          output.show();
          vscode.window.showErrorMessage(
            `Contenox setup finished, but runtime refresh failed: ${errorMessage(error)}`,
          );
        });
    });
    terminal.show();
    terminal.sendText([shellQuote(binary), ...args.map(shellQuote)].join(" "));
  });
}

async function toggleAutocomplete(status: AutocompleteStatus): Promise<void> {
  const enabled = !vscode.workspace
    .getConfiguration("contenox")
    .get<boolean>("autocomplete.enabled", false);
  await setAutocompleteEnabled(enabled, status);
}

async function setAutocompleteEnabled(
  enabled: boolean,
  status: AutocompleteStatus,
): Promise<void> {
  await vscode.workspace
    .getConfiguration("contenox")
    .update(
      "autocomplete.enabled",
      enabled,
      vscode.ConfigurationTarget.Workspace,
    );
  status.update();
  vscode.window.showInformationMessage(
    `Contenox autocomplete ${enabled ? "enabled" : "disabled"}`,
  );
}

// inboxRespond/inboxAnswer/inboxAck back the "answerable outside a live
// turn" requirement (Phase 6): the operator inbox tree, not the chat panel,
// is where a parked approval or an unsupervised mission report gets acted on
// when nobody has a session open at all.
async function inboxRespond(
  inbox: InboxTreeProvider,
  node: InboxTreeNode | undefined,
  verdict: "approve" | "deny",
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  if (!node || node.kind !== "approval") return;
  try {
    telemetry.event("command.inbox_respond", { verdict, kind: node.ask.kind });
    if (verdict === "approve") {
      await inbox.approve(node.ask);
    } else {
      await inbox.deny(node.ask);
    }
  } catch (error) {
    telemetry.error("command.inbox_respond.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function inboxAnswer(
  inbox: InboxTreeProvider,
  node: InboxTreeNode | undefined,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  if (!node || node.kind !== "approval") return;
  const text = await vscode.window.showInputBox({
    prompt: `Answer: ${node.ask.summary || node.ask.tool}`,
    placeHolder: "Your answer",
  });
  if (text === undefined || text.trim() === "") return;
  try {
    telemetry.event("command.inbox_answer");
    await inbox.answer(node.ask, text.trim());
  } catch (error) {
    telemetry.error("command.inbox_answer.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function inboxAck(
  inbox: InboxTreeProvider,
  node: InboxTreeNode | undefined,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  if (!node || node.kind !== "inboxItem") return;
  try {
    telemetry.event("command.inbox_ack");
    await inbox.ack(node.item);
  } catch (error) {
    telemetry.error("command.inbox_ack.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function openSession(
  acpClient: AcpChatClient,
  chatWebview: ChatWebviewViewProvider,
  sessions: SessionTreeProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
  arg: unknown,
): Promise<void> {
  try {
    const sessionId = sessionIdFromArg(arg);
    if (!sessionId) {
      await chatWebview.openChat();
      return;
    }
    telemetry.event("command.open_session", { sessionId });
    await acpClient.loadSession(sessionId);
    await chatWebview.openSession(sessionId);
    sessions.refresh();
  } catch (error) {
    telemetry.error("command.open_session.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function openSessionTranscript(
  acpClient: AcpChatClient,
  sessionDocuments: SessionDocumentProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
  arg: unknown,
): Promise<void> {
  try {
    const sessionId = sessionIdFromArg(arg);
    if (!sessionId) {
      vscode.window.showWarningMessage("No Contenox session is selected for a transcript.");
      return;
    }
    telemetry.event("command.open_session_transcript", { sessionId });
    const loaded = await acpClient.loadSession(sessionId);
    await sessionDocuments.open(toSessionResult(sessionId, loaded));
  } catch (error) {
    telemetry.error("command.open_session_transcript.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

// toSessionResult adapts an ACP session/load replay into the SessionDocumentProvider's
// generic SessionResult/SessionMessage shape (unchanged since the bridge era --
// loose enough to fit both). Tool call replay events fold into brief
// standalone "tool" rows; full tool-call detail lives in the chat panel
// itself, not this read-only transcript export.
function toSessionResult(sessionId: string, loaded: Awaited<ReturnType<AcpChatClient["loadSession"]>>): SessionResult {
  const messages: SessionMessage[] = [];
  let seq = 0;
  for (const event of loaded.events) {
    seq += 1;
    if (event.kind === "message") {
      messages.push({
        id: `${sessionId}-${seq}`,
        role: event.role,
        content: event.text,
        timestamp: undefined,
      });
    } else {
      messages.push({
        id: `${sessionId}-${seq}`,
        role: "tool",
        content: event.event.output ?? event.event.error ?? "",
        toolCallId: event.event.id,
      });
    }
  }
  return {
    session: { id: sessionId, name: sessionId, messageCount: messages.length, isActive: false },
    messages,
  };
}

async function runConfigSelector(
  name: string,
  action: () => Promise<string | undefined>,
  sessions: SessionTreeProvider,
  chatWebview: ChatWebviewViewProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  try {
    const selected = await action();
    if (selected !== undefined) {
      sessions.refresh();
      await chatWebview.refreshRuntimeSummary();
    }
  } catch (error) {
    telemetry.error(`command.${name}.failed`, error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function deleteSession(
  acpClient: AcpChatClient,
  chatWebview: ChatWebviewViewProvider,
  sessions: SessionTreeProvider,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
  arg: unknown,
): Promise<void> {
  try {
    const sessionId = sessionIdFromArg(arg);
    if (!sessionId) {
      return;
    }
    const choice = await vscode.window.showWarningMessage(
      "Delete this Contenox session?",
      { modal: true },
      "Delete",
    );
    if (choice !== "Delete") {
      return;
    }
    telemetry.event("command.delete_session", { sessionId });
    await acpClient.deleteSession(sessionId);
    chatWebview.clearActiveSession(sessionId);
    sessions.refresh();
  } catch (error) {
    telemetry.error("command.delete_session.failed", error);
    output.show();
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

async function openToolDiff(
  diffStore: DiffStore,
  arg: OpenDiffArgs | StoredDiff | undefined,
): Promise<void> {
  if (!arg) {
    vscode.window.showWarningMessage("No Contenox diff is available to open.");
    return;
  }
  await diffStore.open(arg);
}

function runDoctor(
  bridge: BridgeProcess,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): void {
  const settings = readBridgeSettings();
  const binary = bridge.commandBinaryPath();
  const args = bridgeCommandArgs(settings.dataDir, "doctor");
  telemetry.event("command.doctor", { binary, args });

  // Surface doctor in its own terminal for rich text + hints (user-friendly)
  // Future: can parse --json and render structured with action buttons
  const terminal = vscode.window.createTerminal({
    name: "Contenox Doctor",
    cwd: bridge.commandCwd(),
  });
  terminal.show();
  terminal.sendText([shellQuote(binary), ...args.map(shellQuote)].join(" "));
  output.info("Contenox doctor launched in terminal (includes backend reachability + setup issues).");
}

function refreshMCPServers(provider: MCPServerProviderRegistration): void {
  provider.refresh();
  vscode.window.showInformationMessage(
    "Contenox MCP server definitions refreshed.",
  );
}

function sessionIdFromArg(arg: unknown): string | undefined {
  if (typeof arg === "string") {
    return arg;
  }
  if (arg && typeof arg === "object") {
    const maybe = arg as { session?: { id?: unknown }; id?: unknown };
    if (typeof maybe.session?.id === "string") {
      return maybe.session.id;
    }
    if (typeof maybe.id === "string") {
      return maybe.id;
    }
  }
  return undefined;
}

function extensionVersion(context: vscode.ExtensionContext): string {
  const version = (context.extension.packageJSON as { version?: unknown })
    .version;
  return typeof version === "string" ? version : "0.0.0";
}

function collectExtensionRuntimeInfo(
  context: vscode.ExtensionContext,
  bridge: BridgeProcess,
  telemetry: TelemetryLogger,
): ExtensionRuntimeInfo {
  const sessionTreeProviderPath = vscode.Uri.joinPath(
    context.extensionUri,
    "dist",
    "chat",
    "SessionTreeProvider.js",
  ).fsPath;
  const markers = inspectSessionTreeMarkers(sessionTreeProviderPath);
  return {
    extensionId: context.extension.id,
    extensionVersion: extensionVersion(context),
    extensionPath: context.extensionUri.fsPath,
    extensionMode: extensionModeName(context.extensionMode),
    vscodeVersion: vscode.version,
    uiKind: uiKindName(vscode.env.uiKind),
    remoteName: vscode.env.remoteName || "local",
    workspaceTrusted: vscode.workspace.isTrusted,
    runtimeBinaryPath: bridge.commandBinaryPath(),
    runtimeCwd: bridge.commandCwd() || "",
    telemetryLogPath: telemetry.logPath(),
    sessionTreeProviderPath,
    sessionTreeConfigRowsPresent: markers.present,
    sessionTreeMissingMarkers: markers.missing,
    sessionTreeInspectionError: markers.error,
  };
}

interface ExtensionRuntimeInfo extends Record<string, unknown> {
  extensionId: string;
  extensionVersion: string;
  extensionPath: string;
  extensionMode: string;
  vscodeVersion: string;
  uiKind: string;
  remoteName: string;
  workspaceTrusted: boolean;
  runtimeBinaryPath: string;
  runtimeCwd: string;
  telemetryLogPath: string;
  sessionTreeProviderPath: string;
  sessionTreeConfigRowsPresent: boolean;
  sessionTreeMissingMarkers: string[];
  sessionTreeInspectionError?: string;
}

function inspectSessionTreeMarkers(file: string): {
  present: boolean;
  missing: string[];
  error?: string;
} {
  const markers = ["contenoxSession", "listSessions", "openSession"];
  try {
    const content = fs.readFileSync(file, "utf8");
    const missing = markers.filter((marker) => !content.includes(marker));
    return { present: missing.length === 0, missing };
  } catch (error) {
    return { present: false, missing: markers, error: errorMessage(error) };
  }
}

function extensionModeName(mode: vscode.ExtensionMode): string {
  switch (mode) {
    case vscode.ExtensionMode.Development:
      return "development";
    case vscode.ExtensionMode.Test:
      return "test";
    case vscode.ExtensionMode.Production:
      return "production";
    default:
      return String(mode);
  }
}

function uiKindName(kind: vscode.UIKind): string {
  switch (kind) {
    case vscode.UIKind.Desktop:
      return "desktop";
    case vscode.UIKind.Web:
      return "web";
    default:
      return String(kind);
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function shellQuote(value: string): string {
  if (/^[A-Za-z0-9_./:=@%+-]+$/.test(value)) {
    return value;
  }
  return `'${value.replace(/'/g, "'\\''")}'`;
}
