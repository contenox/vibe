import * as vscode from "vscode";
import { BridgeProcess } from "../bridge/BridgeProcess";
import { MCPServerInfo } from "../bridge/protocol";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

const providerId = "contenox.mcpServers";

export interface MCPServerProviderRegistration extends vscode.Disposable {
  refresh(): void;
}

export function registerMCPServerProvider(bridge: BridgeProcess, telemetry: TelemetryLogger): MCPServerProviderRegistration {
  const provider = new ContenoxMCPServerProvider(bridge, telemetry);
  const registration = vscode.lm.registerMcpServerDefinitionProvider(providerId, provider);
  telemetry.event("mcp.provider.registered", { providerId });
  return {
    refresh: () => provider.refresh(),
    dispose: () => {
      registration.dispose();
      provider.dispose();
    },
  };
}

class ContenoxMCPServerProvider implements vscode.McpServerDefinitionProvider {
  private readonly changeEmitter = new vscode.EventEmitter<void>();
  public readonly onDidChangeMcpServerDefinitions = this.changeEmitter.event;

  public constructor(
    private readonly bridge: BridgeProcess,
    private readonly telemetry: TelemetryLogger,
  ) {}

  public refresh(): void {
    this.telemetry.event("mcp.provider.refresh", { providerId });
    this.changeEmitter.fire();
  }

  public dispose(): void {
    this.changeEmitter.dispose();
  }

  public async provideMcpServerDefinitions(_token: vscode.CancellationToken): Promise<vscode.McpServerDefinition[]> {
    try {
      const state = await this.bridge.ensureStarted();
      if (!state.initialize.capabilities.mcp) {
        this.telemetry.warn("mcp.provider.skipped", { reason: "runtime_capability_missing" });
        return [];
      }
      const client = this.bridge.currentClient;
      if (!client) {
        this.telemetry.warn("mcp.provider.skipped", { reason: "runtime_connection_missing" });
        return [];
      }
      const result = await client.listMCPServers();
      const definitions = result.servers.flatMap((server) => {
        const definition = definitionFromServer(server, this.telemetry);
        return definition ? [definition] : [];
      });
      this.telemetry.event("mcp.provider.definitions", {
        serverCount: result.servers.length,
        definitionCount: definitions.length,
      });
      return definitions;
    } catch (error) {
      this.telemetry.error("mcp.provider.failed", error);
      return [];
    }
  }

  public resolveMcpServerDefinition(server: vscode.McpServerDefinition, _token: vscode.CancellationToken): vscode.ProviderResult<vscode.McpServerDefinition> {
    return server;
  }
}

export async function showMCPServers(bridge: BridgeProcess, output: ContenoxOutput, telemetry: TelemetryLogger): Promise<void> {
  output.show();
  try {
    const state = await bridge.ensureStarted();
    if (!state.initialize.capabilities.mcp) {
      vscode.window.showWarningMessage("The active Contenox runtime does not expose MCP server definitions.");
      return;
    }
    const client = bridge.currentClient;
    if (!client) {
      throw new Error("Contenox runtime connection is not available");
    }
    const result = await client.listMCPServers();
    telemetry.event("mcp.servers.show", { count: result.servers.length });
    if (result.servers.length === 0) {
      output.info("Contenox MCP servers: none registered");
      vscode.window.showInformationMessage("No Contenox MCP servers are registered.");
      return;
    }
    output.info(`Contenox MCP servers (${result.servers.length}):`);
    let exposed = 0;
    for (const server of result.servers) {
      const skippedReason = reasonNotExposedToVSCodeMCP(server);
      if (!skippedReason) {
        exposed++;
      }
      const auth = normalizedAuthType(server.authType);
      const authLabel = auth ? `/${auth}` : "";
      const exposure = skippedReason ? ` (Contenox runtime only: ${skippedReason})` : " (exposed to VS Code MCP)";
      output.info(`- ${server.name} [${server.transport}${authLabel}] ${server.command || server.url || ""}${exposure}`.trim());
    }
    vscode.window.showInformationMessage(`Contenox MCP servers registered: ${result.servers.length}; exposed to VS Code MCP: ${exposed}. See Contenox output.`);
  } catch (error) {
    telemetry.error("mcp.servers.show.failed", error);
    vscode.window.showErrorMessage(errorMessage(error));
  }
}

function definitionFromServer(server: MCPServerInfo, telemetry: TelemetryLogger): vscode.McpServerDefinition | undefined {
  const skippedReason = reasonNotExposedToVSCodeMCP(server);
  if (skippedReason) {
    telemetry.warn("mcp.provider.server_skipped", {
      serverName: server.name,
      transport: server.transport,
      authType: server.authType,
      reason: skippedReason,
    });
    return undefined;
  }

  switch (server.transport) {
    case "stdio":
      if (!server.command) {
        telemetry.warn("mcp.provider.server_skipped", { serverName: server.name, reason: "stdio_missing_command" });
        return undefined;
      }
      return new vscode.McpStdioServerDefinition(
        server.name,
        server.command,
        server.args ?? [],
        environmentForServer(server),
        server.id,
      );
    case "http":
      if (!server.url) {
        telemetry.warn("mcp.provider.server_skipped", { serverName: server.name, reason: "http_missing_url" });
        return undefined;
      }
      return new vscode.McpHttpServerDefinition(server.name, vscode.Uri.parse(server.url), headersForServer(server, telemetry), server.id);
    case "sse":
      telemetry.warn("mcp.provider.server_skipped", {
        serverName: server.name,
        reason: "sse_not_supported_by_current_vscode_api",
      });
      return undefined;
    default:
      telemetry.warn("mcp.provider.server_skipped", { serverName: server.name, transport: server.transport, reason: "unknown_transport" });
      return undefined;
  }
}

function environmentForServer(server: MCPServerInfo): Record<string, string | number | null> {
  const env: Record<string, string | number | null> = {};
  if (server.authEnvKey && process.env[server.authEnvKey]) {
    env[server.authEnvKey] = process.env[server.authEnvKey] ?? null;
  }
  return env;
}

function headersForServer(server: MCPServerInfo, telemetry: TelemetryLogger): Record<string, string> {
  const headers: Record<string, string> = { ...(server.headers ?? {}) };
  if (server.authType === "bearer") {
    if (server.authEnvKey && process.env[server.authEnvKey]) {
      headers.Authorization = `Bearer ${process.env[server.authEnvKey]}`;
    } else {
      telemetry.warn("mcp.provider.auth_header_missing", {
        serverName: server.name,
        authType: server.authType,
        hasAuthEnvKey: Boolean(server.authEnvKey),
      });
    }
  }
  return headers;
}

function reasonNotExposedToVSCodeMCP(server: MCPServerInfo): string | undefined {
  switch (server.transport) {
    case "stdio":
      if (!server.command) {
        return "stdio_missing_command";
      }
      break;
    case "http":
      if (!server.url) {
        return "http_missing_url";
      }
      break;
    case "sse":
      return "sse_not_supported_by_current_vscode_api";
    default:
      return "unknown_transport";
  }

  const authType = normalizedAuthType(server.authType);
  if (authType === "" || authType === "none") {
    return undefined;
  }
  if (authType === "oauth") {
    return "oauth_managed_by_contenox_runtime";
  }
  if (authType !== "bearer") {
    return "unsupported_auth_type";
  }
  if (server.transport === "stdio") {
    return server.authEnvKey && process.env[server.authEnvKey] ? undefined : "bearer_auth_env_missing";
  }
  if (server.transport === "http") {
    return bearerHeaderAvailable(server) ? undefined : "bearer_auth_header_missing";
  }
  return undefined;
}

function bearerHeaderAvailable(server: MCPServerInfo): boolean {
  if (server.authEnvKey && process.env[server.authEnvKey]) {
    return true;
  }
  const headers = server.headers ?? {};
  return Object.keys(headers).some((key) => key.toLowerCase() === "authorization" && headers[key].trim() !== "");
}

function normalizedAuthType(authType: string | undefined): string {
  return (authType ?? "").trim().toLowerCase();
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
