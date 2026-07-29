import * as vscode from "vscode";
import { SessionMessage, SessionResult } from "../bridge/protocol";
import { TelemetryLogger } from "../logging/telemetry";

const scheme = "contenox-session";

export class SessionDocumentProvider implements vscode.TextDocumentContentProvider, vscode.Disposable {
  private readonly changeEmitter = new vscode.EventEmitter<vscode.Uri>();
  private readonly contents = new Map<string, string>();

  public readonly onDidChange = this.changeEmitter.event;

  public constructor(private readonly telemetry: TelemetryLogger) {}

  public provideTextDocumentContent(uri: vscode.Uri): string {
    return this.contents.get(uri.toString()) ?? "Session transcript is not loaded.";
  }

  public async open(result: SessionResult): Promise<void> {
    const title = result.session.name || result.session.id;
    const uri = vscode.Uri.from({
      scheme,
      path: `/${safeFileName(title)}-${encodeURIComponent(result.session.id)}.md`,
    });
    this.contents.set(uri.toString(), renderSession(result));
    this.changeEmitter.fire(uri);
    this.telemetry.event("session.open_transcript", {
      sessionId: result.session.id,
      messageCount: result.messages.length,
    });

    await vscode.workspace.openTextDocument(uri);
    await vscode.commands.executeCommand("markdown.showPreview", uri);
  }

  public dispose(): void {
    this.changeEmitter.dispose();
    this.contents.clear();
  }
}

export function sessionDocumentScheme(): string {
  return scheme;
}

function renderSession(result: SessionResult): string {
  const lines = [
    `# ${escapeMarkdown(result.session.name || "Contenox Session")}`,
    "",
    `- ID: \`${escapeInlineCode(result.session.id)}\``,
    `- Messages: ${result.messages.length}`,
    "",
    "> This transcript is read-only. New `@contenox` messages continue in this session after opening it from the sidebar.",
    "",
  ];

  for (const message of result.messages) {
    lines.push(...renderMessage(message));
  }
  return `${lines.join("\n")}\n`;
}

function renderMessage(message: SessionMessage): string[] {
  const role = roleTitle(message.role);
  const lines = [`## ${role}`, ""];
  if (message.timestamp) {
    lines.push(`_${escapeMarkdown(message.timestamp)}_`, "");
  }
  if (message.content?.trim()) {
    lines.push(message.content.trim(), "");
  }
  if (message.thinking?.trim()) {
    lines.push("**Thinking**", "", codeBlock(message.thinking.trim(), ""), "");
  }
  if (message.toolCalls?.length) {
    lines.push("**Tool calls**", "");
    for (const call of message.toolCalls) {
      lines.push(`- \`${escapeInlineCode(call.name || call.id || "tool")}\``);
      if (call.arguments && Object.keys(call.arguments).length > 0) {
        lines.push("", codeBlock(JSON.stringify(call.arguments, null, 2), "json"), "");
      } else if (call.rawArgs?.trim()) {
        lines.push("", codeBlock(call.rawArgs.trim(), "json"), "");
      }
    }
    lines.push("");
  }
  if (message.toolCallId) {
    lines.push(`_Tool result for \`${escapeInlineCode(message.toolCallId)}\`_`, "");
  }
  return lines;
}

function roleTitle(role: string): string {
  switch (role) {
    case "user":
      return "User";
    case "assistant":
      return "Assistant";
    case "system":
      return "System";
    case "tool":
      return "Tool";
    default:
      return role ? role[0].toUpperCase() + role.slice(1) : "Message";
  }
}

function codeBlock(value: string, language: string): string {
  return `\`\`\`${language}\n${value.replace(/```/g, "``\\`")}\n\`\`\``;
}

function safeFileName(value: string): string {
  const safe = value.trim().replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  return safe || "session";
}

function escapeMarkdown(value: string): string {
  return value.replace(/([\\`*_{}[\]()#+.!|-])/g, "\\$1");
}

function escapeInlineCode(value: string): string {
  return value.replace(/[`\\]/g, "\\$&");
}
