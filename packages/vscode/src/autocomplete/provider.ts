import * as vscode from "vscode";
import { BridgeProcess } from "../bridge/BridgeProcess";
import { readAutocompleteSettings } from "../config/settings";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

export function registerAutocomplete(bridge: BridgeProcess, output: ContenoxOutput, telemetry: TelemetryLogger): vscode.Disposable {
  telemetry.event("autocomplete.provider.registered");
  return vscode.languages.registerInlineCompletionItemProvider(
    { pattern: "**" },
    new ContenoxInlineCompletionProvider(bridge, output, telemetry),
  );
}

export async function testAutocompleteAtCursor(
  bridge: BridgeProcess,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): Promise<void> {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    telemetry.event("autocomplete.debug.skipped", { reason: "no_active_editor" });
    vscode.window.showInformationMessage("No active editor is available.");
    return;
  }
  const settings = readAutocompleteSettings();
  const document = editor.document;
  const position = editor.selection.active;
  const common = autocompleteTelemetryCommon(document, "command");
  telemetry.event("autocomplete.debug.invoked", { ...common, enabled: settings.enabled });
  output.show();

  if (!settings.enabled) {
    output.warn("Contenox autocomplete is disabled; running a direct runtime test anyway.");
  }
  if (!vscode.workspace.isTrusted) {
    telemetry.event("autocomplete.debug.skipped", { ...common, reason: "untrusted_workspace" });
    output.warn("Contenox autocomplete test skipped: workspace is not trusted.");
    return;
  }
  const documentCheck = autocompleteDocumentCheck(document, settings.maxDocumentChars);
  if (!documentCheck.ok) {
    telemetry.event("autocomplete.debug.skipped", { ...common, reason: documentCheck.reason });
    output.warn(`Contenox autocomplete test skipped: ${documentCheck.reason}.`);
    return;
  }

  const tokenSource = new vscode.CancellationTokenSource();
  try {
    const result = await requestRawCompletion(bridge, document, position, settings, tokenSource.token);
    const accepted = shouldUseCompletion(result.completion, result.prefix, result.suffix);
    telemetry.event("autocomplete.debug.result", {
      ...common,
      durationMs: result.durationMs,
      prefixChars: result.prefix.length,
      suffixChars: result.suffix.length,
      rawCompletionChars: result.rawCompletion.length,
      completionChars: result.completion.length,
      accepted,
    });
    if (!accepted) {
      output.warn(
        `Contenox autocomplete test returned ${result.completion.length} cleaned chars, but ghost text would reject it as empty or duplicate.`,
      );
      if (result.rawCompletion) {
        output.info(`Raw autocomplete result:\n${result.rawCompletion}`);
      }
      vscode.window.showWarningMessage("Contenox autocomplete returned no usable ghost text. See the Contenox output/telemetry log.");
      return;
    }

    output.info(`Contenox autocomplete test returned ${result.completion.length} chars:\n${result.completion}`);
    const choice = await vscode.window.showInformationMessage(
      `Contenox autocomplete returned ${result.completion.length} chars.`,
      "Insert",
      "Show Log",
    );
    if (choice === "Insert") {
      await editor.edit((edit) => edit.insert(position, result.completion));
    } else if (choice === "Show Log") {
      await telemetry.show();
    }
  } catch (error) {
    telemetry.error("autocomplete.debug.error", error, common);
    output.warn(`Contenox autocomplete test failed: ${errorMessage(error)}`);
    vscode.window.showErrorMessage(`Contenox autocomplete test failed: ${errorMessage(error)}`);
  } finally {
    tokenSource.dispose();
  }
}

class ContenoxInlineCompletionProvider implements vscode.InlineCompletionItemProvider {
  private lastWarningAt = 0;
  private sequence = 0;
  private activeRequest?: { sequence: number; source: vscode.CancellationTokenSource };

  public constructor(
    private readonly bridge: BridgeProcess,
    private readonly output: ContenoxOutput,
    private readonly telemetry: TelemetryLogger,
  ) {}

  public async provideInlineCompletionItems(
    document: vscode.TextDocument,
    position: vscode.Position,
    context: vscode.InlineCompletionContext,
    token: vscode.CancellationToken,
  ): Promise<vscode.InlineCompletionItem[] | undefined> {
    const settings = readAutocompleteSettings();
    const common = autocompleteTelemetryCommon(document, context.triggerKind);
    this.telemetry.event("autocomplete.provider.invoked", common);
    if (!settings.enabled) {
      this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "disabled" });
      return undefined;
    }
    if (token.isCancellationRequested) {
      this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "cancelled_before_start" });
      return undefined;
    }
    if (!vscode.workspace.isTrusted) {
      this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "untrusted_workspace" });
      return undefined;
    }
    const documentCheck = autocompleteDocumentCheck(document, settings.maxDocumentChars);
    if (!documentCheck.ok) {
      this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: documentCheck.reason });
      return undefined;
    }
    const requestSeq = ++this.sequence;
    const documentVersion = document.version;
    if (context.triggerKind === vscode.InlineCompletionTriggerKind.Automatic && settings.debounceMs > 0) {
      await delay(settings.debounceMs);
      if (token.isCancellationRequested || requestSeq !== this.sequence || document.version !== documentVersion) {
        return undefined;
      }
    }

    try {
      const state = await this.bridge.ensureStarted();
      if (!state.initialize.capabilities.autocomplete) {
        this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "runtime_capability_missing" });
        return undefined;
      }
      const client = this.bridge.currentClient;
      if (!client) {
        this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "runtime_connection_missing" });
        return undefined;
      }

      this.cancelActiveRequest();
      const requestTokenSource = new vscode.CancellationTokenSource();
      this.activeRequest = { sequence: requestSeq, source: requestTokenSource };
      let result: RawCompletionResult;
      try {
        result = await requestRawCompletion(this.bridge, document, position, settings, requestTokenSource.token);
      } finally {
        if (this.activeRequest?.sequence === requestSeq) {
          this.activeRequest.source.dispose();
          this.activeRequest = undefined;
        }
      }
      if (result.prefix.length === 0 && result.suffix.length === 0) {
        this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "empty_window" });
        return undefined;
      }

      this.telemetry.event("autocomplete.provider.result", {
        ...common,
        durationMs: result.durationMs,
        prefixChars: result.prefix.length,
        suffixChars: result.suffix.length,
        completionChars: result.completion.length,
      });
      if (token.isCancellationRequested) {
        this.telemetry.event("autocomplete.provider.vscode_cancelled_after_start", common);
      }
      if (requestSeq !== this.sequence || document.version !== documentVersion) {
        this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "stale_after_result" });
        return undefined;
      }
      const completion = result.completion;
      if (!shouldUseCompletion(completion, result.prefix, result.suffix)) {
        this.telemetry.event("autocomplete.provider.skipped", {
          ...common,
          reason: "completion_rejected",
          completionChars: completion.length,
        });
        return undefined;
      }

      const item = new vscode.InlineCompletionItem(completion, new vscode.Range(position, position));
      item.command = { command: "contenox.acceptAutocomplete", title: "Contenox Autocomplete Accepted" };
      this.telemetry.event("autocomplete.provider.item", { ...common, completionChars: completion.length });
      return [item];
    } catch (error) {
      if (isBridgeAutocompleteCancellation(error)) {
        this.telemetry.event("autocomplete.provider.skipped", { ...common, reason: "request_cancelled" });
        return undefined;
      }
      this.telemetry.error("autocomplete.provider.error", error, common);
      this.warnThrottled(error);
      return undefined;
    }
  }

  private cancelActiveRequest(): void {
    if (!this.activeRequest) {
      return;
    }
    this.activeRequest.source.cancel();
    this.activeRequest.source.dispose();
    this.activeRequest = undefined;
  }

  private warnThrottled(error: unknown): void {
    const now = Date.now();
    if (now - this.lastWarningAt < 30000) {
      return;
    }
    this.lastWarningAt = now;
    this.output.warn(`Contenox autocomplete failed: ${errorMessage(error)}`);
  }
}

interface RawCompletionResult {
  prefix: string;
  suffix: string;
  rawCompletion: string;
  completion: string;
  durationMs: number;
}

async function requestRawCompletion(
  bridge: BridgeProcess,
  document: vscode.TextDocument,
  position: vscode.Position,
  settings: ReturnType<typeof readAutocompleteSettings>,
  token: vscode.CancellationToken,
): Promise<RawCompletionResult> {
  const state = await bridge.ensureStarted();
  if (!state.initialize.capabilities.autocomplete) {
    throw new Error("This Contenox runtime does not support autocomplete");
  }
  const client = bridge.currentClient;
  if (!client) {
    throw new Error("Contenox runtime connection is not available");
  }
  const { prefix, suffix } = documentWindow(document, position, settings.maxPrefixChars, settings.maxSuffixChars);
  if (prefix.length === 0 && suffix.length === 0) {
    return {
      prefix,
      suffix,
      rawCompletion: "",
      completion: "",
      durationMs: 0,
    };
  }
  const startedAt = Date.now();
  const result = await client.autocomplete({
      prefix,
      suffix,
      languageId: document.languageId,
      uri: document.uri.toString(),
      provider: settings.provider,
      model: settings.model,
      maxTokens: settings.maxTokens,
    }, token);
  return {
    prefix,
    suffix,
    rawCompletion: result.completion,
    completion: cleanCompletion(result.completion),
    durationMs: Date.now() - startedAt,
  };
}

function autocompleteTelemetryCommon(document: vscode.TextDocument, triggerKind: string | number): Record<string, unknown> {
  return {
    uriScheme: document.uri.scheme,
    languageId: document.languageId,
    triggerKind,
    documentChars: document.getText().length,
    lineCount: document.lineCount,
  };
}

function autocompleteDocumentCheck(document: vscode.TextDocument, maxDocumentChars: number): { ok: true } | { ok: false; reason: string } {
  if (document.isClosed || document.lineCount === 0) {
    return { ok: false, reason: "closed_or_empty" };
  }
  if (document.uri.scheme === "output" || document.uri.scheme === "search-editor") {
    return { ok: false, reason: "unsupported_scheme" };
  }
  const text = document.getText();
  if (text.length > maxDocumentChars || text.includes("\0")) {
    return { ok: false, reason: text.includes("\0") ? "binary_document" : "document_too_large" };
  }
  const longestLine = text.split(/\r?\n/, 250).reduce((max, line) => Math.max(max, line.length), 0);
  if (longestLine > 2000) {
    return { ok: false, reason: "line_too_long" };
  }
  return { ok: true };
}

function documentWindow(
  document: vscode.TextDocument,
  position: vscode.Position,
  maxPrefixChars: number,
  maxSuffixChars: number,
): { prefix: string; suffix: string } {
  const text = document.getText();
  const offset = document.offsetAt(position);
  const prefixStart = Math.max(0, offset - maxPrefixChars);
  const suffixEnd = Math.min(text.length, offset + maxSuffixChars);
  return {
    prefix: text.slice(prefixStart, offset),
    suffix: text.slice(offset, suffixEnd),
  };
}

function cleanCompletion(value: string): string {
  return value
    .replace(/<\/?fim_(prefix|suffix|middle)>/gi, "")
    .replace(/```[a-zA-Z0-9_-]*\n?/g, "")
    .trimEnd();
}

function shouldUseCompletion(completion: string, prefix: string, suffix: string): boolean {
  const trimmed = completion.trim();
  if (!trimmed) {
    return false;
  }
  if (suffix.startsWith(completion)) {
    return false;
  }
  const prefixTail = prefix.slice(-completion.length);
  if (prefixTail === completion) {
    return false;
  }
  return true;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function isBridgeAutocompleteCancellation(error: unknown): boolean {
  return errorMessage(error).includes("Contenox runtime request cancelled: autocomplete");
}
