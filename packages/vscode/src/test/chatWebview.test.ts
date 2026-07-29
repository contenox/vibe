import * as assert from "node:assert/strict";
import * as vscode from "vscode";
import type {
  AcpApprovalRequest,
  AcpApprovalResponse,
  AcpChatClient,
  AcpCommandInfo,
  AcpDeltaEvent,
  AcpSendHandlers,
  AcpSendResult,
  AcpToolCallEvent,
  AcpUsageEvent,
} from "../acp/AcpChatClient";
import type { BridgeProcess } from "../bridge/BridgeProcess";
import { ChatWebviewViewProvider } from "../chat/ChatWebviewViewProvider";
import type { ChatHostToWebviewMessage, ChatWebviewToHostMessage } from "../chat/webviewProtocol";
import { DiffStore } from "../editor/diffStore";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

// ChatWebviewViewProvider's sendMessage/createSession/cancelTurn/approvalResponse
// now drive AcpChatClient (real ACP: session/new, session/prompt,
// session/update, session/request_permission, session/cancel) instead of the
// bespoke bridge -- see vscode-implementation-plan.md Phase 1. This fake
// mirrors AcpChatClient's public shape so these three tests keep exercising
// real provider behaviour (streaming, cancellation, approval round-trip)
// against the interface actually used in production, rather than the retired
// bridge/turnRunner path.
class FakeAcpChatClient {
  private readonly pending = new Map<
    string,
    { handlers: AcpSendHandlers; resolve: (result: AcpSendResult) => void }
  >();
  public readonly cancelledSessionIds: string[] = [];
  private sessionCounter = 0;

  public async createSession(): Promise<{ id: string }> {
    this.sessionCounter += 1;
    return { id: `session-${this.sessionCounter}` };
  }

  public sendMessage(
    sessionId: string,
    _content: string,
    _context: unknown[],
    handlers: AcpSendHandlers,
  ): Promise<AcpSendResult> {
    return new Promise<AcpSendResult>((resolve) => {
      this.pending.set(sessionId, { handlers, resolve });
    });
  }

  public cancelTurn(sessionId: string): void {
    this.cancelledSessionIds.push(sessionId);
  }

  public dispose(): void {}

  public hasPending(sessionId: string): boolean {
    return this.pending.has(sessionId);
  }

  public emitDelta(sessionId: string, event: AcpDeltaEvent): void {
    this.pending.get(sessionId)?.handlers.onDelta?.(event);
  }

  public emitToolCall(sessionId: string, event: AcpToolCallEvent): void {
    this.pending.get(sessionId)?.handlers.onToolCall?.(event);
  }

  public emitUsage(sessionId: string, event: AcpUsageEvent): void {
    this.pending.get(sessionId)?.handlers.onUsage?.(event);
  }

  public emitCommands(sessionId: string, commands: AcpCommandInfo[]): void {
    this.pending.get(sessionId)?.handlers.onCommands?.(commands);
  }

  public async triggerPermission(sessionId: string, request: AcpApprovalRequest): Promise<AcpApprovalResponse> {
    const handler = this.pending.get(sessionId)?.handlers.onPermissionRequested;
    assert.ok(handler, `expected a permission handler for ${sessionId}`);
    return handler(request, new AbortController().signal);
  }

  public completeTurn(sessionId: string, result: AcpSendResult): void {
    const pending = this.pending.get(sessionId);
    this.pending.delete(sessionId);
    pending?.resolve(result);
  }
}

function fakeWebviewView(receive: (cb: (message: ChatWebviewToHostMessage) => void) => void) {
  const posted: ChatHostToWebviewMessage[] = [];
  const view = {
    webview: {
      options: {},
      html: "",
      cspSource: "https://webview.test",
      asWebviewUri: (uri: vscode.Uri) => uri,
      postMessage: async (message: ChatHostToWebviewMessage) => {
        posted.push(message);
        return true;
      },
      onDidReceiveMessage: (cb: (message: ChatWebviewToHostMessage) => void) => {
        receive(cb);
        return { dispose: () => undefined };
      },
    },
  } as unknown as vscode.WebviewView;
  return { view, posted };
}

// In-memory stand-in for vscode.ExtensionContext.workspaceState -- these
// tests never restart the process, so a real Memento's on-disk persistence
// isn't exercised here (see resumeLateVerdict.vtest.ts for that).
function fakeMemento(): vscode.Memento {
  const store = new Map<string, unknown>();
  return {
    keys: () => Array.from(store.keys()),
    get: <T>(key: string, defaultValue?: T) => (store.has(key) ? (store.get(key) as T) : (defaultValue as T)),
    update: async (key: string, value: unknown) => {
      if (value === undefined) {
        store.delete(key);
      } else {
        store.set(key, value);
      }
    },
  } as vscode.Memento;
}

async function eventually(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.ok(predicate(), "condition was not met before timeout");
}

function setup() {
  const acpClient = new FakeAcpChatClient();
  const bridge = {
    ensureStarted: async () => ({ initialize: { capabilities: { chat: true, sessionList: true } } }),
    currentClient: undefined,
  } as unknown as BridgeProcess;
  const telemetry = new TelemetryLogger("test");
  const output = new ContenoxOutput();
  const diffStore = new DiffStore(telemetry);
  const workspaceState = fakeMemento();
  const provider = new ChatWebviewViewProvider(
    bridge,
    acpClient as unknown as AcpChatClient,
    diffStore,
    vscode.Uri.file("/tmp/contenox-test"),
    output,
    telemetry,
    () => undefined,
    workspaceState,
  );
  return {
    acpClient,
    provider,
    teardown: () => {
      provider.dispose();
      diffStore.dispose();
      telemetry.dispose();
      output.dispose();
    },
  };
}

suite("ChatWebviewViewProvider", () => {
  test("sendMessage streams deltas and tool calls before resolving", async () => {
    const { acpClient, provider, teardown } = setup();
    let receiveMessage: ((message: ChatWebviewToHostMessage) => void) | undefined;
    const { view, posted } = fakeWebviewView((cb) => (receiveMessage = cb));
    provider.resolveWebviewView(view);
    assert.ok(receiveMessage, "onDidReceiveMessage should be registered");

    try {
      receiveMessage!({ type: "sendMessage", requestId: "req-1", id: "session-1", content: "hello", attachments: [] });
      await eventually(() => acpClient.hasPending("session-1"));

      acpClient.emitDelta("session-1", { content: "Hi " });
      acpClient.emitDelta("session-1", { content: "there" });
      acpClient.emitToolCall("session-1", {
        id: "call-1",
        title: "local_fs.read",
        status: "completed",
        toolName: "read",
      });
      acpClient.completeTurn("session-1", { cancelled: false, failed: false, content: "Hi there" });

      await eventually(() => posted.some((m) => m.type === "result"));

      const deltas = posted.filter((m) => m.type === "delta") as Array<{ type: "delta"; content?: string }>;
      assert.deepEqual(deltas.map((d) => d.content), ["Hi ", "there"]);

      const toolCalls = posted.filter((m) => m.type === "toolCall");
      assert.equal(toolCalls.length, 1);

      const result = posted.find((m) => m.type === "result") as { type: "result"; ok: boolean; value: unknown };
      assert.equal(result.ok, true);
      const value = result.value as { messages: Array<{ role: string; content: string; sessionId: string }> };
      // The ACP path doesn't return a full transcript from session/prompt (unlike
      // the old bridge's chatSend); the provider synthesizes it from the user
      // message plus the accumulated assistant deltas.
      assert.equal(value.messages.length, 2);
      assert.equal(value.messages[0].role, "user");
      assert.equal(value.messages[0].content, "hello");
      assert.equal(value.messages[1].role, "assistant");
      assert.equal(value.messages[1].content, "Hi there");
      assert.equal(value.messages[1].sessionId, "session-1");
    } finally {
      teardown();
    }
  });

  test("cancelTurn cancels the in-flight ACP turn by session id", async () => {
    const { acpClient, provider, teardown } = setup();
    let receiveMessage: ((message: ChatWebviewToHostMessage) => void) | undefined;
    const { view, posted } = fakeWebviewView((cb) => (receiveMessage = cb));
    provider.resolveWebviewView(view);

    try {
      receiveMessage!({ type: "sendMessage", requestId: "req-2", id: "session-2", content: "hello", attachments: [] });
      await eventually(() => acpClient.hasPending("session-2"));

      receiveMessage!({ type: "cancelTurn", id: "session-2" });
      await eventually(() => acpClient.cancelledSessionIds.length === 1);
      assert.equal(acpClient.cancelledSessionIds[0], "session-2");

      // A cancelled turn is not an error (vscode-implementation-plan.md
      // Phase 2 "turn states"): it must resolve like any other completed
      // turn (ok: true), with the assistant message's stopReason carrying
      // "cancelled" for the transcript to render a neutral badge -- not a
      // rejected result the old "Cancelled" error-message behavior produced.
      acpClient.completeTurn("session-2", {
        cancelled: true,
        failed: false,
        content: "partial reply",
        stopReason: "cancelled",
      });
      await eventually(() => posted.some((m) => m.type === "result"));

      const result = posted.find((m) => m.type === "result") as { type: "result"; ok: boolean; value: unknown };
      assert.equal(result.ok, true, "a cancelled (non-failed) turn should resolve successfully, not reject");
      const value = result.value as { messages: Array<{ role: string; stopReason?: string; error?: string }> };
      const assistantMessage = value.messages[value.messages.length - 1];
      assert.equal(assistantMessage.stopReason, "cancelled");
      assert.equal(assistantMessage.error, undefined, "cancellation must not be carried as an `error`");
    } finally {
      teardown();
    }
  });

  test("a real failure (not a cancellation) still rejects with an error", async () => {
    const { acpClient, provider, teardown } = setup();
    let receiveMessage: ((message: ChatWebviewToHostMessage) => void) | undefined;
    const { view, posted } = fakeWebviewView((cb) => (receiveMessage = cb));
    provider.resolveWebviewView(view);

    try {
      receiveMessage!({ type: "sendMessage", requestId: "req-2b", id: "session-2b", content: "hello", attachments: [] });
      await eventually(() => acpClient.hasPending("session-2b"));

      acpClient.completeTurn("session-2b", { cancelled: false, failed: true, error: "boom", content: "" });
      await eventually(() => posted.some((m) => m.type === "result"));

      const result = posted.find((m) => m.type === "result") as { type: "result"; ok: boolean; error?: string };
      assert.equal(result.ok, false);
      assert.equal(result.error, "boom");
    } finally {
      teardown();
    }
  });

  test("usage_update and available_commands_update are forwarded to the webview", async () => {
    const { acpClient, provider, teardown } = setup();
    let receiveMessage: ((message: ChatWebviewToHostMessage) => void) | undefined;
    const { view, posted } = fakeWebviewView((cb) => (receiveMessage = cb));
    provider.resolveWebviewView(view);

    try {
      receiveMessage!({ type: "sendMessage", requestId: "req-4", id: "session-4", content: "hello", attachments: [] });
      await eventually(() => acpClient.hasPending("session-4"));

      // size 0 is a legitimate "no configured budget" wire value (Phase 2
      // "context/token usage") -- must still be forwarded, not dropped.
      acpClient.emitUsage("session-4", { used: 128, size: 0 });
      acpClient.emitCommands("session-4", [{ name: "model", description: "Switch model" }]);

      await eventually(() => posted.some((m) => m.type === "usage"));
      await eventually(() => posted.some((m) => m.type === "commands"));

      const usage = posted.find((m) => m.type === "usage") as { type: "usage"; used: number; size: number };
      assert.equal(usage.used, 128);
      assert.equal(usage.size, 0);

      const commands = posted.find((m) => m.type === "commands") as {
        type: "commands";
        commands: Array<{ name: string }>;
      };
      assert.deepEqual(
        commands.commands.map((c) => c.name),
        ["model"],
      );

      acpClient.completeTurn("session-4", { cancelled: false, failed: false, content: "hi" });
      await eventually(() => posted.some((m) => m.type === "result"));
    } finally {
      teardown();
    }
  });

  test("approval request round-trips the selected option back to the ACP client", async () => {
    const { acpClient, provider, teardown } = setup();
    let receiveMessage: ((message: ChatWebviewToHostMessage) => void) | undefined;
    const { view, posted } = fakeWebviewView((cb) => (receiveMessage = cb));
    provider.resolveWebviewView(view);

    try {
      receiveMessage!({ type: "sendMessage", requestId: "req-3", id: "session-3", content: "run a command", attachments: [] });
      await eventually(() => acpClient.hasPending("session-3"));

      const outcomePromise = acpClient.triggerPermission("session-3", {
        approvalId: "call-1",
        title: "local_shell.local_shell: rm file",
        options: [
          { id: "allow", label: "Allow", kind: "allow_once" },
          { id: "deny", label: "Deny", kind: "reject_once" },
        ],
      });

      await eventually(() => posted.some((m) => m.type === "approvalRequest"));
      const approvalRequest = posted.find((m) => m.type === "approvalRequest") as {
        type: "approvalRequest";
        requestId: string;
        request: { options: Array<{ id: string }> };
      };
      assert.deepEqual(
        approvalRequest.request.options.map((option) => option.id),
        ["allow", "deny"],
      );

      receiveMessage!({ type: "approvalResponse", requestId: approvalRequest.requestId, optionId: "allow" });

      const outcome = await outcomePromise;
      assert.deepEqual(outcome, { outcome: { outcome: "selected", optionId: "allow" } });

      acpClient.completeTurn("session-3", { cancelled: false, failed: false, content: "" });
      await eventually(() => posted.some((m) => m.type === "result"));
    } finally {
      teardown();
    }
  });
});
