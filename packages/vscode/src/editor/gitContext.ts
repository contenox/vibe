import { execFile } from "node:child_process";
import { promisify } from "node:util";
import * as vscode from "vscode";
import { EditorContextAttachment } from "../bridge/protocol";

const execFileAsync = promisify(execFile);
const defaultMaxGitContextChars = 120000;

export async function collectGitChangeContext(maxChars = defaultMaxGitContextChars): Promise<EditorContextAttachment[]> {
  const cwd = workspaceCwd();
  if (!cwd) {
    return [];
  }
  const root = await gitOutput(cwd, ["rev-parse", "--show-toplevel"]);
  if (!root.trim()) {
    return [];
  }

  const [status, stagedDiff, unstagedDiff] = await Promise.all([
    gitOutput(root.trim(), ["status", "--short"]),
    gitOutput(root.trim(), ["diff", "--cached", "--no-ext-diff"]),
    gitOutput(root.trim(), ["diff", "--no-ext-diff"]),
  ]);
  const content = truncateGitContext(formatGitContext(status, stagedDiff, unstagedDiff), maxChars);
  if (!content.trim()) {
    return [];
  }
  return [
    {
      kind: "git_changes",
      uri: vscode.Uri.file(root.trim()).toString(),
      languageId: "diff",
      content,
    },
  ];
}

function workspaceCwd(): string | undefined {
  const folder = vscode.workspace.workspaceFolders?.find((candidate) => candidate.uri.scheme === "file");
  return folder?.uri.fsPath;
}

async function gitOutput(cwd: string, args: string[]): Promise<string> {
  try {
    const { stdout } = await execFileAsync("git", ["-C", cwd, ...args], {
      maxBuffer: 4 * 1024 * 1024,
      windowsHide: true,
    });
    return stdout.trimEnd();
  } catch (error) {
    const maybe = error as { stdout?: unknown; stderr?: unknown };
    if (typeof maybe.stdout === "string" && maybe.stdout.trim()) {
      return maybe.stdout.trimEnd();
    }
    if (typeof maybe.stderr === "string" && /not a git repository/i.test(maybe.stderr)) {
      return "";
    }
    throw error;
  }
}

function formatGitContext(status: string, stagedDiff: string, unstagedDiff: string): string {
  const sections = [
    ["Git status", status],
    ["Staged diff", stagedDiff],
    ["Unstaged diff", unstagedDiff],
  ];
  return sections
    .filter(([, body]) => body.trim())
    .map(([title, body]) => `## ${title}\n\n${body}`)
    .join("\n\n");
}

function truncateGitContext(value: string, maxChars: number): string {
  if (value.length <= maxChars) {
    return value;
  }
  return `${value.slice(0, maxChars)}\n\n[Contenox truncated git context at ${maxChars} characters.]`;
}
