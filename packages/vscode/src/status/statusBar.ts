import * as vscode from "vscode";
import { HealthResult } from "../bridge/protocol";
import { setBridgeContext } from "./contextKeys";

export class ContenoxStatusBar implements vscode.Disposable {
  private readonly item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100);

  public constructor() {
    this.item.command = "contenox.showStatus";
    this.setStopped();
    this.item.show();
  }

  public setStarting(): void {
    this.item.text = "$(sync~spin) Contenox";
    this.item.tooltip = "Contenox runtime is starting";
    void setBridgeContext(false, false);
  }

  public setReady(health: HealthResult): void {
    const configured = health.configured ? "ready" : "setup";
    this.item.text = `$(plug) Contenox ${configured}`;
    this.item.tooltip = statusTooltip(health);
    // When unconfigured, clicking the status item directly launches setup (user friendly)
    this.item.command = health.configured ? "contenox.showStatus" : "contenox.runSetup";
    void setBridgeContext(true, health.status === "ok");
  }

  public setCrashed(): void {
    this.item.text = "$(warning) Contenox";
    this.item.tooltip = "Contenox runtime stopped unexpectedly. Click to restart and view status.";
    void setBridgeContext(false, false);
  }

  public setStopped(): void {
    this.item.text = "$(circle-slash) Contenox";
    this.item.tooltip = "Contenox runtime is stopped. Click to start.";
    void setBridgeContext(false, false);
  }

  public dispose(): void {
    this.item.dispose();
  }
}

function statusTooltip(health: HealthResult): string {
  const provider = health.defaultProvider || "no provider";
  const model = health.defaultModel || "no model";
  return `Contenox runtime: ${health.status}\nProvider: ${provider}\nModel: ${model}`;
}
