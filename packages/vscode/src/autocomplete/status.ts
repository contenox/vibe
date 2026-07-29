import * as vscode from "vscode";
import { readAutocompleteSettings } from "../config/settings";

export class AutocompleteStatus implements vscode.Disposable {
  private readonly item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 90);

  public constructor() {
    this.item.command = "contenox.toggleAutocomplete";
    this.update();
    this.item.show();
  }

  public update(): void {
    const settings = readAutocompleteSettings();
    this.item.text = settings.enabled ? "$(sparkle) Contenox" : "$(circle-slash) Contenox";
    this.item.tooltip = settings.enabled ? "Contenox autocomplete is enabled" : "Contenox autocomplete is disabled";
  }

  public dispose(): void {
    this.item.dispose();
  }
}
