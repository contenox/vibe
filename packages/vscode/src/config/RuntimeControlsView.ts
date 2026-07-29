import * as vscode from "vscode";
import { AcpChatClient } from "../acp/AcpChatClient";
import { SessionConfigOption, SessionConfigSelectGroup, SessionConfigSelectOption } from "../acp/types";
import { TelemetryLogger } from "../logging/telemetry";

type RuntimeControlMessage =
  | { type: "refresh" }
  | { type: "setModel"; value: string }
  | { type: "setThink"; value: string }
  | { type: "setHitl"; value: string };

interface RuntimeSelectValue {
  value: string;
  name: string;
  description?: string;
}

interface RuntimeSelectGroup {
  group: string;
  name: string;
  values: RuntimeSelectValue[];
}

// A rendered "select" control: either grouped (the model option, grouped by
// provider -- see acpsvc/config_options.go's modelConfigValues) or flat
// (think, hitl-policy).
interface RuntimeSelectState {
  currentValue: string;
  groups: RuntimeSelectGroup[];
  values: RuntimeSelectValue[];
}

interface RuntimeControlsState {
  configured: boolean;
  model?: RuntimeSelectState;
  think?: RuntimeSelectState;
  hitl?: RuntimeSelectState;
}

const configIdModel = "model";
const configIdThink = "think";
const configIdHitl = "hitl-policy";

// Runtime controls over ACP (vscode-implementation-plan.md Phase 1 §"runtime
// controls over ACP"): pickers come from initialize's workspaceConfigOptions
// (session-less) or the current session's config options (once one exists),
// and edits drive session/set_config_option -- replacing the bespoke bridge's
// getConfig/setConfig/listProviders/listModels, which is what produced the
// "vertex-google (not configured)" bug (vscodeagent's own listProviders/
// listModels reimplementation, never routed through acpsvc.Transport).
export class RuntimeControlsViewProvider implements vscode.WebviewViewProvider, vscode.Disposable {
  private view: vscode.WebviewView | undefined;

  public constructor(
    private readonly acpClient: AcpChatClient,
    private readonly telemetry: TelemetryLogger,
    private readonly onChanged: () => void,
  ) {}

  public resolveWebviewView(view: vscode.WebviewView): void {
    this.view = view;
    view.webview.options = { enableScripts: true };
    view.webview.html = this.renderShell(view.webview);
    view.webview.onDidReceiveMessage((message: RuntimeControlMessage) => {
      void this.handleMessage(message);
    });
    void this.refresh();
  }

  public async refresh(): Promise<void> {
    const view = this.view;
    if (!view) {
      return;
    }
    try {
      const state = await this.loadState();
      await view.webview.postMessage({ type: "state", state });
      this.telemetry.event("runtime_controls.refreshed", {
        configured: state.configured,
        hasModel: Boolean(state.model),
        hasThink: Boolean(state.think),
        hasHitl: Boolean(state.hitl),
      });
    } catch (error) {
      await view.webview.postMessage({ type: "error", message: errorMessage(error) });
      this.telemetry.warn("runtime_controls.refresh.failed", { error: errorMessage(error) });
    }
  }

  public dispose(): void {
    this.view = undefined;
  }

  private async handleMessage(message: RuntimeControlMessage): Promise<void> {
    if (message.type === "refresh") {
      await this.refresh();
      return;
    }

    try {
      switch (message.type) {
        case "setModel":
          await this.acpClient.setSessionConfigOption(configIdModel, message.value);
          break;
        case "setThink":
          await this.acpClient.setSessionConfigOption(configIdThink, message.value);
          break;
        case "setHitl":
          await this.acpClient.setSessionConfigOption(configIdHitl, message.value);
          break;
      }
      this.onChanged();
      await this.refresh();
    } catch (error) {
      this.telemetry.warn("runtime_controls.set.failed", { type: message.type, error: errorMessage(error) });
      await this.view?.webview.postMessage({ type: "error", message: errorMessage(error) });
    }
  }

  private async loadState(): Promise<RuntimeControlsState> {
    await this.acpClient.ensureStarted();
    const options = this.acpClient.getCurrentConfigOptions() ?? this.acpClient.getWorkspaceConfigOptions() ?? [];
    return {
      configured: options.length > 0,
      model: toSelectState(options, configIdModel),
      think: toSelectState(options, configIdThink),
      hitl: toSelectState(options, configIdHitl),
    };
  }

  private renderShell(webview: vscode.Webview): string {
    const nonce = `${Date.now()}-${Math.random().toString(16).slice(2)}`;
    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src ${webview.cspSource} 'unsafe-inline'; script-src 'nonce-${nonce}';">
  <style>
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 12px;
      color: var(--vscode-foreground);
      font: var(--vscode-font-weight) var(--vscode-font-size) var(--vscode-font-family);
      background: var(--vscode-sideBar-background, transparent);
    }
    h1 {
      margin: 0 0 12px;
      font-size: 11px;
      font-weight: 600;
      letter-spacing: 0.04em;
      text-transform: uppercase;
      color: var(--vscode-descriptionForeground);
    }
    fieldset {
      margin: 0;
      padding: 0;
      border: 0;
    }
    .row {
      display: grid;
      gap: 4px;
      margin-bottom: 12px;
    }
    label {
      color: var(--vscode-descriptionForeground);
      font-size: 11px;
      font-weight: 500;
      letter-spacing: 0.03em;
      text-transform: uppercase;
    }
    select, button {
      width: 100%;
      min-height: 28px;
      font: inherit;
      color: var(--vscode-dropdown-foreground);
      background: var(--vscode-dropdown-background);
      border: 1px solid var(--vscode-dropdown-border);
      border-radius: 4px;
    }
    select:focus-visible, button:focus-visible {
      outline: 1px solid var(--vscode-focusBorder);
      outline-offset: 1px;
    }
    select:disabled, button:disabled {
      opacity: 0.55;
      cursor: not-allowed;
    }
    button {
      margin-top: 4px;
      color: var(--vscode-button-foreground);
      background: var(--vscode-button-secondaryBackground);
      border-color: var(--vscode-button-border, transparent);
      cursor: pointer;
    }
    button:hover:not(:disabled) {
      background: var(--vscode-button-secondaryHoverBackground);
    }
    .status {
      margin: 10px 0 0;
      color: var(--vscode-descriptionForeground);
      font-size: 11px;
      line-height: 1.4;
      min-height: 15px;
    }
    body.busy select,
    body.busy button:not(#refresh) {
      pointer-events: none;
      opacity: 0.7;
    }
  </style>
</head>
<body>
  <h1>Runtime</h1>
  <fieldset id="controls">
  <div class="row">
    <label for="provider">Provider</label>
    <select id="provider"></select>
  </div>
  <div class="row">
    <label for="model">Model</label>
    <select id="model"></select>
  </div>
  <div class="row">
    <label for="think">Thinking</label>
    <select id="think"></select>
  </div>
  <div class="row">
    <label for="hitl">HITL Policy</label>
    <select id="hitl"></select>
  </div>
  </fieldset>
  <button id="refresh" type="button">Refresh runtime</button>
  <p id="status" class="status">Loading…</p>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    const provider = document.getElementById("provider");
    const model = document.getElementById("model");
    const think = document.getElementById("think");
    const hitl = document.getElementById("hitl");
    const status = document.getElementById("status");

    // Provider is a client-side filter over the model select's groups (ACP
    // has one grouped "model" config option, not a separate provider option
    // -- see acpsvc/config_options.go). Selecting a provider re-populates the
    // model list to that group and pushes its first model as the new value.
    let modelState;

    function setBusy(busy) {
      document.body.classList.toggle("busy", busy);
      status.textContent = busy ? "Applying runtime settings…" : "Runtime settings are applied immediately.";
    }

    provider.addEventListener("change", () => {
      if (!modelState) return;
      const group = modelState.groups.find((g) => g.group === provider.value);
      const values = group ? group.values : modelState.values;
      renderModelOptions(values, values[0]?.value);
      if (values[0]) {
        setBusy(true);
        vscode.postMessage({ type: "setModel", value: values[0].value });
      }
    });
    model.addEventListener("change", () => { setBusy(true); vscode.postMessage({ type: "setModel", value: model.value }); });
    think.addEventListener("change", () => { setBusy(true); vscode.postMessage({ type: "setThink", value: think.value }); });
    hitl.addEventListener("change", () => { setBusy(true); vscode.postMessage({ type: "setHitl", value: hitl.value }); });
    document.getElementById("refresh").addEventListener("click", () => vscode.postMessage({ type: "refresh" }));

    window.addEventListener("message", (event) => {
      if (event.data.type === "state") {
        render(event.data.state);
      } else if (event.data.type === "error") {
        status.textContent = event.data.message || "Runtime unavailable";
      }
    });

    function renderModelOptions(values, selectedValue) {
      setOptions(model, values.map((v) => ({ value: v.value, label: v.name })), selectedValue || "");
    }

    function render(state) {
      modelState = state.model;
      if (state.model && state.model.groups.length > 0) {
        const currentGroup = state.model.groups.find((g) => g.values.some((v) => v.value === state.model.currentValue))
          || state.model.groups[0];
        setOptions(provider, state.model.groups.map((g) => ({ value: g.group, label: g.name || g.group })), currentGroup.group);
        renderModelOptions(currentGroup.values, state.model.currentValue);
      } else if (state.model) {
        setOptions(provider, [{ value: "default", label: "default" }], "default");
        renderModelOptions(state.model.values, state.model.currentValue);
      } else {
        setOptions(provider, [], "");
        setOptions(model, [], "");
      }
      if (state.think) {
        setOptions(think, state.think.values.map((v) => ({ value: v.value, label: v.name })), state.think.currentValue);
      } else {
        setOptions(think, [], "");
      }
      if (state.hitl) {
        setOptions(hitl, state.hitl.values.map((v) => ({ value: v.value, label: v.name })), state.hitl.currentValue);
      } else {
        setOptions(hitl, [], "");
      }
      status.textContent = state.configured
        ? "Runtime settings are applied immediately."
        : "Contenox runtime needs setup — run \\"Contenox: Run Guided Setup\\".";
      setBusy(false);
    }

    function setOptions(select, options, selectedValue) {
      select.replaceChildren();
      if (!options.length) {
        const option = document.createElement("option");
        option.value = "";
        option.textContent = "not available";
        select.appendChild(option);
        select.disabled = true;
        return;
      }
      select.disabled = false;
      let hasSelected = false;
      for (const item of options) {
        const option = document.createElement("option");
        option.value = item.value;
        option.textContent = item.label;
        option.selected = item.value === selectedValue;
        hasSelected ||= option.selected;
        select.appendChild(option);
      }
      if (selectedValue && !hasSelected) {
        const option = document.createElement("option");
        option.value = selectedValue;
        option.textContent = selectedValue + " (current)";
        option.selected = true;
        select.prepend(option);
      }
    }
  </script>
</body>
</html>`;
  }
}

function toSelectState(options: SessionConfigOption[], configId: string): RuntimeSelectState | undefined {
  const option = options.find((candidate) => candidate.id === configId);
  // model/think/hitl-policy are always ACP v1 "select" options (see
  // acpsvc/config_options.go); "boolean" options (none of these three) have
  // no `.options` to render as a dropdown.
  if (!option || option.type !== "select") {
    return undefined;
  }
  const entries = option.options as Array<SessionConfigSelectOption | SessionConfigSelectGroup>;
  const groups: RuntimeSelectGroup[] = [];
  const values: RuntimeSelectValue[] = [];
  for (const entry of entries) {
    if (isGroup(entry)) {
      groups.push({
        group: entry.group,
        name: entry.name || entry.group,
        values: entry.options.map(toRuntimeValue),
      });
    } else {
      values.push(toRuntimeValue(entry));
    }
  }
  return { currentValue: String(option.currentValue), groups, values };
}

function isGroup(entry: SessionConfigSelectOption | SessionConfigSelectGroup): entry is SessionConfigSelectGroup {
  return "group" in entry;
}

function toRuntimeValue(value: SessionConfigSelectOption): RuntimeSelectValue {
  return { value: value.value, name: value.name, description: value.description ?? undefined };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
