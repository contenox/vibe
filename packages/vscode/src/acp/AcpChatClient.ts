// ACP-backed chat client: spawns `contenox acp` and drives it as the real ACP
// client role (session/new, session/prompt, session/update, session/cancel,
// session/request_permission, session/list, session/load, session/delete,
// session/set_config_option), replacing the bespoke bridge for the chat
// panel's send path, session lifecycle, and runtime controls. See
// docs/development/internal/vscode-implementation-plan.md (Phase 1) and
// acp-client-ts-spike.md for the wiring this mirrors (acpHandshake.vtest.ts is
// the proven reference).
import { ChildProcessWithoutNullStreams, spawn } from "node:child_process";
import { Readable, Writable } from "node:stream";
import * as vscode from "vscode";
import { bridgeCommandArgs, resolveBinaryPath, workspaceCwd } from "../bridge/BridgeProcess";
import { readAcpSettings, readBridgeSettings } from "../config/settings";
import { WireAttachment } from "../chat/webviewProtocol";
import { ContenoxOutput } from "../logging/output";
import { loadAcpSdk } from "./sdk";
import type {
  AcpSessionInfo,
  ActiveSession,
  ActiveSessionMessage,
  ClientContext,
  ContentBlock,
  PromptResponse,
  RequestPermissionRequest,
  RequestPermissionResponse,
  SessionConfigOption,
  SessionNotification,
  StopReason,
  ToolCall,
  ToolCallUpdate,
} from "./types";

export interface AcpDeltaEvent {
  content?: string;
  thinking?: string;
}

// Context window + cost, from usage_update -- see
// internal/surfaces/beamtui/enginebridge/events.go's UsageUpdated for the
// documented wire semantics: size 0 is a legitimate "no configured budget"
// value, never a divide-by-zero bug. Consumers must render Used absolutely
// when Size is 0, not as a ratio.
export interface AcpUsageEvent {
  used: number;
  size: number;
  cost?: { amount: number; currency: string };
}

// One agent-advertised slash command, from available_commands_update
// (full-replacement per turn -- the latest event wins, see
// enginebridge/events.go's CommandsUpdated). Invoking one is plain text
// through the normal prompt path; this is autocomplete metadata only.
export interface AcpCommandInfo {
  name: string;
  description: string;
  hint?: string;
}

export interface AcpToolCallEvent {
  id: string;
  title?: string;
  status: string;
  toolName?: string;
  output?: string;
  error?: string;
  diffPath?: string;
  diffOld?: string;
  diffNew?: string;
}

export interface AcpApprovalOption {
  id: string;
  label: string;
  kind: string;
}

export interface AcpApprovalRequest {
  approvalId: string;
  toolsName?: string;
  toolName?: string;
  title: string;
  policyName?: string;
  policyPath?: string;
  // Rule index within the named policy that produced this ask, when the
  // agent's _meta carries one (see approvalflow.Meta -- not populated by the
  // acpsvc transport today; read defensively in case that changes).
  matchedRule?: number;
  // The matched rule's human-readable cause (approvalflow.Meta.Detail),
  // e.g. `shell command "rm" matched command_ask_always` -- displaces
  // matchedRule in the rendered reason when present; see gateReason.
  matchedRuleDetail?: string;
  details?: string;
  diffOld?: string;
  diffNew?: string;
  options: AcpApprovalOption[];
}

export type AcpApprovalOutcome = { outcome: "selected"; optionId: string } | { outcome: "cancelled" };

export interface AcpApprovalResponse {
  outcome: AcpApprovalOutcome;
}

export interface AcpSendHandlers {
  onDelta?: (event: AcpDeltaEvent) => void;
  onToolCall?: (event: AcpToolCallEvent) => void;
  onPermissionRequested?: (request: AcpApprovalRequest, signal: AbortSignal) => Promise<AcpApprovalResponse>;
  // usage_update -- context window + cost, re-sent by the agent as it changes
  // during the turn (Phase 2 "context/token usage").
  onUsage?: (event: AcpUsageEvent) => void;
  // available_commands_update -- full-replacement slash-command menu (Phase 2
  // "slash commands"). May arrive before any delta, since the SDK buffers
  // notifications sent immediately after session/new.
  onCommands?: (commands: AcpCommandInfo[]) => void;
}

export interface AcpSendResult {
  cancelled: boolean;
  failed: boolean;
  error?: string;
  content: string;
  // The ACP StopReason for this turn ("end_turn", "max_tokens",
  // "max_turn_requests", "refusal", "cancelled") -- Phase 2 "turn states".
  // Undefined only when the turn failed before a PromptResponse ever arrived.
  stopReason?: StopReason;
}

// One event replayed from a loaded/resumed session's history, in wire order.
// ChatWebviewViewProvider folds these into WireMessage[]/WireToolCall[] --
// AcpChatClient stays protocol-shaped, not webview-shaped.
export type AcpReplayEvent =
  | { kind: "message"; role: "user" | "assistant"; messageId: string; text: string }
  | { kind: "toolCall"; event: AcpToolCallEvent };

export interface AcpLoadedSession {
  sessionId: string;
  configOptions?: SessionConfigOption[];
  events: AcpReplayEvent[];
}

export interface AcpSessionListResult {
  sessions: AcpSessionInfo[];
  nextCursor?: string;
}

// Minimal shape of the dynamically-loaded SDK surface this client actually
// uses -- keeps the class body free of `Awaited<ReturnType<typeof loadAcpSdk>>`
// noise while still being real, checked types (see acp/types.ts).
type AcpSdk = Awaited<ReturnType<typeof loadAcpSdk>>;

// A session obtained via session/new (ActiveSession, from the SDK's own
// helper) and one obtained via session/load (RawSession, hand-rolled below --
// the SDK's ActiveSession/SessionBuilder pair only wraps session/new) both
// expose this shape, so sendMessage/cancelTurn treat them uniformly.
interface SessionHandle {
  readonly sessionId: string;
  prompt(prompt: ContentBlock[]): Promise<PromptResponse>;
  nextUpdate(): Promise<ActiveSessionMessage>;
}

// Async queue of session/update notifications (plus a synthetic "stop" once
// the in-flight prompt resolves) for one session id -- mirrors the SDK's own
// internal AsyncQueue/ActiveSession.prompt() pairing (acp.js), reimplemented
// here because that pairing isn't exposed for a session obtained any way
// other than session/new.
class SessionUpdateQueue {
  private buffer: ActiveSessionMessage[] = [];
  private waiters: Array<{ resolve: (v: ActiveSessionMessage) => void; reject: (e: unknown) => void }> = [];
  private pendingError: { error: unknown } | undefined;

  public enqueue(message: ActiveSessionMessage): void {
    const waiter = this.waiters.shift();
    if (waiter) {
      waiter.resolve(message);
      return;
    }
    this.buffer.push(message);
  }

  public reject(error: unknown): void {
    const waiter = this.waiters.shift();
    if (waiter) {
      waiter.reject(error);
      return;
    }
    this.pendingError = { error };
  }

  public next(): Promise<ActiveSessionMessage> {
    const message = this.buffer.shift();
    if (message) {
      return Promise.resolve(message);
    }
    if (this.pendingError) {
      const { error } = this.pendingError;
      this.pendingError = undefined;
      return Promise.reject(error);
    }
    return new Promise((resolve, reject) => this.waiters.push({ resolve, reject }));
  }

  // Drains everything buffered so far without waiting -- used right after a
  // session/load response resolves, since the agent sends the whole replay as
  // notifications before that response (session.go's LoadSession calls
  // replayMessages synchronously, ahead of returning).
  public drainBuffered(): ActiveSessionMessage[] {
    const drained = this.buffer;
    this.buffer = [];
    return drained;
  }
}

class RawSession implements SessionHandle {
  public constructor(
    private readonly ctx: ClientContext,
    private readonly acp: AcpSdk,
    public readonly sessionId: string,
    private readonly queue: SessionUpdateQueue,
  ) {}

  public prompt(prompt: ContentBlock[]): Promise<PromptResponse> {
    const response = this.ctx.request(this.acp.methods.agent.session.prompt, {
      sessionId: this.sessionId,
      prompt,
    });
    void response.then(
      (value) => this.queue.enqueue({ kind: "stop", response: value, stopReason: value.stopReason }),
      (error: unknown) => this.queue.reject(error),
    );
    return response;
  }

  public nextUpdate(): Promise<ActiveSessionMessage> {
    return this.queue.next();
  }
}

export class AcpChatClient implements vscode.Disposable {
  private child: ChildProcessWithoutNullStreams | undefined;
  private acp: AcpSdk | undefined;
  private ctx: ClientContext | undefined;
  private starting: Promise<void> | undefined;
  private stopConnection: (() => void) | undefined;
  private disposed = false;
  private chainPath: string | undefined;
  private workspaceConfigOptions: SessionConfigOption[] | undefined;

  // The config options last returned by whichever session was most recently
  // created/loaded/reconfigured through this client -- what the runtime
  // controls view renders as "the current session's settings" (Phase 1 §2).
  private lastSessionId: string | undefined;
  private lastConfigOptions: SessionConfigOption[] | undefined;

  // The exact ContentBlock[] built for the most recent session/prompt call --
  // exposed only for the visual test suite (via ContenoxTestApi), so Phase 3
  // "real context attachment" can be asserted on the actual outgoing wire
  // shape (e.g. a resource_link block for an @-attached file), not just on
  // webview DOM (vscode-implementation-plan.md Phase 3 acceptance gate).
  private lastPromptBlocks: ContentBlock[] | undefined;

  private readonly sessions = new Map<string, SessionHandle>();
  // Update queues for sessions obtained via session/load, keyed by session id,
  // fed by the one connection-wide session/update handler registered in
  // start(). ActiveSession (session/new) sessions route internally in the SDK
  // and never need an entry here.
  private readonly rawQueues = new Map<string, SessionUpdateQueue>();
  private readonly permissionHandlers = new Map<
    string,
    (request: RequestPermissionRequest, signal: AbortSignal) => Promise<RequestPermissionResponse>
  >();

  public constructor(
    private readonly extensionUri: vscode.Uri,
    private readonly extensionVersion: string,
    private readonly output: ContenoxOutput,
  ) {}

  public ensureStarted(): Promise<void> {
    if (this.starting) {
      return this.starting;
    }
    this.starting = this.start().catch((error) => {
      this.starting = undefined;
      throw error;
    });
    return this.starting;
  }

  public async createSession(): Promise<{ id: string }> {
    await this.ensureStarted();
    const session = await this.newSession();
    this.sessions.set(session.sessionId, session);
    this.trackCurrentConfig(session.sessionId, session.newSessionResponse.configOptions ?? undefined);
    this.output.info(`[acp] session ${session.sessionId} started with chain=${this.chainDescription()}`);
    return { id: session.sessionId };
  }

  // getWorkspaceConfigOptions returns the session-less config options
  // advertised at initialize time (initialize's `_meta`), so the runtime
  // controls view has something real to render before any session exists.
  public getWorkspaceConfigOptions(): SessionConfigOption[] | undefined {
    return this.workspaceConfigOptions;
  }

  // getCurrentConfigOptions returns the config options of whichever session
  // was most recently created/loaded/reconfigured, or undefined if none yet.
  public getCurrentConfigOptions(): SessionConfigOption[] | undefined {
    return this.lastConfigOptions;
  }

  // setSessionConfigOption drives session/set_config_option for "the current
  // session" (lazily minting one if none exists yet), returning the resulting
  // full config option set the way the panel's runtime controls need it.
  public async setSessionConfigOption(configId: string, value: string): Promise<SessionConfigOption[]> {
    await this.ensureStarted();
    if (!this.ctx || !this.acp) {
      throw new Error("Contenox ACP connection is not available");
    }
    const sessionId = await this.ensureAnySessionId();
    const response = await this.ctx.request(this.acp.methods.agent.session.setConfigOption, {
      sessionId,
      configId,
      value,
    });
    this.trackCurrentConfig(sessionId, response.configOptions);
    return response.configOptions;
  }

  // listSessions drives session/list -- durable, persisted sessions (unlike
  // the pre-Phase-1 in-memory-only sessions this replaces).
  public async listSessions(cursor?: string): Promise<AcpSessionListResult> {
    await this.ensureStarted();
    if (!this.ctx || !this.acp) {
      throw new Error("Contenox ACP connection is not available");
    }
    const response = await this.ctx.request(this.acp.methods.agent.session.list, {
      cursor: cursor || undefined,
    });
    return { sessions: response.sessions, nextCursor: response.nextCursor ?? undefined };
  }

  // loadSession drives session/load: the agent replays the session's history
  // as session/update notifications before the request resolves (see
  // acpsvc/session.go's LoadSession), so this collects that replay into
  // AcpReplayEvent[] and binds the session for continued prompting.
  public async loadSession(sessionId: string): Promise<AcpLoadedSession> {
    await this.ensureStarted();
    if (!this.ctx || !this.acp) {
      throw new Error("Contenox ACP connection is not available");
    }
    const cwd = workspaceCwd() ?? process.cwd();
    const queue = new SessionUpdateQueue();
    this.rawQueues.set(sessionId, queue);
    try {
      const response = await this.ctx.request(this.acp.methods.agent.session.load, {
        sessionId,
        cwd,
        mcpServers: [],
      });
      const raw = new RawSession(this.ctx, this.acp, sessionId, queue);
      this.sessions.set(sessionId, raw);
      this.trackCurrentConfig(sessionId, response.configOptions ?? undefined);
      const events = replayEventsFromMessages(queue.drainBuffered());
      return { sessionId, configOptions: response.configOptions ?? undefined, events };
    } catch (error) {
      this.rawQueues.delete(sessionId);
      throw error;
    }
  }

  // resumeSession drives session/resume: rebinds a session server-side
  // without replaying its history (acpsvc/session.go's ResumeSession -- "the
  // client kept its transcript and only needs the server-side session
  // re-bound"). Used for Phase 5 "walk-away": after `contenox acp` is
  // respawned (a fresh process, e.g. VS Code was closed and reopened, or the
  // chain was switched), a session id the client already knows about is
  // otherwise unbound in the new process. Cheaper than loadSession when the
  // caller doesn't need a fresh replay -- e.g. just confirming a parked
  // session is still reachable before asking whether its approval resolved.
  public async resumeSession(sessionId: string): Promise<{ sessionId: string; configOptions?: SessionConfigOption[] }> {
    await this.ensureStarted();
    if (!this.ctx || !this.acp) {
      throw new Error("Contenox ACP connection is not available");
    }
    const cwd = workspaceCwd() ?? process.cwd();
    const queue = this.rawQueues.get(sessionId) ?? new SessionUpdateQueue();
    this.rawQueues.set(sessionId, queue);
    const response = await this.ctx.request(this.acp.methods.agent.session.resume, {
      sessionId,
      cwd,
      mcpServers: [],
    });
    const raw = new RawSession(this.ctx, this.acp, sessionId, queue);
    this.sessions.set(sessionId, raw);
    this.trackCurrentConfig(sessionId, response.configOptions ?? undefined);
    return { sessionId, configOptions: response.configOptions ?? undefined };
  }

  // deleteSession drives session/delete, removing the session's durable
  // history (see acpsvc/session.go's DeleteSession).
  public async deleteSession(sessionId: string): Promise<void> {
    await this.ensureStarted();
    if (!this.ctx || !this.acp) {
      throw new Error("Contenox ACP connection is not available");
    }
    await this.ctx.request(this.acp.methods.agent.session.delete, { sessionId });
    this.sessions.delete(sessionId);
    this.rawQueues.delete(sessionId);
    if (this.lastSessionId === sessionId) {
      this.lastSessionId = undefined;
      this.lastConfigOptions = undefined;
    }
  }

  // getLastPromptBlocksForTest -- test-only hook, see lastPromptBlocks.
  public getLastPromptBlocksForTest(): ContentBlock[] | undefined {
    return this.lastPromptBlocks;
  }

  public async sendMessage(
    sessionId: string,
    content: string,
    attachments: WireAttachment[],
    handlers: AcpSendHandlers,
  ): Promise<AcpSendResult> {
    await this.ensureStarted();
    let session = this.sessions.get(sessionId);
    if (!session) {
      // Lazily bind: sessionId came from something other than our own
      // createSession (e.g. a session selected from the pre-ACP sessions
      // list). Mint a real ACP session and key it by the id the webview
      // already has, so the panel doesn't need to know the switch happened.
      const minted = await this.newSession();
      session = minted;
      this.sessions.set(sessionId, minted);
      this.trackCurrentConfig(minted.sessionId, minted.newSessionResponse.configOptions ?? undefined);
      this.output.info(`[acp] session ${minted.sessionId} started with chain=${this.chainDescription()}`);
    }

    if (handlers.onPermissionRequested) {
      this.permissionHandlers.set(session.sessionId, (request, signal) =>
        handlers.onPermissionRequested!(mapPermissionRequest(request), signal).then(toAcpPermissionResponse),
      );
    }

    const promptBlocks = buildPromptBlocks(content, attachments);
    this.lastPromptBlocks = promptBlocks;
    const promptPromise = session.prompt(promptBlocks);
    let accumulated = "";
    let cancelled = false;
    let failed = false;
    let errorMessage: string | undefined;
    let stopReason: StopReason | undefined;

    try {
      for (;;) {
        const message = await session.nextUpdate();
        if (message.kind === "stop") {
          stopReason = message.stopReason;
          cancelled = message.stopReason === "cancelled";
          break;
        }
        const update = message.update;
        switch (update.sessionUpdate) {
          case "agent_message_chunk": {
            const text = contentText(update.content);
            if (text) {
              accumulated += text;
              handlers.onDelta?.({ content: text });
            }
            break;
          }
          case "agent_thought_chunk": {
            const text = contentText(update.content);
            if (text) {
              handlers.onDelta?.({ thinking: text });
            }
            break;
          }
          case "tool_call":
          case "tool_call_update": {
            handlers.onToolCall?.(toToolCallEvent(update));
            break;
          }
          case "usage_update": {
            handlers.onUsage?.({
              used: update.used,
              size: update.size,
              cost: update.cost ? { amount: update.cost.amount, currency: update.cost.currency } : undefined,
            });
            break;
          }
          case "available_commands_update": {
            handlers.onCommands?.(
              update.availableCommands.map((command) => ({
                name: command.name,
                description: command.description,
                hint: command.input?.hint,
              })),
            );
            break;
          }
          default:
            // Plan, mode and config updates are Phase 3+ rendering work (see
            // vscode-implementation-plan.md); ignored here.
            break;
        }
      }
      await promptPromise;
    } catch (error) {
      failed = true;
      errorMessage = error instanceof Error ? error.message : String(error);
    } finally {
      this.permissionHandlers.delete(session.sessionId);
    }

    return { cancelled, failed, error: errorMessage, content: accumulated, stopReason };
  }

  public cancelTurn(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    const targetId = session?.sessionId ?? sessionId;
    const acp = this.acp;
    if (!acp || !this.ctx) {
      return;
    }
    void this.ctx.notify(acp.methods.agent.session.cancel, { sessionId: targetId }).catch((error) => {
      this.output.warn(`[acp] session/cancel failed: ${errorMessage(error)}`);
    });
  }

  public dispose(): void {
    this.disposed = true;
    this.sessions.clear();
    this.rawQueues.clear();
    this.permissionHandlers.clear();
    this.stopConnection?.();
    this.stopConnection = undefined;
    if (this.child && !this.child.killed) {
      this.child.kill();
    }
    this.child = undefined;
    this.ctx = undefined;
    this.starting = undefined;
  }

  private trackCurrentConfig(sessionId: string, options: SessionConfigOption[] | undefined): void {
    this.lastSessionId = sessionId;
    this.lastConfigOptions = options;
  }

  private async ensureAnySessionId(): Promise<string> {
    if (this.lastSessionId && this.sessions.has(this.lastSessionId)) {
      return this.lastSessionId;
    }
    const session = await this.newSession();
    this.sessions.set(session.sessionId, session);
    this.trackCurrentConfig(session.sessionId, session.newSessionResponse.configOptions ?? undefined);
    this.output.info(`[acp] session ${session.sessionId} started with chain=${this.chainDescription()}`);
    return session.sessionId;
  }

  private chainDescription(): string {
    return this.chainPath ? this.chainPath : "runtime default (chain-acp.json)";
  }

  private async newSession(): Promise<ActiveSession> {
    if (!this.ctx) {
      throw new Error("Contenox ACP connection is not available");
    }
    const cwd = workspaceCwd() ?? process.cwd();
    return this.ctx.buildSession(cwd).start();
  }

  private async handlePermissionRequest(
    request: RequestPermissionRequest,
    signal: AbortSignal,
  ): Promise<RequestPermissionResponse> {
    const handler = this.permissionHandlers.get(request.sessionId);
    if (!handler) {
      return { outcome: { outcome: "cancelled" } };
    }
    return handler(request, signal);
  }

  // dispatchSessionUpdate feeds session/update notifications to whichever
  // RawSession (session/load) queue is waiting for this session id.
  // ActiveSession (session/new) sessions are routed by the SDK's own
  // internal router regardless of this handler (acp.js's SessionUpdateRouter
  // always reports "not handled" so normal notification dispatch continues),
  // so there is no double-delivery to worry about here.
  private dispatchSessionUpdate(notification: SessionNotification): void {
    this.rawQueues.get(notification.sessionId)?.enqueue({
      kind: "session_update",
      notification,
      update: notification.update,
    });
  }

  private async start(): Promise<void> {
    if (this.disposed) {
      throw new Error("Contenox ACP client is disposed");
    }
    const acp = await loadAcpSdk();
    this.acp = acp;

    const settings = readBridgeSettings();
    const acpSettings = readAcpSettings();
    this.chainPath = acpSettings.chainPath;
    const cwd = workspaceCwd();
    const binaryPath = resolveBinaryPath(settings.binaryPath, this.extensionUri);
    const args = bridgeCommandArgs(settings.dataDir, "acp");

    this.output.info(`Starting Contenox ACP: ${binaryPath} ${args.join(" ")} (chain=${this.chainDescription()})`);
    const env: NodeJS.ProcessEnv = { ...process.env, NO_COLOR: "1" };
    if (this.chainPath) {
      env.CONTENOX_ACP_CHAIN_PATH = this.chainPath;
    }
    const child = spawn(binaryPath, args, {
      cwd,
      env,
      stdio: "pipe",
      windowsHide: true,
    });
    this.child = child;
    child.stderr.on("data", (chunk: Buffer) => {
      const text = chunk.toString("utf8").trimEnd();
      if (text) {
        this.output.info(`[acp stderr] ${text}`);
      }
    });

    const input = Writable.toWeb(child.stdin);
    const output = Readable.toWeb(child.stdout);
    const wireStream = acp.ndJsonStream(input, output);

    let readyResolve: (() => void) | undefined;
    let readyReject: ((error: unknown) => void) | undefined;
    const ready = new Promise<void>((resolve, reject) => {
      readyResolve = resolve;
      readyReject = reject;
    });

    const spawnErrorHandler = (error: Error) => readyReject?.(error);
    child.once("error", spawnErrorHandler);
    child.once("exit", (code, signal) => {
      if (!this.disposed) {
        readyReject?.(new Error(`contenox acp exited (code ${String(code)}, signal ${String(signal)})`));
      }
    });

    const connection = acp
      .client({ name: "contenox-vscode" })
      .onRequest(acp.methods.client.session.requestPermission, async (requestCtx) =>
        this.handlePermissionRequest(requestCtx.params, requestCtx.signal),
      )
      .onNotification(acp.methods.client.session.update, (notifyCtx) => {
        this.dispatchSessionUpdate(notifyCtx.params);
      })
      .connectWith(wireStream, async (ctx) => {
        this.ctx = ctx;
        const initResult = await ctx.request(acp.methods.agent.initialize, {
          protocolVersion: acp.PROTOCOL_VERSION,
          clientCapabilities: { fs: { readTextFile: false, writeTextFile: false }, terminal: false },
          clientInfo: { name: "contenox-vscode", version: this.extensionVersion },
        });
        this.workspaceConfigOptions = extractWorkspaceConfigOptions(initResult._meta);
        child.off("error", spawnErrorHandler);
        readyResolve?.();
        await new Promise<void>((resolve) => {
          this.stopConnection = resolve;
        });
      });

    connection.catch((error) => {
      if (!this.disposed) {
        this.output.warn(`[acp] connection ended: ${errorMessage(error)}`);
      }
      readyReject?.(error);
    });

    await ready;
  }
}

// contenox.workspaceConfigOptions (acpsvc's WorkspaceConfigOptionsMetaKey) is
// a plain array of SessionConfigOption serialized directly under that key in
// initialize's `_meta`; unrecognized `_meta` keys are ignored per spec, so a
// missing/malformed value degrades to "no workspace-level options" rather
// than an error.
function extractWorkspaceConfigOptions(meta: { [key: string]: unknown } | null | undefined): SessionConfigOption[] | undefined {
  if (!meta) {
    return undefined;
  }
  const raw = meta["contenox.workspaceConfigOptions"];
  if (!Array.isArray(raw)) {
    return undefined;
  }
  return raw as SessionConfigOption[];
}

// replayEventsFromMessages folds a session/load replay's session/update
// notifications into ordered AcpReplayEvent[], coalescing chunks that share a
// messageId (see acpsvc/session.go's replayMessages, which emits at most one
// user/assistant text chunk per historical message, defensively grouped here
// in case that ever changes).
function replayEventsFromMessages(messages: ActiveSessionMessage[]): AcpReplayEvent[] {
  const events: AcpReplayEvent[] = [];
  const textIndexByKey = new Map<string, number>();
  for (const message of messages) {
    if (message.kind !== "session_update") {
      continue;
    }
    const update = message.update;
    switch (update.sessionUpdate) {
      case "user_message_chunk":
      case "agent_message_chunk": {
        const text = contentText(update.content);
        if (!text) {
          break;
        }
        const role = update.sessionUpdate === "user_message_chunk" ? "user" : "assistant";
        const key = `${role}:${update.messageId ?? `auto-${events.length}`}`;
        const existingIndex = textIndexByKey.get(key);
        const existing = existingIndex !== undefined ? events[existingIndex] : undefined;
        if (existing && existing.kind === "message") {
          existing.text += text;
        } else {
          textIndexByKey.set(key, events.length);
          events.push({ kind: "message", role, messageId: update.messageId ?? key, text });
        }
        break;
      }
      case "tool_call":
      case "tool_call_update":
        events.push({ kind: "toolCall", event: toToolCallEvent(update) });
        break;
      default:
        // Thinking, plan, usage, commands, mode and config updates are Phase
        // 2+ rendering work; ignored here (mirrors the live sendMessage loop).
        break;
    }
  }
  return events;
}

function contentText(content: ContentBlock): string | undefined {
  return content.type === "text" ? content.text : undefined;
}

function toToolCallEvent(update: ToolCall | ToolCallUpdate): AcpToolCallEvent {
  const content = update.content ?? [];
  const diff = content.find((entry) => entry.type === "diff");
  const textParts = content
    .filter((entry) => entry.type === "content")
    .map((entry) => (entry.content.type === "text" ? entry.content.text : undefined))
    .filter((text): text is string => Boolean(text && text.trim()));
  return {
    id: update.toolCallId,
    title: update.title ?? undefined,
    status: update.status ?? "in_progress",
    toolName: update.name ?? undefined,
    output: textParts.length > 0 ? textParts.join("\n\n") : undefined,
    diffPath: diff?.path,
    diffOld: diff?.oldText ?? undefined,
    diffNew: diff?.newText,
  };
}

function mapPermissionRequest(request: RequestPermissionRequest): AcpApprovalRequest {
  const meta = (request._meta ?? request.toolCall._meta ?? {}) as Record<string, unknown>;
  const content = request.toolCall.content ?? [];
  const diff = content.find((entry) => entry.type === "diff");
  const details = content
    .filter((entry) => entry.type === "content")
    .map((entry) => (entry.content.type === "text" ? entry.content.text : undefined))
    .filter((text): text is string => Boolean(text && text.trim()))
    .join("\n\n");
  return {
    approvalId: request.toolCall.toolCallId,
    toolsName: stringField(meta.toolsName),
    toolName: stringField(meta.toolName),
    title: request.toolCall.title?.trim() || request.toolCall.toolCallId,
    policyName: stringField(meta.policyName),
    policyPath: stringField(meta.policyPath),
    matchedRule: numberField(meta.matchedRule),
    matchedRuleDetail: stringField(meta.detail),
    details: details || undefined,
    diffOld: diff?.oldText ?? undefined,
    diffNew: diff?.newText,
    options: request.options.map((option) => ({ id: option.optionId, label: option.name, kind: option.kind })),
  };
}

function toAcpPermissionResponse(response: AcpApprovalResponse): RequestPermissionResponse {
  return { outcome: response.outcome };
}

// buildPromptBlocks turns Phase 3's structured attachments into ACP content
// blocks: a `resource_link` per attachment that has a URI (the "typed URI +
// name" shape from libacp/content.go:12,55). Whole-file attachments send the
// link alone -- the agent dereferences it with its own tools. Kinds with no
// dereferenceable source (selection range, diagnostics, diff) also carry a
// `text` block with the snapshot frozen at attach time. Images have no
// textual form and become a single `image` block.
// Exported (not just module-private) so it can be unit-tested directly --
// see src/test/promptBlocks.test.ts -- without spinning up a real `contenox
// acp` child process just to check the wire shape of an attachment.
export function buildPromptBlocks(content: string, attachments: WireAttachment[]): ContentBlock[] {
  const blocks: ContentBlock[] = [];
  for (const attachment of attachments) {
    if (attachment.kind === "image") {
      if (attachment.data && attachment.mimeType) {
        blocks.push({ type: "image", data: attachment.data, mimeType: attachment.mimeType, uri: attachment.uri });
      }
      continue;
    }
    if (attachment.uri) {
      blocks.push({
        type: "resource_link",
        uri: attachment.uri,
        name: attachment.name,
        description: attachment.description,
      });
    }
    // A whole file is a pointer, not payload: the agent has read_file, gointel
    // and workspace_search and fetches it on demand. Inlining it would double
    // the token cost for nothing, and the editor chain must stay affordable on
    // local models. Only kinds with no dereferenceable source — a selection
    // range, diagnostics, a diff — carry their text.
    if (attachment.text && inlinesText(attachment.kind)) {
      const label = attachment.description ?? attachment.uri ?? attachment.name;
      blocks.push({ type: "text", text: `[${attachment.kind}: ${label}]\n${attachment.text}` });
    }
  }
  blocks.push({ type: "text", text: content });
  return blocks;
}

// Whole-file kinds resolve from their URI; the rest have no source the agent
// could read to reconstruct exactly what the developer attached.
function inlinesText(kind: WireAttachment["kind"]): boolean {
  return kind !== "file" && kind !== "active_file";
}

function stringField(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function numberField(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
