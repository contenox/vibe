import * as vscode from "vscode";
import { TelemetryLogger } from "../logging/telemetry";

export function registerDiagnosticCodeActions(telemetry: TelemetryLogger): vscode.Disposable {
  return vscode.languages.registerCodeActionsProvider(
    { pattern: "**" },
    new ContenoxCodeActionProvider(telemetry),
    {
      providedCodeActionKinds: [
        vscode.CodeActionKind.QuickFix,
        vscode.CodeActionKind.Refactor,
        vscode.CodeActionKind.RefactorRewrite,
      ],
    },
  );
}

class ContenoxCodeActionProvider implements vscode.CodeActionProvider {
  public constructor(private readonly telemetry: TelemetryLogger) {}

  public provideCodeActions(
    document: vscode.TextDocument,
    range: vscode.Range | vscode.Selection,
    context: vscode.CodeActionContext,
    _token: vscode.CancellationToken,
  ): vscode.CodeAction[] {
    const diagnostics = relevantDiagnostics(context.diagnostics, range);
    this.telemetry.event("code_action.provide", {
      uriScheme: document.uri.scheme,
      languageId: document.languageId,
      requestedDiagnostics: context.diagnostics.length,
      relevantDiagnostics: diagnostics.length,
      only: context.only?.value,
      rangeEmpty: range.isEmpty,
      triggerKind: vscode.CodeActionTriggerKind[context.triggerKind],
    });

    const actions: vscode.CodeAction[] = [];
    if (diagnostics.length > 0) {
      actions.push(...diagnosticCodeActions(diagnostics));
    }
    if (shouldOfferSelectionActions(range)) {
      actions.push(...selectionCodeActions());
    }

    return actions.filter((action) => matchesRequestedKind(action, context.only));
  }
}

function diagnosticCodeActions(diagnostics: readonly vscode.Diagnostic[]): vscode.CodeAction[] {
  const diagnosticList = [...diagnostics];
  const fix = new vscode.CodeAction("Fix with Contenox", vscode.CodeActionKind.QuickFix);
  fix.diagnostics = diagnosticList;
  fix.isPreferred = false;
  fix.command = {
    command: "contenox.fixDiagnostics",
    title: "Fix with Contenox",
    arguments: [diagnosticList],
  };

  const explain = new vscode.CodeAction("Explain diagnostic with Contenox", vscode.CodeActionKind.QuickFix);
  explain.diagnostics = diagnosticList;
  explain.command = {
    command: "contenox.explainDiagnostics",
    title: "Explain diagnostic with Contenox",
    arguments: [diagnosticList],
  };

  return [fix, explain];
}

function selectionCodeActions(): vscode.CodeAction[] {
  const fix = new vscode.CodeAction("Fix selection with Contenox", vscode.CodeActionKind.QuickFix);
  fix.command = {
    command: "contenox.fixSelection",
    title: "Fix selection with Contenox",
  };

  const explain = new vscode.CodeAction("Explain selection with Contenox", vscode.CodeActionKind.Refactor);
  explain.command = {
    command: "contenox.askSelection",
    title: "Explain selection with Contenox",
  };

  const addToChat = new vscode.CodeAction("Add selection to Contenox Chat", vscode.CodeActionKind.Refactor);
  addToChat.command = {
    command: "contenox.addSelectionToChat",
    title: "Add selection to Contenox Chat",
  };

  return [fix, explain, addToChat];
}

function shouldOfferSelectionActions(range: vscode.Range): boolean {
  return !range.isEmpty;
}

function matchesRequestedKind(action: vscode.CodeAction, only: vscode.CodeActionKind | undefined): boolean {
  return !only || !action.kind || only.contains(action.kind);
}

function relevantDiagnostics(diagnostics: readonly vscode.Diagnostic[], range: vscode.Range): vscode.Diagnostic[] {
  return diagnostics.filter((diagnostic) => diagnostic.range.intersection(range) || diagnostic.range.contains(range.start));
}
