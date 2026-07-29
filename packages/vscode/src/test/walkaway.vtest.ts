// Phase 5 "walk-away" acceptance test (vscode-implementation-plan.md §5,
// "THE acceptance test"):
//   1. Trip a gated tool call (reuses visual.vtest.ts's A4 write_file gate).
//   2. Let it park past the fast window (localtools.ApprovalParkWindow, 30s)
//      -- the run checkpoints and suspends server-side.
//   3. Close VS Code entirely.
//   4. Answer from the CLI: `contenox approvals respond <id> --approve`.
//   5. Reopen VS Code.
//   6. Assert the run completed and the result is visible in the transcript.
//
// A single `vscode-test` process IS one VS Code window for its whole run, so
// steps 3-5 cannot happen inside one `task vscode:visual` invocation -- that
// command only ever proves the in-panel half (suspension renders, the
// resume affordance works). This file is the other half: two mocha tests,
// "park" and "verify", each running in ITS OWN `vscode-test` process launch,
// selected by CONTENOX_WALKAWAY_PHASE so a bare `task vscode:visual` run
// (no env var set) skips both and stays a single, fast, unattended pass.
// Driving the real two-process run (with a real `contenox approvals respond`
// shelled out between them) is the caller's job -- see
// scripts/run-walkaway-acceptance.sh, which does exactly steps 1-6 for real.
import * as assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import type { Browser, FrameLocator, Page } from "playwright-core";
import * as vscode from "vscode";
import type { ContenoxTestApi } from "../extension";

const phase = process.env.CONTENOX_WALKAWAY_PHASE; // "park" | "verify" | undefined
const userDataDir =
  process.env.CONTENOX_VISUAL_USER_DATA ??
  path.resolve(__dirname, "..", "..", ".vscode-test", "visual-user-data");
const outDir = path.resolve(__dirname, "..", "..", "artifacts", "screenshots");
const stateFile = path.resolve(__dirname, "..", "..", ".vscode-test", "walkaway-state.json");
const paintMs = Number(process.env.CONTENOX_VISUAL_PAINT_MS ?? 3000);

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// Duplicated from visual.vtest.ts rather than shared: that file's helpers are
// module-private, and this file needs its own attach() lifecycle anyway
// (park and verify are separate process launches, each attaching fresh).
async function cdpPort(): Promise<string> {
  const file = path.join(userDataDir, "DevToolsActivePort");
  for (let i = 0; i < 45; i++) {
    try {
      const first = fs.readFileSync(file, "utf8").split("\n")[0].trim();
      if (first) return first;
    } catch {
      // not written yet
    }
    await sleep(1000);
  }
  throw new Error(`no DevToolsActivePort in ${userDataDir} — devtools server never started`);
}

async function attach(): Promise<{ browser: Browser; page: Page }> {
  const { chromium } = require("playwright-core") as typeof import("playwright-core");
  const port = await cdpPort();
  let browser: Browser | undefined;
  for (let i = 0; i < 20 && !browser; i++) {
    try {
      browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`, { timeout: 5000 });
    } catch {
      await sleep(1000);
    }
  }
  if (!browser) throw new Error(`CDP endpoint on :${port} never accepted a connection`);
  const ctx = browser.contexts()[0];
  let page: Page | undefined;
  for (let i = 0; i < 30 && !page; i++) {
    page = ctx.pages().find((p) => p.url().includes("workbench"));
    if (!page) await sleep(1000);
  }
  if (!page) throw new Error("workbench page not found over CDP");
  return { browser, page };
}

async function chatFrame(page: Page): Promise<FrameLocator> {
  page.setDefaultTimeout(3000);
  try {
    for (let attempt = 0; attempt < 10; attempt++) {
      const count = await page.locator("iframe.webview").count();
      for (let i = 0; i < count; i++) {
        const inner = page.frameLocator("iframe.webview").nth(i).frameLocator("#active-frame");
        try {
          if (await inner.locator("textarea").first().isVisible()) return inner;
        } catch {
          // not this webview, or not painted yet
        }
      }
      await sleep(1000);
    }
    throw new Error("chat webview frame not found");
  } finally {
    page.setDefaultTimeout(30_000);
  }
}

async function shoot(page: Page, name: string): Promise<void> {
  fs.mkdirSync(outDir, { recursive: true });
  const file = path.join(outDir, `${name}.png`);
  await page.screenshot({ path: file });
  console.log(`captured ${file}`);
}

interface WalkawayState {
  sessionId: string;
  approvalId: string;
}

suite("Contenox walk-away (two-process acceptance test)", function () {
  test("park: trip a gated call and let it suspend", async function () {
    if (phase !== "park") {
      this.skip();
      return;
    }
    this.timeout(300_000);

    const extension = vscode.extensions.getExtension("contenox.contenox-runtime");
    assert.ok(extension, "extension missing from the Extension Development Host");
    await extension!.activate();

    const { browser, page } = await attach();
    try {
      await vscode.commands.executeCommand("workbench.view.extension.contenox");
      await vscode.commands.executeCommand("contenox.openChat");
      await sleep(paintMs);

      const chat = await chatFrame(page);
      const composer = chat.locator("textarea").first();
      assert.ok(await composer.isVisible(), "composer should be visible");

      const newSessionButton = chat.getByRole("button", { name: /New .* session/i });
      if (await newSessionButton.isVisible().catch(() => false)) {
        await newSessionButton.click();
        await sleep(500);
      }

      // Same real gated call visual.vtest.ts's A4 uses: hitl-policy-acp.json
      // gates local_fs.write_file with action "approve" unconditionally.
      await composer.fill(
        "Use your write_file tool right now to create a file named artifacts/contenox-walkaway-test.txt " +
          "(the artifacts directory already exists) containing exactly: contenox walkaway test. " +
          "Call the tool immediately, do not ask first.",
      );
      await composer.press("Enter");

      const allowButton = chat.getByRole("button", { name: "Allow" });
      await allowButton.waitFor({ state: "visible", timeout: 90_000 });
      console.log("approval card visible; deliberately NOT answering it — waiting past the fast park window");
      await shoot(page, "20-walkaway-pending");

      const sessionId = await chat.locator("#beam-embedded-session").inputValue();
      assert.ok(sessionId, "the active session id should be readable from the composer's session select");
      console.log(`walk-away session id: ${sessionId}`);

      // localtools.ApprovalParkWindow is 30s; wait comfortably past it so the
      // run genuinely checkpoints and suspends server-side (StopSuspended),
      // not just "still pending inside the fast window".
      await sleep(40_000);

      // The submit button unsticking (turn ended: end_turn on the wire) while
      // the approval card is STILL showing is exactly "suspended" -- and the
      // card must now say so, per ChatWebviewViewProvider.handleSendMessage /
      // ChatSurface.tsx's suspended rendering (Phase 5).
      const sendButton = chat.locator('button[type="submit"]');
      const stillSending = /sending/i.test(await sendButton.innerText().catch(() => ""));
      assert.ok(!stillSending, "the turn should have ended (suspended, not still in flight) after the park window");

      const cardText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
      console.log(`card text after park window: ${cardText.slice(0, 600)}`);
      assert.ok(/parked/i.test(cardText), 'expected the approval card to render "Parked" once suspended');
      assert.ok(/safe to close/i.test(cardText), "expected the suspended banner to say it is safe to close VS Code");
      // The original card's "Allow" button is gone -- the agent itself
      // cancelled that RPC once the park window elapsed (see
      // ChatWebviewViewProvider.handleSendMessage's approvalAutoCancelled
      // comment); it's been replaced by the durable-ask-backed
      // ParkedApprovalCard's Approve/Deny, deliberately left unanswered here
      // so the CLI can answer it instead (steps 3-4 of the acceptance test).
      assert.ok(
        !(await allowButton.isVisible().catch(() => false)),
        "the original Allow button should be gone once the run suspended -- its RPC is dead",
      );
      assert.ok(
        await chat.getByRole("button", { name: "Approve" }).isVisible(),
        "the parked card should offer an Approve button backed by the durable ask store",
      );
      await shoot(page, "21-walkaway-suspended");

      // Read the approval id off the card itself (rendered in the "safe to
      // close" copy as `contenox approvals respond <id> ...`) rather than
      // re-deriving it, so this test asserts on exactly what a human would
      // read off the screen.
      const idMatch = cardText.match(/approvals respond ([a-zA-Z0-9-]+)/);
      assert.ok(idMatch, "expected the suspended banner to name the approvals-respond id");
      const approvalId = idMatch![1];
      console.log(`approval id read off the card: ${approvalId}`);

      fs.mkdirSync(path.dirname(stateFile), { recursive: true });
      fs.writeFileSync(stateFile, JSON.stringify({ sessionId, approvalId } satisfies WalkawayState), "utf8");
      console.log(`wrote walk-away state to ${stateFile}`);
      console.log(
        "PARK PHASE DONE. VS Code stays open only because this process hasn't exited yet -- the orchestrating " +
          "script now kills this process entirely (`close VS Code entirely`) before answering from the CLI.",
      );
    } finally {
      await browser.close().catch(() => undefined);
    }
  });

  test("verify: reopen and observe the completed run", async function () {
    if (phase !== "verify") {
      this.skip();
      return;
    }
    this.timeout(300_000);

    assert.ok(fs.existsSync(stateFile), `expected ${stateFile} from the park phase — run it first`);
    const state = JSON.parse(fs.readFileSync(stateFile, "utf8")) as WalkawayState;
    console.log(`verify phase: reopening session ${state.sessionId} (approval ${state.approvalId})`);

    const extension = vscode.extensions.getExtension("contenox.contenox-runtime");
    assert.ok(extension, "extension missing from the Extension Development Host");
    const api = (await extension!.activate()) as ContenoxTestApi;

    const { browser, page } = await attach();
    try {
      await vscode.commands.executeCommand("workbench.view.extension.contenox");
      // The real user path: click the session in History, exactly like
      // SessionTreeProvider's item command -- not a lower-level API call.
      await vscode.commands.executeCommand("contenox.openSession", state.sessionId);
      await sleep(paintMs);

      const chat = await chatFrame(page);
      await shoot(page, "22-walkaway-reopened");

      // Phase 5 "catch up on reopen": this is a cold getSession (fresh
      // extension host, never saw the session park) -- the answer given from
      // the CLI while VS Code was closed the whole time must show up here
      // without any further click.
      const deadline = Date.now() + 60_000;
      let bodyText = "";
      while (Date.now() < deadline) {
        bodyText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
        if (
          /contenox walkaway test|walkaway test/i.test(bodyText) &&
          !/checkpointed and is safe to close/i.test(bodyText)
        )
          break;
        await sleep(2000);
      }
      console.log(`reopened transcript text: ${bodyText.slice(0, 800)}`);
      assert.ok(
        !/checkpointed and is safe to close/i.test(bodyText),
        `expected the parked banner to be gone once the approval resolved out of band, got: ${bodyText.slice(0, 400)}`,
      );

      const writtenPath = path.resolve(
        __dirname,
        "..",
        "..",
        "test",
        "fixtures",
        "smoke-workspace",
        "artifacts",
        "contenox-walkaway-test.txt",
      );
      const written = fs.existsSync(writtenPath) ? fs.readFileSync(writtenPath, "utf8") : undefined;
      console.log(`file the resumed run should have written: ${written ?? "<missing>"}`);
      assert.ok(written?.includes("contenox walkaway test"), "the resumed run should have actually executed write_file");

      await shoot(page, "23-walkaway-resumed-complete");
    } finally {
      try {
        await api.deleteSessionForTest(state.sessionId);
        console.log(`cleanup: deleted session ${state.sessionId}`);
      } catch (error) {
        console.warn(`cleanup: failed to delete session ${state.sessionId}: ${error}`);
      }
      try {
        fs.rmSync(
          path.resolve(__dirname, "..", "..", "test", "fixtures", "smoke-workspace", "artifacts", "contenox-walkaway-test.txt"),
          { force: true },
        );
      } catch {
        // best-effort
      }
      fs.rmSync(stateFile, { force: true });
      await browser.close().catch(() => undefined);
    }
  });
});
