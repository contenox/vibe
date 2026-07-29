import * as assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { PassThrough } from "node:stream";
import * as vscode from "vscode";
import type { AcpChatClient } from "../acp/AcpChatClient";
import { approvalToolName } from "../approval/nativeTool";
import { BridgeClient } from "../bridge/BridgeClient";
import { JsonRpcFramer } from "../bridge/JsonRpcFramer";
import type { RequestPermissionParams } from "../bridge/protocol";
import { approvalEventFromPermissionRequest } from "../chat/permissionOutcome";
import { SessionTreeProvider } from "../chat/SessionTreeProvider";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

suite("Contenox VS Code extension", () => {
  test("activates and registers core commands", async () => {
    const extension = vscode.extensions.getExtension("contenox.contenox-runtime");
    assert.ok(extension, "extension should be available in the Extension Development Host");
    await extension.activate();

    const commands = await vscode.commands.getCommands(true);
    for (const command of [
      "contenox.openWalkthrough",
      "contenox.openChat",
      "contenox.openRuntimeSettings",
      "contenox.runSetup",
      "contenox.showStatus",
      "contenox.doctor",
      "contenox.refreshDefaults",
      "contenox.showExtensionRuntimeInfo",
      "contenox.restartRuntime",
      "contenox.testAutocompleteAtCursor",
      "contenox.doctor",
      "contenox.refreshDefaults",
    ]) {
      assert.ok(commands.includes(command), `${command} should be registered`);
    }

    assert.ok(vscode.lm.tools.some((tool) => tool.name === approvalToolName), `${approvalToolName} should be registered`);
  });

  test("manifest keeps command titles, walkthroughs, and welcome content coherent", () => {
    const manifest = readManifest();
    const contributes = manifest.contributes;

    assert.deepEqual(manifest.extensionKind, ["workspace"]);
    const controlsView = contributes.views?.contenox?.find((entry) => entry.id === "contenox.controls");
    assert.ok(controlsView, "contenox.controls view should be contributed");
    assert.equal((controlsView as { type?: string; name?: string }).type, "webview");
    assert.equal((controlsView as { name?: string }).name, "Runtime");
    assert.equal(contributes.views?.contenox?.[0]?.id, "contenox.chat");
    assert.ok(contributes.viewsWelcome?.some((entry) => entry.view === "contenox.controls"));
    assert.ok(contributes.viewsWelcome?.some((entry) => entry.view === "contenox.sessions"));

    const walkthrough = contributes.walkthroughs?.find((entry) => entry.id === "getStarted");
    assert.ok(walkthrough, "getStarted walkthrough should be contributed");
    assert.equal(walkthrough.steps.length, 5);

    const commandTitles = new Map(contributes.commands.map((command) => [command.command, command.title]));
    assert.equal(commandTitles.get("contenox.openWalkthrough"), "Open Walkthrough");
    assert.equal(commandTitles.get("contenox.showExtensionRuntimeInfo"), "Show Runtime Info");
    assert.equal(commandTitles.get("contenox.restartRuntime"), "Restart Runtime");
    assert.equal(commandTitles.has("contenox.restartBridge"), false);
    assert.equal(commandTitles.has("contenox.openAgentSession"), false);
    assert.equal(commandTitles.has("contenox.diagnoseAgentSessions"), false);
    assert.equal(commandTitles.has("contenox.selectModel"), false);
    for (const [command, title] of commandTitles) {
      assert.ok(!title.startsWith("Contenox:"), `${command} should not duplicate the Contenox category prefix`);
    }
    for (const command of ["contenox.selectProvider", "contenox.selectChatModel", "contenox.selectHitlPolicy", "contenox.selectThinkLevel"]) {
      assert.ok(commandTitles.has(command), `${command} should be contributed for the sessions sidebar`);
    }
    assert.ok(commandTitles.has("contenox.doctor"), "contenox.doctor should be contributed");
    assert.ok(commandTitles.has("contenox.refreshDefaults"), "contenox.refreshDefaults should be contributed");

    const approvalTool = contributes.languageModelTools?.find((tool) => tool.name === approvalToolName);
    assert.ok(approvalTool, `${approvalToolName} should be contributed`);
    assert.equal(approvalTool.displayName, "Approve Contenox Tool Call");
    assert.deepEqual(approvalTool.inputSchema.required, ["approvalId", "title", "summary"]);

    const referencedMarkdown = walkthrough.steps.map((step) => step.media?.markdown).filter(isString);
    for (const relativePath of referencedMarkdown) {
      const absolutePath = path.join(extensionRoot(), relativePath);
      assert.ok(fs.existsSync(absolutePath), `${relativePath} should exist`);
    }
  });

  test("sessions tree lists sessions without duplicating runtime config rows", async () => {
    const acpClient = {
      listSessions: async () => ({
        sessions: [
          {
            sessionId: "session-1",
            cwd: "/tmp/contenox-test",
            title: "First session",
            updatedAt: new Date().toISOString(),
          },
        ],
      }),
    } as unknown as AcpChatClient;
    const provider = new SessionTreeProvider(acpClient);
    try {
      const children = (await provider.getChildren()) as Array<{ kind: string; label?: string }>;
      assert.equal(children.length, 1);
      assert.equal(children[0].kind, "session");
    } finally {
      provider.dispose();
    }
  });

  test("bridge answers ACP-shaped permission requests", async () => {
    const outbound = new PassThrough();
    const inbound = new PassThrough();
    const output = new ContenoxOutput();
    const telemetry = new TelemetryLogger("test");
    const client = new BridgeClient(outbound, inbound, output, 1000, false, telemetry);
    const responses: unknown[] = [];
    const responseFramer = new JsonRpcFramer(new PassThrough(), (message) => responses.push(message), (error) => {
      throw error;
    });
    outbound.on("data", (chunk: Buffer) => responseFramer.accept(chunk));

    const disposable = client.pushPermissionRequestHandler("session-1", async (params) => {
      assert.equal(params.sessionId, "session-1");
      assert.equal(params.toolCall.toolCallId, "call-1");
      return { outcome: { outcome: "selected", optionId: "allow" } };
    });
    try {
      writeFramed(inbound, {
        jsonrpc: "2.0",
        id: 99,
        method: "session/request_permission",
        params: {
          sessionId: "session-1",
          toolCall: {
            toolCallId: "call-1",
            title: "local_shell.local_shell: python3",
            kind: "execute",
            status: "pending",
            rawInput: { command: "python3" },
          },
          options: [
            { optionId: "allow", name: "Allow", kind: "allow_once" },
            { optionId: "deny", name: "Deny", kind: "reject_once" },
          ],
        },
      });

      await eventually(() => responses.length === 1);
      assert.deepEqual(responses[0], {
        jsonrpc: "2.0",
        id: 99,
        result: { outcome: { outcome: "selected", optionId: "allow" } },
      });
    } finally {
      disposable.dispose();
      client.dispose();
      telemetry.dispose();
      output.dispose();
    }
  });

  test("bridge routes permission requests by session", async () => {
    const outbound = new PassThrough();
    const inbound = new PassThrough();
    const output = new ContenoxOutput();
    const telemetry = new TelemetryLogger("test");
    const client = new BridgeClient(outbound, inbound, output, 1000, false, telemetry);
    const responses: unknown[] = [];
    const responseFramer = new JsonRpcFramer(new PassThrough(), (message) => responses.push(message), (error) => {
      throw error;
    });
    outbound.on("data", (chunk: Buffer) => responseFramer.accept(chunk));

    const calls: string[] = [];
    const disposableA = client.pushPermissionRequestHandler("session-a", async () => {
      calls.push("a");
      return { outcome: { outcome: "selected", optionId: "deny" } };
    });
    const disposableB = client.pushPermissionRequestHandler("session-b", async () => {
      calls.push("b");
      return { outcome: { outcome: "selected", optionId: "allow" } };
    });
    try {
      writeFramed(inbound, permissionRequest(100, "session-a", "call-a"));
      await eventually(() => responses.length === 1);
      assert.deepEqual(calls, ["a"]);
      assert.deepEqual(responses[0], {
        jsonrpc: "2.0",
        id: 100,
        result: { outcome: { outcome: "selected", optionId: "deny" } },
      });
    } finally {
      disposableA.dispose();
      disposableB.dispose();
      client.dispose();
      telemetry.dispose();
      output.dispose();
    }
  });

  test("bridge cancels in-flight permission handlers from server cancel notification", async () => {
    const outbound = new PassThrough();
    const inbound = new PassThrough();
    const output = new ContenoxOutput();
    const telemetry = new TelemetryLogger("test");
    const client = new BridgeClient(outbound, inbound, output, 1000, false, telemetry);
    const responses: unknown[] = [];
    const responseFramer = new JsonRpcFramer(new PassThrough(), (message) => responses.push(message), (error) => {
      throw error;
    });
    outbound.on("data", (chunk: Buffer) => responseFramer.accept(chunk));

    let cancelled = false;
    const disposable = client.pushPermissionRequestHandler(
      "session-cancel",
      (_params, token) =>
        new Promise((resolve) => {
          token.onCancellationRequested(() => {
            cancelled = true;
            resolve({ outcome: { outcome: "cancelled" } });
          });
        }),
    );
    try {
      writeFramed(inbound, permissionRequest(101, "session-cancel", "call-cancel"));
      writeFramed(inbound, {
        jsonrpc: "2.0",
        method: "$/cancelRequest",
        params: { id: 101 },
      });
      await eventually(() => cancelled && responses.length === 1);
      assert.deepEqual(responses[0], {
        jsonrpc: "2.0",
        id: 101,
        result: { outcome: { outcome: "cancelled" } },
      });
    } finally {
      disposable.dispose();
      client.dispose();
      telemetry.dispose();
      output.dispose();
    }
  });

  test("permission request conversion preserves meaningful approval-card details", () => {
    const params: RequestPermissionParams = {
      sessionId: "session-1",
      toolCall: {
        toolCallId: "call-1",
        title: "local_fs.sed: Questions: hello@contenox.com in README.md",
        kind: "edit",
        status: "pending",
        rawInput: {
          path: "README.md",
          pattern: "Questions: hello@contenox.com",
          replacement: "Questions: **hello@contenox.com**",
        },
        content: [
          {
            type: "diff",
            path: "README.md",
            oldText: "Questions: hello@contenox.com\n",
            newText: "Questions: **hello@contenox.com**\n",
          },
        ],
        _meta: {
          toolsName: "local_fs",
          toolName: "sed",
          policyName: "hitl-policy-strict.json",
          policyPath: "/home/user/.contenox/hitl-policy-strict.json",
          diff: "--- README.md\n+++ README.md\n@@\n-Questions: hello@contenox.com\n+Questions: **hello@contenox.com**\n",
        },
      },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "deny", name: "Deny", kind: "reject_once" },
      ],
    };

    const event = approvalEventFromPermissionRequest(params);

    assert.deepEqual(event.args, {
      path: "README.md",
      pattern: "Questions: hello@contenox.com",
      replacement: "Questions: **hello@contenox.com**",
    });
    assert.equal(event.toolsName, "local_fs");
    assert.equal(event.toolName, "sed");
    assert.equal(event.policyName, "hitl-policy-strict.json");
    assert.equal(event.policyPath, "/home/user/.contenox/hitl-policy-strict.json");
    assert.equal(event.diffOld, "Questions: hello@contenox.com\n");
    assert.equal(event.diffNew, "Questions: **hello@contenox.com**\n");
    assert.ok(event.diff?.includes("+Questions: **hello@contenox.com**"));
  });

  test("permission request conversion drops empty diff placeholders", () => {
    const event = approvalEventFromPermissionRequest({
      sessionId: "session-1",
      toolCall: {
        toolCallId: "call-blank",
        title: "local_fs.sed: README.md",
        rawInput: "{\"path\":\"README.md\",\"pattern\":\"x\",\"replacement\":\"y\"}",
        content: [{ type: "diff", path: "README.md", oldText: "", newText: "" }],
        _meta: {
          toolsName: "local_fs",
          toolName: "sed",
          diff: "",
          diffOld: "",
          diffNew: "",
        },
      },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "deny", name: "Deny", kind: "reject_once" },
      ],
    });

    assert.deepEqual(event.args, { path: "README.md", pattern: "x", replacement: "y" });
    assert.equal(event.diff, undefined);
    assert.equal(event.diffOld, undefined);
    assert.equal(event.diffNew, undefined);
  });

  test("permission request conversion preserves ACP content as fallback details", () => {
    const event = approvalEventFromPermissionRequest({
      sessionId: "session-1",
      toolCall: {
        toolCallId: "call-content",
        title: "custom.tool",
        content: [
          {
            type: "content",
            path: "README.md",
            content: {
              type: "text",
              text: "This action will update README metadata.",
            },
          },
        ],
      },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "deny", name: "Deny", kind: "reject_once" },
      ],
    });

    assert.equal(event.details, "README.md\nThis action will update README metadata.");
  });
});

interface ExtensionManifest {
  extensionKind?: string[];
  contributes: {
    commands: Array<{ command: string; title: string }>;
    languageModelTools?: Array<{
      name: string;
      displayName: string;
      inputSchema: {
        required: string[];
      };
    }>;
    viewsWelcome?: Array<{ view: string; contents: string }>;
    views?: {
      contenox?: Array<{ id: string; name: string }>;
    };
    walkthroughs?: Array<{
      id: string;
      steps: Array<{
        id: string;
        media?: {
          markdown?: string;
        };
      }>;
    }>;
  };
}

function readManifest(): ExtensionManifest {
  return JSON.parse(fs.readFileSync(path.join(extensionRoot(), "package.json"), "utf8")) as ExtensionManifest;
}

function extensionRoot(): string {
  return path.resolve(__dirname, "..", "..");
}

function isString(value: unknown): value is string {
  return typeof value === "string";
}

function permissionRequest(id: number, sessionId: string, toolCallId: string): unknown {
  return {
    jsonrpc: "2.0",
    id,
    method: "session/request_permission",
    params: {
      sessionId,
      toolCall: {
        toolCallId,
        title: `local_shell.local_shell: ${toolCallId}`,
        kind: "execute",
        status: "pending",
        rawInput: { command: toolCallId },
      },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "deny", name: "Deny", kind: "reject_once" },
      ],
    },
  };
}

function writeFramed(stream: PassThrough, message: unknown): void {
  const payload = Buffer.from(JSON.stringify(message), "utf8");
  stream.write(Buffer.concat([Buffer.from(`Content-Length: ${payload.length}\r\n\r\n`, "ascii"), payload]));
}

async function eventually(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1000;
  while (Date.now() < deadline) {
    if (predicate()) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.ok(predicate(), "condition was not met before timeout");
}
