import * as vscode from "vscode";

export interface BridgeSettings {
  binaryPath: string;
  dataDir?: string;
  startOnActivation: boolean;
  requestTimeoutMs: number;
  logProtocol: boolean;
  telemetryEnabled: boolean;
}

export interface AcpSettings {
  chainPath?: string;
}

export interface AutocompleteSettings {
  enabled: boolean;
  provider?: string;
  model?: string;
  maxPrefixChars: number;
  maxSuffixChars: number;
  maxDocumentChars: number;
  maxTokens: number;
  debounceMs: number;
}

export function readBridgeSettings(): BridgeSettings {
  const config = vscode.workspace.getConfiguration("contenox");
  const binaryPath = normalize(config.get<string>("binaryPath"), "contenox");
  const dataDir = normalize(config.get<string>("dataDir"), "");
  const requestTimeoutMs = config.get<number>("requestTimeoutMs", 15000);

  return {
    binaryPath,
    dataDir: dataDir || undefined,
    startOnActivation: config.get<boolean>("startOnActivation", true),
    requestTimeoutMs: Math.max(1000, requestTimeoutMs),
    logProtocol: config.get<boolean>("logProtocol", false),
    telemetryEnabled: config.get<boolean>("telemetry.enabled", true),
  };
}

// readAcpSettings reads contenox.chainPath -- passed as CONTENOX_ACP_CHAIN_PATH
// when spawning `contenox acp`, so answer quality is attributable to a chain
// rather than blamed on the port (vscode-implementation-plan.md Phase 1).
// Empty/unset means the runtime's own default (chain-acp.json).
export function readAcpSettings(): AcpSettings {
  const config = vscode.workspace.getConfiguration("contenox");
  const chainPath = normalize(config.get<string>("chainPath"), "");
  return { chainPath: chainPath || undefined };
}

export function readAutocompleteSettings(): AutocompleteSettings {
  const config = vscode.workspace.getConfiguration("contenox");
  const provider = normalize(config.get<string>("autocompleteProvider"), "");
  const model = normalize(config.get<string>("autocompleteModel"), "");
  const maxOutputTokens = config.get<number>("maxOutputTokens", 0);
  return {
    enabled: config.get<boolean>("autocomplete.enabled", false),
    provider: provider || undefined,
    model: model || undefined,
    maxPrefixChars: clamp(config.get<number>("autocomplete.maxPrefixChars", 6000), 256, 200000),
    maxSuffixChars: clamp(config.get<number>("autocomplete.maxSuffixChars", 2000), 0, 200000),
    maxDocumentChars: clamp(config.get<number>("autocomplete.maxDocumentChars", 200000), 1000, 2000000),
    maxTokens: clamp(config.get<number>("autocomplete.maxTokens", maxOutputTokens || 128), 16, 2048),
    debounceMs: clamp(config.get<number>("autocomplete.debounceMs", 180), 0, 2000),
  };
}

function normalize(value: string | undefined, fallback: string): string {
  const trimmed = value?.trim();
  return trimmed ? trimmed : fallback;
}

function clamp(value: number | undefined, min: number, max: number): number {
  const n = Number.isFinite(value) ? Number(value) : min;
  return Math.max(min, Math.min(max, n));
}
