// Shells out to `contenox approvals` / `contenox inbox` -- the durable ask
// store's only read/write surface (see internal/services/hitlservice and
// internal/services/operatorinbox). Neither has a JSON output mode or an ACP
// method, so this is real, deliberate CLI-table parsing, not a stopgap: it is
// how the panel answers a parked approval from a process other than the one
// that asked (vscode-implementation-plan.md Phase 5 "walk-away" / Phase 6
// "operator inbox"), exactly mirroring `contenox approvals respond` and
// `contenox inbox ack`.
//
// Table parsing relies on tabwriter's own invariant (text/tabwriter): every
// cell in a column is right-padded to that column's widest cell, so a
// column's start offset is identical on the header row and every data row.
// Reading the header once and slicing every row at those offsets is exactly
// how tabwriter output is meant to be consumed without a machine-readable
// mode.
import { execFile } from "node:child_process";
import * as vscode from "vscode";
import { resolveBinaryPath, workspaceCwd } from "../bridge/BridgeProcess";
import { readBridgeSettings } from "../config/settings";

export interface PendingAsk {
  id: string;
  kind: "permission" | "question";
  tool: string;
  summary: string;
  mission: string;
}

export interface InboxRow {
  id: string;
  reason: string;
  mission: string;
  kind: string;
  summary: string;
  acked: boolean;
}

export type ApprovalVerdict = { kind: "approve" } | { kind: "deny" } | { kind: "answer"; text: string };

// resolveBinaryPath only needs extensionUri for the bundled-binary fallback;
// every caller here passes the real one so a bundled `bin/contenox` resolves
// exactly as it does for the spawned `contenox acp` process.
function contenoxArgsWith(extensionUri: vscode.Uri, extra: string[]): { bin: string; args: string[]; cwd: string | undefined } {
  const settings = readBridgeSettings();
  const args: string[] = [];
  if (settings.dataDir) {
    args.push("--data-dir", settings.dataDir);
  }
  args.push(...extra);
  return { bin: resolveBinaryPath(settings.binaryPath, extensionUri), args, cwd: workspaceCwd() };
}

function runContenox(extensionUri: vscode.Uri, extra: string[]): Promise<string> {
  const { bin, args, cwd } = contenoxArgsWith(extensionUri, extra);
  return new Promise((resolve, reject) => {
    execFile(bin, args, { cwd, env: { ...process.env, NO_COLOR: "1" }, timeout: 20_000 }, (error, stdout, stderr) => {
      if (error) {
        reject(new Error(stderr?.trim() || error.message));
        return;
      }
      resolve(stdout);
    });
  });
}

// isApprovalPending checks one ask id's presence in `approvals list` -- a
// direct substring/line match rather than full-table parsing, since the id
// is a unique token (uuid or synthetic call id) that only ever appears at
// the start of its own row.
export async function isApprovalPending(extensionUri: vscode.Uri, approvalId: string): Promise<boolean> {
  const stdout = await runContenox(extensionUri, ["approvals", "list", "--limit", "500"]);
  const escaped = approvalId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`^${escaped}\\s`, "m").test(stdout);
}

// respondApproval answers a durable ask (approve/deny/answer) exactly the
// way `contenox approvals respond` does, in a fresh process -- which is the
// whole point of the durable row: the process that asked does not need to
// still be alive (resume.go's ResumeFromCheckpoint). Returns the CLI's own
// confirmation line for logging/telemetry.
export async function respondApproval(extensionUri: vscode.Uri, approvalId: string, verdict: ApprovalVerdict): Promise<string> {
  const args = ["approvals", "respond", approvalId];
  switch (verdict.kind) {
    case "approve":
      args.push("--approve");
      break;
    case "deny":
      args.push("--deny");
      break;
    case "answer":
      args.push("--answer", verdict.text);
      break;
  }
  return runContenox(extensionUri, args);
}

// listPendingApprovals parses `approvals list`'s tabwriter table (ID KIND
// TOOL SUMMARY MISSION AGE EXPIRES-IN) into rows for the inbox tree. Returns
// [] for the empty-state message, never throws on "nothing pending".
export async function listPendingApprovals(extensionUri: vscode.Uri): Promise<PendingAsk[]> {
  const stdout = await runContenox(extensionUri, ["approvals", "list", "--limit", "200"]);
  const table = parseTabwriterTable(stdout, ["ID", "KIND", "TOOL", "SUMMARY", "MISSION", "AGE", "EXPIRES-IN"]);
  return table.map((row) => ({
    id: row.ID ?? "",
    kind: row.KIND === "question" ? "question" : "permission",
    tool: row.TOOL ?? "",
    summary: row.SUMMARY ?? "",
    mission: row.MISSION ?? "",
  }));
}

// listInboxUnacked parses `inbox list` (ID REASON MISSION KIND SUMMARY AGE
// ACKED) -- unacknowledged mission reports/blockers with no live supervisor
// (internal/services/operatorinbox), distinct from the live ask queue above.
export async function listInboxUnacked(extensionUri: vscode.Uri): Promise<InboxRow[]> {
  const stdout = await runContenox(extensionUri, ["inbox", "list", "--limit", "200"]);
  const table = parseTabwriterTable(stdout, ["ID", "REASON", "MISSION", "KIND", "SUMMARY", "AGE", "ACKED"]);
  return table.map((row) => ({
    id: row.ID ?? "",
    reason: row.REASON ?? "",
    mission: row.MISSION ?? "",
    kind: row.KIND ?? "",
    summary: row.SUMMARY ?? "",
    acked: (row.ACKED ?? "no").toLowerCase() === "yes",
  }));
}

export async function ackInboxItem(extensionUri: vscode.Uri, id: string): Promise<void> {
  await runContenox(extensionUri, ["inbox", "ack", id]);
}

// parseTabwriterTable finds the header line (all `columns` present, in
// order) and slices every following non-blank line at the header's own
// column start offsets. Stops at the first blank line or a line that starts
// before the table's left margin (the trailing "Answer with..."/diff dump
// both CLIs print after their table). Returns [] if no header line is found
// (the "No pending asks"/"No unacknowledged..." empty-state message).
function parseTabwriterTable(stdout: string, columns: string[]): Array<Record<string, string>> {
  const lines = stdout.split(/\r?\n/);
  const headerIndex = lines.findIndex((line) => columns.every((col) => line.includes(col)));
  if (headerIndex === -1) {
    return [];
  }
  const header = lines[headerIndex];
  const offsets = columns.map((col) => header.indexOf(col));
  if (offsets.some((offset) => offset === -1)) {
    return [];
  }
  const rows: Array<Record<string, string>> = [];
  for (let i = headerIndex + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.trim() === "") {
      break;
    }
    const row: Record<string, string> = {};
    for (let c = 0; c < columns.length; c += 1) {
      const start = offsets[c];
      const end = c + 1 < offsets.length ? offsets[c + 1] : undefined;
      row[columns[c]] = line.slice(start, end).trim();
    }
    if (row[columns[0]]) {
      rows.push(row);
    }
  }
  return rows;
}
