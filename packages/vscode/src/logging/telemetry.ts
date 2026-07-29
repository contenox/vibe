import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import * as vscode from "vscode";

type TelemetryFields = Record<string, unknown>;

const redactedKeyPattern = /(api|auth|content|env|input|key|password|prompt|secret|token)/i;

export class TelemetryLogger implements vscode.Disposable {
  private disposed = false;

  public constructor(private readonly extensionVersion: string) {}

  public event(name: string, fields: TelemetryFields = {}): void {
    this.write("info", name, fields);
  }

  public warn(name: string, fields: TelemetryFields = {}): void {
    this.write("warn", name, fields);
  }

  public error(name: string, error: unknown, fields: TelemetryFields = {}): void {
    this.write("error", name, {
      ...fields,
      error: errorMessage(error),
      errorName: error instanceof Error ? error.name : undefined,
    });
  }

  public logPath(): string {
    return telemetryPath();
  }

  public async show(): Promise<void> {
    const file = this.logPath();
    await ensureParentDir(file);
    if (!fs.existsSync(file)) {
      await fs.promises.writeFile(file, "");
    }
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(file));
    await vscode.window.showTextDocument(doc, { preview: false });
  }

  public async clear(): Promise<void> {
    const file = this.logPath();
    await ensureParentDir(file);
    await fs.promises.writeFile(file, "");
    this.event("telemetry.cleared");
  }

  public dispose(): void {
    this.disposed = true;
  }

  private write(level: "info" | "warn" | "error", name: string, fields: TelemetryFields): void {
    if (this.disposed || !telemetryEnabled()) {
      return;
    }
    const file = this.logPath();
    const record = {
      ts: new Date().toISOString(),
      level,
      name,
      extensionVersion: this.extensionVersion,
      vscodeVersion: vscode.version,
      remoteName: vscode.env.remoteName,
      ...sanitizeFields(fields),
    };

    void ensureParentDir(file)
      .then(() => fs.promises.appendFile(file, `${JSON.stringify(record)}\n`))
      .catch(() => {
        // Telemetry is diagnostic-only; never break extension behavior on log IO.
      });
  }
}

export function telemetryPath(): string {
  const configuredDataDir = vscode.workspace.getConfiguration("contenox").get<string>("dataDir", "").trim();
  const root = configuredDataDir ? resolvePath(configuredDataDir) : path.join(os.homedir(), ".contenox");
  return path.join(root, "vscode-telemetry.log");
}

function telemetryEnabled(): boolean {
  return vscode.workspace.getConfiguration("contenox").get<boolean>("telemetry.enabled", true);
}

function resolvePath(value: string): string {
  if (path.isAbsolute(value)) {
    return value;
  }
  const workspace = vscode.workspace.workspaceFolders?.find((folder) => folder.uri.scheme === "file");
  return path.resolve(workspace?.uri.fsPath ?? os.homedir(), value);
}

async function ensureParentDir(file: string): Promise<void> {
  await fs.promises.mkdir(path.dirname(file), { recursive: true });
}

function sanitizeFields(fields: TelemetryFields): TelemetryFields {
  const out: TelemetryFields = {};
  for (const [key, value] of Object.entries(fields)) {
    out[key] = sanitizeValue(key, value);
  }
  return out;
}

function sanitizeValue(key: string, value: unknown): unknown {
  if (value === undefined) {
    return undefined;
  }
  if (redactedKeyPattern.test(key)) {
    return summarizeRedacted(value);
  }
  if (value instanceof Error) {
    return errorMessage(value);
  }
  if (typeof value === "string") {
    return value.length > 512 ? `${value.slice(0, 512)}...` : value;
  }
  if (typeof value === "number" || typeof value === "boolean" || value === null) {
    return value;
  }
  if (Array.isArray(value)) {
    return value.slice(0, 20).map((item, index) => sanitizeValue(`${key}.${index}`, item));
  }
  if (typeof value === "object") {
    const out: TelemetryFields = {};
    for (const [childKey, childValue] of Object.entries(value as TelemetryFields)) {
      out[childKey] = sanitizeValue(childKey, childValue);
    }
    return out;
  }
  return String(value);
}

function summarizeRedacted(value: unknown): string {
  if (typeof value === "string") {
    return `[redacted string len=${value.length}]`;
  }
  if (Array.isArray(value)) {
    return `[redacted array len=${value.length}]`;
  }
  if (value && typeof value === "object") {
    return `[redacted object keys=${Object.keys(value).length}]`;
  }
  return "[redacted]";
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
