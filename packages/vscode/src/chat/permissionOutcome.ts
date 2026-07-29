import {
  ApprovalRequestedEvent,
  PermissionOption,
  RequestPermissionParams,
  ToolCallContent,
} from "../bridge/protocol";

export function approvalEventFromPermissionRequest(
  event: RequestPermissionParams,
): ApprovalRequestedEvent {
  const toolCall = event.toolCall;
  const meta = { ...(event._meta ?? {}), ...(toolCall._meta ?? {}) };
  const diff = firstDiffContent(toolCall.content);
  const args = objectRecord(toolCall.rawInput);
  const rawDiffOld = stringValue(meta.diffOld) ?? stringValue(diff?.oldText);
  const rawDiffNew = stringValue(meta.diffNew) ?? stringValue(diff?.newText);
  const hasContentDiff = isNonBlank(rawDiffOld) || isNonBlank(rawDiffNew);
  return {
    approvalId: toolCall.toolCallId,
    toolsName: stringValue(meta.toolsName),
    toolName: stringValue(meta.toolName),
    title: nonBlankString(toolCall.title) ?? toolCall.toolCallId,
    policyName: stringValue(meta.policyName),
    policyPath: stringValue(meta.policyPath),
    args,
    details: contentDetails(toolCall.content),
    diff: nonBlankString(meta.diff),
    diffOld: hasContentDiff ? (rawDiffOld ?? "") : undefined,
    diffNew: hasContentDiff ? (rawDiffNew ?? "") : undefined,
    options: event.options.map((option) => ({
      id: option.optionId,
      label: option.name,
      kind: option.kind,
    })),
  };
}

export function selectedPermissionOption(
  event: RequestPermissionParams,
  optionId: string | undefined,
): PermissionOption | undefined {
  if (optionId) {
    const exact = event.options.find((candidate) => candidate.optionId === optionId);
    if (exact) {
      return exact;
    }
  }
  return event.options.find(
    (candidate) => candidate.optionId === "deny" || candidate.kind.startsWith("reject"),
  );
}

function firstDiffContent(
  content: readonly ToolCallContent[] | undefined,
): ToolCallContent | undefined {
  return content?.find(
    (entry) => entry.type === "diff" && (isNonBlank(entry.oldText) || isNonBlank(entry.newText)),
  );
}

function contentDetails(content: readonly ToolCallContent[] | undefined): string | undefined {
  const parts =
    content
      ?.filter((entry) => entry.type !== "diff")
      .map((entry) => {
        const text =
          stringValue(entry.content?.text) ?? stringValue((entry as { text?: unknown }).text);
        if (!isNonBlank(text)) {
          return undefined;
        }
        return entry.path ? `${entry.path}\n${text}` : text;
      })
      .filter(isNonBlank) ?? [];
  return parts.length > 0 ? parts.join("\n\n") : undefined;
}

function objectRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value === "string") {
    try {
      const parsed = JSON.parse(value) as unknown;
      return objectRecord(parsed);
    } catch {
      return undefined;
    }
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return undefined;
  }
  return value as Record<string, unknown>;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function nonBlankString(value: unknown): string | undefined {
  const str = stringValue(value);
  return isNonBlank(str) ? str : undefined;
}

function isNonBlank(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}
