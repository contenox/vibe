import * as vscode from "vscode";
import { BridgeClient } from "../bridge/BridgeClient";
import { BridgeProcess } from "../bridge/BridgeProcess";
import {
  ChatDeltaEvent,
  ChatLifecycleEvent,
  EditorContextAttachment,
  RequestPermissionParams,
  RequestPermissionResponse,
  SessionMessage,
  ToolCallEvent,
} from "../bridge/protocol";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

export interface TurnResult {
  failed: boolean;
  event: ChatLifecycleEvent;
}

export interface ChatTurnOptions {
  input: string;
  context?: EditorContextAttachment[];
  vars?: Record<string, string>;
  sessionId?: string;
  createSessionName?: string;
  reuseExistingSession?: boolean;
  token: vscode.CancellationToken;
  timeoutMs?: number;
}

export interface ChatTurnHandlers {
  onStarted?: (event: ChatLifecycleEvent) => void;
  onDelta?: (event: ChatDeltaEvent) => void;
  onToolCall?: (event: ToolCallEvent) => void;
  onPermissionRequested?: (
    client: BridgeClient,
    event: RequestPermissionParams,
    token: vscode.CancellationToken,
  ) => Promise<RequestPermissionResponse>;
  onCompletedWithoutDelta?: (event: ChatLifecycleEvent, content: string | undefined) => void;
}

const defaultChatTurnTimeoutMs = 10 * 60 * 1000;

export class ChatTurnRunner {
  public constructor(
    private readonly bridge: BridgeProcess,
    private readonly output: ContenoxOutput,
    private readonly telemetry: TelemetryLogger,
  ) {}

  public async run(options: ChatTurnOptions, handlers: ChatTurnHandlers = {}): Promise<TurnResult> {
    const state = await this.bridge.ensureStarted();
    if (!state.initialize.capabilities.chat) {
      throw new Error("This Contenox runtime does not support chat");
    }
    const client = this.bridge.currentClient;
    if (!client) {
      throw new Error("Contenox runtime connection is not available");
    }

    const sessionId = await this.resolveSession(client, options);
    let expectedSessionId = sessionId;
    let expectedTurnId: string | undefined;
    let wroteAssistantText = false;
    let settled = false;
    let timeout: NodeJS.Timeout | undefined;
    let resolveDone: ((result: TurnResult) => void) | undefined;
    const done = new Promise<TurnResult>((resolve) => {
      resolveDone = resolve;
    });
    const finish = (result: TurnResult) => {
      if (settled) {
        return;
      }
      settled = true;
      if (timeout) {
        clearTimeout(timeout);
        timeout = undefined;
      }
      resolveDone?.(result);
    };
    timeout = setTimeout(() => {
      const event: ChatLifecycleEvent = {
        sessionId: expectedSessionId,
        turnId: expectedTurnId ?? "unknown",
        error: "Timed out waiting for Contenox chat completion",
      };
      this.telemetry.warn("chat.turn.timeout", {
        sessionId: expectedSessionId,
        turnId: expectedTurnId,
        timeoutMs: options.timeoutMs ?? defaultChatTurnTimeoutMs,
      });
      this.output.warn("Timed out waiting for Contenox chat completion");
      if (expectedTurnId) {
        void client.chatCancel({ turnId: expectedTurnId });
      }
      finish({ failed: true, event });
    }, options.timeoutMs ?? defaultChatTurnTimeoutMs);

    const disposables = [
      client.onChatStarted((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId)) {
          return;
        }
        expectedTurnId = event.turnId;
        handlers.onStarted?.(event);
      }),
      client.onChatDelta((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId)) {
          return;
        }
        expectedTurnId = event.turnId;
        if (event.content) {
          wroteAssistantText = true;
        }
        handlers.onDelta?.(event);
      }),
      client.onToolCall((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId)) {
          return;
        }
        expectedTurnId = event.turnId;
        handlers.onToolCall?.(event);
      }),
      handlers.onPermissionRequested
        ? client.pushPermissionRequestHandler(expectedSessionId, (event, permissionToken) =>
            handlers.onPermissionRequested?.(client, event, permissionToken) ?? Promise.resolve({ outcome: { outcome: "cancelled" } }),
          )
        : undefined,
      client.onChatCompleted((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId) || settled) {
          return;
        }
        expectedTurnId = event.turnId;
        if (!wroteAssistantText) {
          handlers.onCompletedWithoutDelta?.(event, lastAssistantMessage(event));
        }
        finish({ failed: false, event });
      }),
      client.onChatFailed((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId) || settled) {
          return;
        }
        expectedTurnId = event.turnId;
        finish({ failed: true, event });
      }),
      client.onChatCancelled((event) => {
        if (!matchesTurn(event, expectedSessionId, expectedTurnId) || settled) {
          return;
        }
        expectedTurnId = event.turnId;
        finish({ failed: true, event });
      }),
    ].filter(isDisposable);

    try {
      const sent = await client.chatSend({
        sessionId,
        input: options.input,
        context: options.context ?? [],
        vars: options.vars,
      });
      expectedSessionId = sent.sessionId;
      expectedTurnId = expectedTurnId ?? sent.turnId;
      const cancellation = options.token.onCancellationRequested(() => {
        if (expectedTurnId) {
          void client.chatCancel({ turnId: expectedTurnId });
        }
      });
      try {
        return await done;
      } finally {
        cancellation.dispose();
      }
    } finally {
      if (timeout) {
        clearTimeout(timeout);
      }
      for (const disposable of disposables) {
        disposable.dispose();
      }
    }
  }

  private async resolveSession(client: BridgeClient, options: ChatTurnOptions): Promise<string> {
    if (options.sessionId) {
      return options.sessionId;
    }
    if (options.reuseExistingSession ?? true) {
      const sessions = await client.sessionList();
      const active = sessions.sessions.find((session) => session.isActive) ?? sessions.sessions[0];
      if (active) {
        return active.id;
      }
    }
    const created = await client.sessionCreate({ name: options.createSessionName ?? "vscode-chat" });
    return created.session.id;
  }
}

function matchesTurn(event: ChatLifecycleEvent | ChatDeltaEvent | ToolCallEvent, sessionId: string, turnId: string | undefined): boolean {
  return event.sessionId === sessionId && (!turnId || event.turnId === turnId);
}

function lastAssistantMessage(event: ChatLifecycleEvent): string | undefined {
  const messages: readonly SessionMessage[] = event.messages ?? [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message.role === "assistant" && message.content) {
      return message.content;
    }
  }
  return undefined;
}

function isDisposable(value: vscode.Disposable | undefined): value is vscode.Disposable {
  return Boolean(value);
}
