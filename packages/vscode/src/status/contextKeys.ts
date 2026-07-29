import * as vscode from "vscode";

export async function setBridgeContext(connected: boolean, healthy: boolean): Promise<void> {
  await vscode.commands.executeCommand("setContext", "contenox.connected", connected);
  await vscode.commands.executeCommand("setContext", "contenox.bridgeHealthy", healthy);
}

export async function setTurnContext(inProgress: boolean): Promise<void> {
  await vscode.commands.executeCommand("setContext", "contenox.turnInProgress", inProgress);
}

export async function setDiagnosticsContext(hasDiagnostics: boolean): Promise<void> {
  await vscode.commands.executeCommand("setContext", "contenox.hasDiagnostics", hasDiagnostics);
}
