import * as vscode from "vscode";
import { ApprovalRequestedEvent } from "../bridge/protocol";
import { TelemetryLogger } from "../logging/telemetry";

export const approvalToolName = "approve_contenox_tool_call";

interface ApprovalToolInput {
  approvalId: string;
  title: string;
  toolName?: string;
  toolsName?: string;
  policyName?: string;
  policyPath?: string;
  summary: string;
  input?: Record<string, unknown>;
  inputJson?: string;
  details?: string;
  diff?: string;
  currentContent?: string;
  proposedContent?: string;
}

const invokedApprovalIds = new Set<string>();

export function registerApprovalTool(telemetry: TelemetryLogger): vscode.Disposable {
  return vscode.lm.registerTool(approvalToolName, new ContenoxApprovalTool(telemetry));
}

export async function requestNativeApproval(
  event: ApprovalRequestedEvent,
  toolInvocationToken: vscode.ChatParticipantToolToken | undefined,
  token: vscode.CancellationToken,
  telemetry: TelemetryLogger,
): Promise<boolean> {
  if (token.isCancellationRequested || !toolInvocationToken) {
    telemetry.warn("approval.editor.denied", {
      approvalId: event.approvalId,
      toolName: event.toolName,
      toolsName: event.toolsName,
      reason: token.isCancellationRequested ? "cancelled_before_prompt" : "missing_tool_invocation_token",
    });
    return false;
  }

  telemetry.event("approval.editor.requested", {
    approvalId: event.approvalId,
    toolName: event.toolName,
    toolsName: event.toolsName,
    title: event.title,
  });

  const input = approvalInputFromEvent(event);
  telemetry.event("approval.native.payload", {
    approvalId: event.approvalId,
    toolName: event.toolName,
    toolsName: event.toolsName,
    argKeyCount: input.input ? Object.keys(input.input).length : 0,
    detailChars: input.details?.length ?? 0,
    diffChars: input.diff?.length ?? 0,
    currentChars: input.currentContent?.length ?? 0,
    proposedChars: input.proposedContent?.length ?? 0,
  });
  if (!input.inputJson && !input.details && !input.diff && input.currentContent === undefined && input.proposedContent === undefined) {
    telemetry.warn("approval.native.payload.missing_details", {
      approvalId: event.approvalId,
      toolName: event.toolName,
      toolsName: event.toolsName,
      title: event.title,
    });
  }

  invokedApprovalIds.delete(event.approvalId);
  try {
    await vscode.lm.invokeTool(
      approvalToolName,
      {
        toolInvocationToken,
        input,
      },
      token,
    );
  } catch (error) {
    telemetry.warn("approval.editor.denied", {
      approvalId: event.approvalId,
      toolName: event.toolName,
      toolsName: event.toolsName,
      reason: "native_tool_rejected",
      error: errorMessage(error),
    });
    return false;
  }

  const approved = !token.isCancellationRequested && invokedApprovalIds.delete(event.approvalId);
  if (!approved) {
    telemetry.warn("approval.editor.denied", {
      approvalId: event.approvalId,
      toolName: event.toolName,
      toolsName: event.toolsName,
      reason: token.isCancellationRequested ? "cancelled_after_prompt" : "native_tool_not_invoked",
    });
    return false;
  }

  telemetry.event("approval.editor.approved", {
    approvalId: event.approvalId,
    toolName: event.toolName,
    toolsName: event.toolsName,
  });
  return true;
}

class ContenoxApprovalTool implements vscode.LanguageModelTool<ApprovalToolInput> {
  public constructor(private readonly telemetry: TelemetryLogger) {}

  public prepareInvocation(
    options: vscode.LanguageModelToolInvocationPrepareOptions<ApprovalToolInput>,
    token: vscode.CancellationToken,
  ): vscode.ProviderResult<vscode.PreparedToolInvocation> {
    const input = options.input;
    this.telemetry.event("approval.native.prepare", {
      approvalId: input.approvalId,
      toolName: input.toolName,
      toolsName: input.toolsName,
      title: input.title,
    });
    if (token.isCancellationRequested) {
      return undefined;
    }
    return {
      invocationMessage: `Waiting for Contenox approval: ${input.title}`,
      confirmationMessages: {
        title: `Run ${input.title}?`,
        message: approvalMarkdown(input),
      },
    };
  }

  public invoke(
    options: vscode.LanguageModelToolInvocationOptions<ApprovalToolInput>,
    token: vscode.CancellationToken,
  ): vscode.ProviderResult<vscode.LanguageModelToolResult> {
    const input = options.input;
    if (token.isCancellationRequested) {
      return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(`Cancelled ${input.title}.`)]);
    }
    invokedApprovalIds.add(input.approvalId);
    this.telemetry.event("approval.native.invoke", {
      approvalId: input.approvalId,
      toolName: input.toolName,
      toolsName: input.toolsName,
      title: input.title,
    });
    return new vscode.LanguageModelToolResult([new vscode.LanguageModelTextPart(`Approved ${input.title}.`)]);
  }
}

function approvalInputFromEvent(event: ApprovalRequestedEvent): ApprovalToolInput {
  const inputJson = event.args && Object.keys(event.args).length > 0 ? JSON.stringify(event.args, null, 2) : undefined;
  const diff = nonBlankString(event.diff);
  const hasContentPair = isNonBlank(event.diffOld) || isNonBlank(event.diffNew);
  return {
    approvalId: event.approvalId,
    title: event.title,
    toolName: event.toolName,
    toolsName: event.toolsName,
    policyName: event.policyName,
    policyPath: event.policyPath,
    summary: approvalSummary(event),
    input: event.args,
    inputJson,
    details: nonBlankString(event.details),
    diff,
    currentContent: hasContentPair ? event.diffOld ?? "" : undefined,
    proposedContent: hasContentPair ? event.diffNew ?? "" : undefined,
  };
}

function approvalSummary(event: ApprovalRequestedEvent): string {
  const parts = [`Tool: ${event.title}`];
  if (event.policyName || event.policyPath) {
    parts.push(`Policy: ${event.policyName || "active HITL policy"}`);
    if (event.policyPath) {
      parts.push(`Policy file: ${event.policyPath}`);
    }
  }
  return parts.join("\n");
}

function approvalMarkdown(input: ApprovalToolInput): vscode.MarkdownString {
  const markdown = new vscode.MarkdownString(undefined, true);
  markdown.supportHtml = false;
  markdown.supportThemeIcons = true;

  const sections = [
    "Contenox HITL policy requires approval before this action can run.",
    "",
    `**Tool:** \`${escapeInline(input.title)}\``,
  ];
  if (input.policyName || input.policyPath) {
    sections.push("", `**Policy:** \`${escapeInline(input.policyName || "active HITL policy")}\``);
    if (input.policyPath) {
      sections.push(`**Policy file:** \`${escapeInline(input.policyPath)}\``);
    }
  }
  if (input.inputJson) {
    sections.push("", "**Input:**", codeBlock(input.inputJson, "json", 6000));
  }
  if (input.details) {
    sections.push("", "**Details:**", codeBlock(input.details, "", 6000));
  }
  const diff = nonBlankString(input.diff);
  if (diff) {
    sections.push("", "**Proposed change:**", codeBlock(diff, "diff", 12000));
  } else if (input.currentContent !== undefined || input.proposedContent !== undefined) {
    sections.push("", "**Current content:**", codeBlock(input.currentContent ?? "", "", 6000));
    sections.push("", "**Proposed content:**", codeBlock(input.proposedContent ?? "", "", 6000));
  } else if (!input.inputJson && !input.details) {
    sections.push("", "**Details:** No structured tool input or diff was provided by the runtime.");
  }
  markdown.value = sections.join("\n");
  return markdown;
}

function nonBlankString(value: string | undefined): string | undefined {
  return isNonBlank(value) ? value : undefined;
}

function isNonBlank(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

function codeBlock(value: string, language: string, maxChars: number): string {
  return `\`\`\`${language}\n${truncate(value, maxChars).replace(/```/g, "``\\`")}\n\`\`\``;
}

function escapeInline(value: string): string {
  return value.replace(/[`\\]/g, "\\$&");
}

function truncate(value: string, maxChars: number): string {
  if (value.length <= maxChars) {
    return value;
  }
  return `${value.slice(0, maxChars)}\n... truncated ...`;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
