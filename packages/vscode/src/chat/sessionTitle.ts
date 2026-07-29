const maxSessionTitleLength = 64;

const genericSessionTitlePattern = /^(default|new contenox session|new session|new chat|contenox chat|vscode-chat|vscode-lm|vscode-agent-session|untitled|untitled chat|session-[0-9a-f-]+)$/i;

export function sessionTitleFromChatInput(command: string | undefined, prompt: string): string {
  const cleanPrompt = normalizeTitle(prompt);
  switch ((command ?? "").trim()) {
    case "fix":
      return titleWithFallback(cleanPrompt ? `Fix: ${cleanPrompt}` : "Fix diagnostics");
    case "explain":
      return titleWithFallback(cleanPrompt ? `Explain: ${cleanPrompt}` : "Explain selected code");
    case "review":
      return titleWithFallback(cleanPrompt ? `Review changes: ${cleanPrompt}` : "Review current changes");
    case "commit":
      return titleWithFallback(cleanPrompt ? `Draft commit: ${cleanPrompt}` : "Draft commit message");
    case "doctor":
      return "Check Contenox setup";
    case "compact":
      return titleWithFallback(cleanPrompt ? `Compact history: ${cleanPrompt}` : "Compact session history");
    case "policy":
      return titleWithFallback(cleanPrompt ? `HITL policy: ${cleanPrompt}` : "Show HITL policy");
    case "websearch":
      return titleWithFallback(cleanPrompt ? `Web search: ${cleanPrompt}` : "Web search");
    default:
      return sessionTitleFromInput(prompt);
  }
}

export function sessionTitleFromInput(input: string): string {
  let title = normalizeTitle(input);
  title = title.replace(/^@contenox\s+/i, "").trim();
  if (title.startsWith("/")) {
    const match = /^\/([a-z][\w-]*)(?:\s+(.*))?$/i.exec(title);
    if (match) {
      return titleFromSlashCommand(match[1], match[2] ?? "");
    }
  }
  return titleWithFallback(title);
}

function titleFromSlashCommand(command: string, args: string): string {
  const cleanArgs = normalizeTitle(args);
  switch (command.toLowerCase()) {
    case "doctor":
      return "Check Contenox setup";
    case "help":
      return "Contenox help";
    case "compact":
      return titleWithFallback(cleanArgs ? `Compact history: ${cleanArgs}` : "Compact session history");
    case "model":
      return titleWithFallback(cleanArgs ? `Model: ${cleanArgs}` : "Show model");
    case "provider":
      return titleWithFallback(cleanArgs ? `Provider: ${cleanArgs}` : "Show provider");
    case "autocomplete-model":
      return titleWithFallback(cleanArgs ? `Autocomplete model: ${cleanArgs}` : "Show autocomplete model");
    case "autocomplete-provider":
      return titleWithFallback(cleanArgs ? `Autocomplete provider: ${cleanArgs}` : "Show autocomplete provider");
    case "max-tokens":
      return titleWithFallback(cleanArgs ? `Max tokens: ${cleanArgs}` : "Show max tokens");
    case "think":
      return titleWithFallback(cleanArgs ? `Thinking: ${cleanArgs}` : "Show thinking level");
    case "capability":
      return titleWithFallback(cleanArgs ? `Capability: ${cleanArgs}` : "Show model capability");
    case "policy":
      return titleWithFallback(cleanArgs ? `HITL policy: ${cleanArgs}` : "Show HITL policy");
    case "websearch":
      return titleWithFallback(cleanArgs ? `Web search: ${cleanArgs}` : "Web search");
    default:
      return titleWithFallback(cleanArgs || command);
  }
}

function titleWithFallback(value: string): string {
  const title = truncateTitle(normalizeTitle(value));
  if (!title || genericSessionTitlePattern.test(title)) {
    return "Contenox chat";
  }
  return title;
}

function normalizeTitle(value: string): string {
  return value
    .replace(/```[\s\S]*?```/g, " code block ")
    .replace(/`([^`]+)`/g, "$1")
    .replace(/\s+/g, " ")
    .replace(/^["'\s]+|["'\s]+$/g, "")
    .trim();
}

function truncateTitle(value: string): string {
  if (value.length <= maxSessionTitleLength) {
    return value;
  }
  return `${value.slice(0, maxSessionTitleLength - 3).trimEnd()}...`;
}
