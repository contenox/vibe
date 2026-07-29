import { spawn, ChildProcessWithoutNullStreams } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import * as vscode from "vscode";
import { readBridgeSettings } from "../config/settings";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";
import { ContenoxStatusBar } from "../status/statusBar";
import { BridgeClient } from "./BridgeClient";
import { HealthResult, InitializeResult } from "./protocol";

export interface BridgeState {
  initialize: InitializeResult;
  health: HealthResult;
}

export class BridgeProcess implements vscode.Disposable {
  private child: ChildProcessWithoutNullStreams | undefined;
  private client: BridgeClient | undefined;
  private state: BridgeState | undefined;
  private starting: Promise<BridgeState> | undefined;
  private stoppingChild: ChildProcessWithoutNullStreams | undefined;
  private disposed = false;

  public constructor(
    private readonly output: ContenoxOutput,
    private readonly status: ContenoxStatusBar,
    private readonly extensionVersion: string,
    public readonly extensionUri: vscode.Uri,
    private readonly telemetry: TelemetryLogger,
  ) {}

  public get currentClient(): BridgeClient | undefined {
    return this.client;
  }

  public get currentState(): BridgeState | undefined {
    return this.state;
  }

  public commandBinaryPath(): string {
    return resolveBinaryPath(
      readBridgeSettings().binaryPath,
      this.extensionUri,
    );
  }

  public commandCwd(): string | undefined {
    return workspaceCwd();
  }

  public ensureStarted(): Promise<BridgeState> {
    if (this.state && this.client) {
      return Promise.resolve(this.state);
    }
    this.telemetry.event("runtime.ensure_started", {
      alreadyStarting: Boolean(this.starting),
    });
    if (this.starting) {
      return this.starting;
    }
    this.starting = this.start().finally(() => {
      this.starting = undefined;
    });
    return this.starting;
  }

  public async restart(): Promise<BridgeState> {
    this.telemetry.event("runtime.restart");
    await this.stop();
    return this.ensureStarted();
  }

  public async refreshHealth(): Promise<HealthResult> {
    if (!this.client) {
      throw new Error("Contenox runtime connection is not available");
    }
    const health = await this.client.health();
    if (this.state) {
      this.state = { ...this.state, health };
    }
    this.status.setReady(health);
    return health;
  }

  public async stop(): Promise<void> {
    const child = this.child;
    const client = this.client;
    this.state = undefined;
    this.client = undefined;
    this.child = undefined;
    this.stoppingChild = child;
    this.status.setStopped();
    this.telemetry.event("runtime.stop", {
      hadChild: Boolean(child),
      hadClient: Boolean(client),
    });

    if (client) {
      try {
        await client.shutdown();
      } catch (error) {
        this.output.warn(
          `Runtime shutdown request failed: ${errorMessage(error)}`,
        );
      }
      client.dispose();
    }
    if (child && !child.killed) {
      child.kill();
    }
  }

  public dispose(): void {
    this.disposed = true;
    void this.stop();
  }

  private async start(): Promise<BridgeState> {
    if (this.disposed) {
      throw new Error("Contenox runtime process is disposed");
    }

    const settings = readBridgeSettings();
    const cwd = workspaceCwd();
    const args = bridgeArgs(settings.dataDir);
    const binaryPath = resolveBinaryPath(
      settings.binaryPath,
      this.extensionUri,
    );

    const startAt = Date.now();
    this.status.setStarting();
    this.output.info(
      `Starting Contenox runtime: ${binaryPath} ${args.join(" ")}`,
    );
    this.telemetry.event("runtime.spawn.start", {
      binaryPath,
      args,
      cwd,
      dataDir: settings.dataDir,
      logProtocol: settings.logProtocol,
      telemetryEnabled: settings.telemetryEnabled,
      platform: process.platform,
      arch: process.arch,
      remoteName: vscode.env.remoteName,
    });
    if (vscode.env.remoteName) {
      this.output.info(
        `VS Code remote environment: ${vscode.env.remoteName} (${process.platform}/${process.arch})`,
      );
    }

    const child = spawn(binaryPath, args, {
      cwd,
      env: { ...process.env, NO_COLOR: "1" },
      stdio: "pipe",
      windowsHide: true,
    });
    this.child = child;
    this.telemetry.event("runtime.spawn.pid", { pid: child.pid });

    child.stderr.on("data", (chunk: Buffer) => {
      const text = chunk.toString("utf8").trimEnd();
      if (text) {
        this.output.info(`[runtime stderr] ${text}`);
        this.telemetry.warn("runtime.stderr", {
          bytes: chunk.byteLength,
          lineCount: text.split(/\r?\n/).length,
          sample: text.slice(0, 512),
        });
      }
    });
    child.on("exit", (code, signal) => {
      const detail = signal ? `signal ${signal}` : `code ${String(code)}`;
      if (this.stoppingChild === child) {
        this.stoppingChild = undefined;
        this.output.info(`Contenox runtime stopped with ${detail}`);
        this.telemetry.event("runtime.exit.expected", { code, signal });
        return;
      }
      this.output.warn(`Contenox runtime exited with ${detail}`);
      this.telemetry.warn("runtime.exit.unexpected", { code, signal });
      if (this.child === child) {
        this.client?.dispose();
        this.child = undefined;
        this.client = undefined;
        this.state = undefined;
        this.status.setCrashed();
      }
    });

    const client = new BridgeClient(
      child.stdin,
      child.stdout,
      this.output,
      settings.requestTimeoutMs,
      settings.logProtocol,
      this.telemetry,
    );
    this.client = client;

    let spawnErrorHandler: ((error: Error) => void) | undefined;
    try {
      const spawnError = new Promise<never>((_, reject) => {
        spawnErrorHandler = (error: Error) => {
          reject(error);
        };
        child.once("error", spawnErrorHandler);
      });
      const initTimeout = new Promise<never>((_, reject) =>
        setTimeout(() => reject(new Error("Contenox runtime initialize timed out after 60s")), 60_000),
      );
      const initialize = await Promise.race([
        client.initialize({
          clientInfo: {
            name: "contenox-vscode",
            version: this.extensionVersion,
          },
          workspace: vscode.workspace.name,
          workspacePath: cwd,
        }),
        spawnError,
        initTimeout,
      ]);
      if (spawnErrorHandler) {
        child.off("error", spawnErrorHandler);
      }
      const health = await client.health();
      this.state = { initialize, health };
      this.status.setReady(health);
      const durationMs = Date.now() - startAt;
      this.telemetry.event("runtime.spawn.ready", {
        protocolVersion: initialize.protocolVersion,
        serverVersion: initialize.serverVersion,
        stateDir: initialize.stateDir,
        workspaceMode: initialize.workspaceMode,
        healthStatus: health.status,
        configured: health.configured,
        defaultProvider: health.defaultProvider,
        defaultModel: health.defaultModel,
        durationMs,
      });
      this.output.info(`Contenox runtime ready in ${durationMs}ms`);
      return this.state;
    } catch (error) {
      if (spawnErrorHandler) {
        child.off("error", spawnErrorHandler);
      }
      this.status.setCrashed();
      client.dispose();
      if (!child.killed) {
        child.kill();
      }
      this.client = undefined;
      this.child = undefined;
      this.telemetry.error("runtime.spawn.failed", error, { binaryPath, cwd });
      throw new Error(
        `Failed to start Contenox runtime: ${errorMessage(error)}`,
      );
    }
  }
}

export function resolveBinaryPath(
  configured: string,
  extensionUri: vscode.Uri,
): string {
  if (configured && configured !== "contenox") {
    return configured;
  }
  const executable = process.platform === "win32" ? "contenox.exe" : "contenox";
  const bundled = path.join(extensionUri.fsPath, "bin", executable);
  if (fs.existsSync(bundled)) {
    return bundled;
  }
  return configured || "contenox";
}

export function bridgeCommandArgs(
  dataDir: string | undefined,
  command: string,
): string[] {
  const args: string[] = [];
  if (dataDir) {
    args.push("--data-dir", dataDir);
  }
  args.push(command);
  return args;
}

function bridgeArgs(dataDir: string | undefined): string[] {
  return [...bridgeCommandArgs(dataDir, "vscode-agent"), "--stdio"];
}

export function workspaceCwd(): string | undefined {
  const folder = vscode.workspace.workspaceFolders?.[0];
  if (!folder || folder.uri.scheme !== "file") {
    return undefined;
  }
  return path.normalize(folder.uri.fsPath);
}

function errorMessage(error: unknown): string {
  if (error instanceof Error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") {
      return `${error.message}. Install the contenox binary${runtimeLocation()}, set contenox.binaryPath to a runtime available there, or install a VSIX/Marketplace package that bundles the matching runtime.`;
    }
    if (code === "EACCES") {
      return `${error.message}. The Contenox runtime is not executable${runtimeLocation()}; fix its permissions or set contenox.binaryPath to an executable runtime.`;
    }
    if (code === "ENOEXEC") {
      return `${error.message}. The bundled Contenox runtime cannot execute on ${process.platform}/${process.arch}${runtimeLocation()}; install the matching VSIX/Marketplace target or set contenox.binaryPath to a compatible runtime.`;
    }
    return error.message;
  }
  return String(error);
}

function runtimeLocation(): string {
  const remote = vscode.env.remoteName
    ? ` in the ${vscode.env.remoteName} remote environment`
    : "";
  return `${remote} for ${process.platform}/${process.arch}`;
}
