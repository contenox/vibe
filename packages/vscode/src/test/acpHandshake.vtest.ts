// Proves @agentclientprotocol/sdk is usable at runtime from this extension
// host, against a real `contenox acp` process -- not just "it compiles".
// Exercises: dynamic-import ESM interop (../acp/sdk), Readable.toWeb() /
// Writable.toWeb() bridging a Node child_process's stdio into the SDK's
// Web-Streams-based ndJsonStream (acp-client-ts-spike.md Unknown #4), and
// the real ACP client role (initialize -> session/new -> clean shutdown)
// against the actual wire libacp/acpsvc implements.
//
// Needs a real child process and the repo-root `bin/contenox` built by
// `task build`, so this lives in the visual/e2e suite (*.vtest.ts), run only
// under .vscode-test.visual.js -- never under the fast, hermetic `npm test`.
import * as assert from "node:assert/strict";
import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { Readable, Writable } from "node:stream";
import { loadAcpSdk } from "../acp/sdk";

// dist/test/acpHandshake.vtest.js -> up to packages/vscode/ -> up to repo root.
const repoRoot = path.resolve(__dirname, "..", "..", "..", "..");
const contenoxBin = path.join(repoRoot, "bin", "contenox");

suite("Contenox ACP SDK handshake", function () {
  test("SDK client completes initialize + session/new against a real contenox acp process", async function () {
    this.timeout(30_000);

    assert.ok(
      fs.existsSync(contenoxBin),
      `${contenoxBin} missing -- run "task build" from the repo root first`,
    );

    const acp = await loadAcpSdk();

    const child = spawn(contenoxBin, ["acp"], {
      stdio: ["pipe", "pipe", "pipe"],
      env: { ...process.env, NO_COLOR: "1" },
    });
    const stderrChunks: Buffer[] = [];
    child.stderr.on("data", (chunk: Buffer) => stderrChunks.push(chunk));
    const exited = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) => {
      child.on("exit", (code, signal) => resolve({ code, signal }));
    });

    try {
      // Web-Streams bridge: the SDK's ndJsonStream reads/writes Web Streams,
      // not Node streams -- this is exactly what Unknown #4 flagged as
      // untested outside a throwaway script.
      const input = Writable.toWeb(child.stdin);
      const output = Readable.toWeb(child.stdout);
      const wireStream = acp.ndJsonStream(input, output);

      const result = await acp
        .client({ name: "contenox-vscode-test" })
        .onRequest(acp.methods.client.session.requestPermission, async () => ({
          outcome: { outcome: "cancelled" as const },
        }))
        .connectWith(wireStream, async (ctx) => {
          const initResult = await ctx.request(acp.methods.agent.initialize, {
            protocolVersion: acp.PROTOCOL_VERSION,
            clientCapabilities: {
              fs: { readTextFile: false, writeTextFile: false },
              terminal: false,
            },
            clientInfo: { name: "contenox-vscode-test", version: "0.0.0" },
          });
          assert.equal(initResult.protocolVersion, acp.PROTOCOL_VERSION);
          assert.equal(initResult.agentInfo?.name, "contenox");

          const session = await ctx.buildSession(repoRoot).start();
          try {
            assert.ok(session.sessionId, "session/new should return a sessionId");
            await ctx.request(acp.methods.agent.session.delete, {
              sessionId: session.sessionId,
            });
          } finally {
            session.dispose();
          }

          return { protocolVersion: initResult.protocolVersion, sessionId: session.sessionId };
        });

      assert.equal(result.protocolVersion, acp.PROTOCOL_VERSION);
      assert.ok(result.sessionId);
    } finally {
      child.stdin.end();
      child.kill("SIGTERM");
      const { code, signal } = await Promise.race([
        exited,
        new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) =>
          setTimeout(() => resolve({ code: null, signal: null }), 5000),
        ),
      ]);
      if (code !== 0 && code !== null) {
        console.log(`contenox acp stderr:\n${Buffer.concat(stderrChunks).toString("utf8")}`);
      }
      void signal;
    }
  });
});
