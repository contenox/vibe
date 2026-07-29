// Visual harness: drives the real Extension Development Host and inspects the
// chat webview's DOM, so a panel change can be confirmed by looking at it and
// asserted on, not just inferred from extension-host state.
//
// It runs inside the extension host, so it drives VS Code through the real
// command API (reliable), and attaches Playwright to its own window over CDP
// (observation). Driving the command palette by keystroke was tried and is
// flaky: the palette ranks built-in commands like "Chat: Open Chat" above ours.
//
// Runs only under .vscode-test.visual.js (glob *.vtest.js, its own
// user-data-dir), never under `npm test`: VS Code refuses concurrent test
// instances sharing a user-data-dir. Needs an X display. Run: task vscode:visual
// Output: packages/vscode/artifacts/screenshots/<name>.png (gitignored).
import * as assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import type { Browser, FrameLocator, Page } from "playwright-core";
import * as vscode from "vscode";
import type { ContenoxTestApi } from "../extension";

const userDataDir =
  process.env.CONTENOX_VISUAL_USER_DATA ??
  path.resolve(__dirname, "..", "..", ".vscode-test", "visual-user-data");
const outDir = path.resolve(__dirname, "..", "..", "artifacts", "screenshots");
const paintMs = Number(process.env.CONTENOX_VISUAL_PAINT_MS ?? 3000);

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// Chromium writes the port it actually bound into DevToolsActivePort (first
// line) once the devtools server is up. Reading it beats guessing a port.
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

// The panel lives in a nested webview iframe. VS Code names the inner content
// frame fake.html; the outer index.html is only a shell. There are at least
// two webviews (chat, runtime controls) with no view id in their DOM — name,
// id and src are all per-instance random. So pick by content, not index: scan
// every iframe.webview and return whichever one's #active-frame exposes a
// composer textarea.
async function chatFrame(page: Page): Promise<FrameLocator> {
  // Short per-probe timeout: the default 30s applied to a detached or
  // still-loading webview is what makes a naive scan hang past the test budget.
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
    const count = await page.locator("iframe.webview").count();
    throw new Error(
      `chat webview frame not found: ${count} iframe.webview element(s), none exposed ` +
        `an #active-frame with a composer textarea. If count is 0, the panel never opened; ` +
        `if the outer iframe exists but its content is unreachable, Playwright likely attached ` +
        `after the webview's out-of-process frame was created (see attach() — it must run first).`,
    );
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

// Same content-not-index scan as chatFrame(), but for the Runtime webview
// (contenox.controls): identified by its "model" <select>, which only that
// panel renders (vscode-implementation-plan.md Phase 1 "runtime controls over
// ACP").
async function runtimeFrame(page: Page): Promise<FrameLocator> {
  page.setDefaultTimeout(3000);
  try {
    for (let attempt = 0; attempt < 10; attempt++) {
      const count = await page.locator("iframe.webview").count();
      for (let i = 0; i < count; i++) {
        const inner = page.frameLocator("iframe.webview").nth(i).frameLocator("#active-frame");
        try {
          if (await inner.locator("select#model").first().isVisible()) return inner;
        } catch {
          // not this webview, or not painted yet
        }
      }
      await sleep(1000);
    }
    throw new Error("runtime controls webview frame not found (no iframe.webview exposed a #model select)");
  } finally {
    page.setDefaultTimeout(30_000);
  }
}

suite("Contenox visual surfaces", function () {
  test("chat panel renders its composer, session controls and runtime summary", async function () {
    // Several real model turns now (Phase 1 exchange + Phase 2 usage/cancel/
    // approval scenarios + Phase 3 attachment send + Phase 4 code-block
    // turn), each with its own ~150s settle budget -- a single local model
    // call can be slow, so the whole-test budget must comfortably exceed the
    // sum, not just one turn's.
    this.timeout(900_000);

    const extension = vscode.extensions.getExtension("contenox.contenox-runtime");
    assert.ok(extension, "extension missing from the Extension Development Host");
    const api = (await extension.activate()) as ContenoxTestApi;
    assert.ok(
      typeof api?.deleteSessionForTest === "function",
      "activate() should return the test API used to clean up sessions this suite creates",
    );

    // Every ACP session this run creates, tracked so teardown can delete it
    // via session/delete (AcpChatClient.deleteSession) -- this suite must not
    // leave junk sessions in the maintainer's real ~/.contenox session list
    // (vscode-implementation-plan.md Phase 2 "Task C"). Deletion happens in
    // the `finally` below so it runs even when an assertion fails.
    const createdSessionIds: string[] = [];

    // Attach BEFORE opening the panel. The webview content is an
    // out-of-process iframe (OOPIF); Chromium's Target.setAutoAttach only
    // reliably attaches to targets created after the CDP session subscribes.
    // Attaching after the webview already exists leaves Playwright's frame
    // stub permanently empty (its init promise rejects silently), so the
    // panel must not be opened until Playwright is already listening.
    const { browser, page } = await attach();
    try {
      // Real command API, not the palette.
      await vscode.commands.executeCommand("workbench.view.extension.contenox");
      await vscode.commands.executeCommand("contenox.openChat");
      await sleep(paintMs);

      await shoot(page, "01-chat-panel");

      const chat = await chatFrame(page);
      const composer = chat.locator("textarea").first();
      assert.ok(await composer.isVisible(), "composer should be visible");

      const text = (await chat.locator("body").innerText()).replace(/\s+/g, " ").trim();
      console.log(`panel text: ${text.slice(0, 400)}`);
      assert.ok(text.length > 0, "panel should render some text");

      // Typing is the cheapest proof the webview is live, not a painted corpse.
      await composer.fill("what files are in this workspace?");
      await sleep(500);
      await shoot(page, "02-composer-typed");
      assert.equal(await composer.inputValue(), "what files are in this workspace?");

      // Acceptance gate for the ACP port (vscode-implementation-plan.md Phase
      // 1): force a real session/new, then send a real session/prompt and
      // assert a non-empty assistant response streams into the transcript --
      // against a real `contenox acp` child process, not the bespoke bridge.
      // Reuses this test's already-attached CDP session (browser/page) rather
      // than reattaching: the webview's out-of-process iframe target only
      // attaches reliably to a CDP session that existed before the webview
      // was created (see attach()'s comment) -- a second, later attach would
      // not see it.
      const newSessionButton = chat.getByRole("button", { name: /New .* session/i });
      if (await newSessionButton.isVisible().catch(() => false)) {
        await newSessionButton.click();
        await sleep(500);
      }

      await composer.fill("Reply with the exact three words: hello from contenox");
      await composer.press("Enter");

      // Wait for the turn to finish: the submit button reads "Sending" while
      // pending and reverts once the ACP session/prompt round-trip settles.
      const sendButton = chat.locator('button[type="submit"]');
      const deadline = Date.now() + 150_000;
      let sending = true;
      while (Date.now() < deadline) {
        const label = await sendButton.innerText().catch(() => "");
        sending = /sending/i.test(label);
        if (!sending) break;
        await sleep(1000);
      }
      assert.ok(!sending, "chat turn did not finish (submit button stuck on Sending)");

      // Assistant messages render as <article aria-label="Contenox">
      // (ChatMessage's transcript appearance uses roleLabel as the label).
      const assistantMessages = chat.locator('article[aria-label="Contenox"]');
      await assistantMessages.first().waitFor({ state: "visible", timeout: 10_000 });
      const assistantCount = await assistantMessages.count();
      assert.ok(assistantCount > 0, "expected at least one assistant message in the transcript");
      const assistantText = (await assistantMessages.last().innerText()).trim();
      console.log(`assistant response: ${assistantText.slice(0, 400)}`);
      assert.ok(assistantText.length > 0, "assistant response should be non-empty");

      await shoot(page, "03-chat-response");

      // Sessions over ACP (vscode-implementation-plan.md Phase 1 "sessions
      // over ACP"): the session just chatted in must be durable -- listed via
      // a real session/list call (SessionTreeProvider, not the bespoke
      // bridge) and loadable via a real session/load call
      // (contenox.openSessionTranscript -> AcpChatClient.loadSession),
      // neither of which reads the webview provider's own in-memory cache.
      const sessionId = await chat.locator("#beam-embedded-session").inputValue();
      assert.ok(sessionId, "the active session id should be readable from the composer's session select");
      console.log(`active session id: ${sessionId}`);
      createdSessionIds.push(sessionId);

      // ---------------------------------------------------------------
      // Phase 2 (vscode-implementation-plan.md §5): the four features
      // already on the ACP wire, rendered. Same session as above -- one
      // real session for the whole suite keeps the cleanup surface small.
      // ---------------------------------------------------------------

      // A1: context/token usage. usage_update carries a real used/size pair
      // (acpsvc/transport.go's sendInitialUsageUpdate + sendUsageUpdate);
      // the chip must show a real number, not the old permanently-static
      // "0/4.096 (0%)" sourced from a max-tokens guess.
      const usageChip = chat.locator('span[title^="Context:"]');
      await usageChip.first().waitFor({ state: "visible", timeout: 20_000 });
      const usageLabel = (await usageChip.first().innerText()).trim();
      const usageTitle = (await usageChip.first().getAttribute("title")) ?? "";
      console.log(`usage chip: "${usageLabel}" (title="${usageTitle}")`);
      assert.ok(/\d/.test(usageLabel), `usage chip should render a real token count, got "${usageLabel}"`);
      await shoot(page, "07-usage-chip");

      // A2: slash commands. available_commands_update advertises the
      // agent's real command set; typing "/" must open an autocomplete menu
      // built from it (not a hardcoded list).
      await composer.fill("/");
      await sleep(500);
      const slashMenu = chat.getByRole("listbox", { name: "Slash commands" });
      await slashMenu.waitFor({ state: "visible", timeout: 10_000 });
      // Scoped to the listbox: an unscoped getByRole("option") also matches
      // the (hidden) native <option> elements of the session <select>, which
      // still "passes" a bare length check for the wrong reason.
      const slashOptionTexts = await slashMenu.getByRole("option").allInnerTexts();
      console.log(`slash commands offered: ${slashOptionTexts.join(" | ").slice(0, 400)}`);
      assert.ok(slashOptionTexts.length > 0, "the slash menu should list at least one agent-advertised command");
      assert.ok(
        slashOptionTexts.some((text) => /\/(help|doctor|clear|compact|model|policy|think)\b/i.test(text)),
        `expected a real acpsvc-advertised command (e.g. /help) among: ${slashOptionTexts.join(" | ")}`,
      );
      await shoot(page, "08-slash-commands");
      await composer.fill("");
      await sleep(300);

      // A3: turn states. Start a long-ish turn, cancel it mid-flight, and
      // confirm the transcript renders a neutral "Cancelled" state rather
      // than the top-level error banner a cancelled turn used to produce.
      await composer.fill("Count slowly from one to fifty, one number per line, with a short aside after each.");
      await composer.press("Enter");
      await sleep(2000);
      const cancelButton = chat.getByRole("button", { name: "Cancel" });
      const cancelled = await cancelButton.isVisible().catch(() => false);
      if (cancelled) {
        await cancelButton.click();
      } else {
        console.log("A3: turn finished before Cancel became clickable; asserting on whatever stop reason it settled with instead");
      }
      {
        const deadline2 = Date.now() + 150_000;
        let stillSending = true;
        while (Date.now() < deadline2) {
          const label = await sendButton.innerText().catch(() => "");
          stillSending = /sending/i.test(label);
          if (!stillSending) break;
          await sleep(1000);
        }
        assert.ok(!stillSending, "the cancelled/completed turn should still settle (submit button unstuck)");
      }
      await shoot(page, "09-turn-cancelled");
      if (cancelled) {
        // The badge (Badge component) is the only place "Cancelled" should
        // render; the old behavior additionally threw a top-level error
        // banner reading "Cancelled", which would make this count >= 2.
        const cancelledOccurrences = await chat.getByText("Cancelled", { exact: true }).count();
        console.log(`"Cancelled" text occurrences after cancel: ${cancelledOccurrences}`);
        assert.ok(cancelledOccurrences > 0, "expected a Cancelled badge in the transcript after cancelling a turn");
        assert.ok(
          cancelledOccurrences === 1,
          `a cancelled turn must not also render a top-level error banner (found ${cancelledOccurrences} "Cancelled" texts)`,
        );
      }

      // A4: why it was gated. hitl-policy-acp.json (the real policy
      // `contenox acp` runs under) gates local_fs.write_file with
      // action "approve" unconditionally, so asking the agent to write a
      // file trips a real approval card. Render the reason, not just the
      // tool name.
      await composer.fill(
        "Use your write_file tool right now to create a file named artifacts/contenox-visual-test.txt " +
          "(the artifacts directory already exists) containing exactly: contenox visual test. " +
          "Call the tool immediately, do not ask first.",
      );
      await composer.press("Enter");
      const allowButton = chat.getByRole("button", { name: "Allow" });
      await allowButton.waitFor({ state: "visible", timeout: 90_000 });
      const approvalText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
      console.log(`approval card text: ${approvalText.slice(0, 500)}`);
      assert.ok(/write_file/i.test(approvalText), "the approval card should name the gated tool (local_fs.write_file)");
      assert.ok(
        /requires (human )?approval|policy/i.test(approvalText),
        "the approval card should explain why the call was gated, not just show the tool name",
      );
      // "Why it was gated" (gateReason, ChatSurface.tsx): the acpsvc/vscodeagent
      // transport now forwards policyName/matchedRule end to end, so the card
      // must name the actual policy and say which of its rules fired --
      // "rule N" 1-based (never "rule 0") or "no rule matched" when the
      // policy's own default action applied, the same vocabulary as the beam
      // TUI card (comp/approval.policyText).
      assert.match(
        approvalText,
        /Asking because policy "[^"]+" \((rule \d+|no rule matched)\) requires approval/i,
        "the approval card should name the policy and say which rule matched (or that no rule matched)",
      );
      assert.doesNotMatch(approvalText, /\(rule 0\)/, "a matched rule must render 1-based, never as rule 0");
      await shoot(page, "10-approval-gate");
      await allowButton.click();
      {
        const deadline3 = Date.now() + 150_000;
        let stillSending = true;
        while (Date.now() < deadline3) {
          const label = await sendButton.innerText().catch(() => "");
          stillSending = /sending/i.test(label);
          if (!stillSending) break;
          await sleep(1000);
        }
        assert.ok(!stillSending, "the approved turn should finish after Allow is clicked");
      }
      await shoot(page, "11-approval-resolved");

      // A4b: why it was gated, cause not index. hitl-policy-acp.json's tier 4
      // ("commands that delete, escalate or overwrite always ask") matches
      // `rm` via command_ask_always, and the structural shell reading that
      // sets EvaluationResult.Detail names the actual command caught, not
      // just the rule's ordinal -- "rule 41" tells a human almost nothing
      // next to `shell command "rm" matched command_ask_always`. This is
      // the acceptance case for closing hitlservice.EvaluationResult.Detail
      // -> ApprovalRequest -> approvalflow.Meta -> the wire end to end.
      await composer.fill(
        "Use your shell tool right now to run: rm /tmp/contenox-visual-shell-scratch.txt " +
          "(it is fine if that file does not exist). Call the tool immediately, do not ask first.",
      );
      await composer.press("Enter");
      const shellAllowButton = chat.getByRole("button", { name: "Allow" });
      await shellAllowButton.waitFor({ state: "visible", timeout: 90_000 });
      // The card is at the end of a now-longer transcript; scroll it into
      // view so the screenshot actually shows the line under test, not just
      // whatever the panel happened to be scrolled to.
      await shellAllowButton.scrollIntoViewIfNeeded();
      const shellApprovalText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
      const shellDetailAt = shellApprovalText.indexOf("Asking because");
      console.log(
        `shell approval card text: ${shellApprovalText.slice(shellDetailAt >= 0 ? shellDetailAt : 0, (shellDetailAt >= 0 ? shellDetailAt : 0) + 300)}`,
      );
      assert.ok(/\brm\b/i.test(shellApprovalText), "the approval card should name the gated shell command (rm)");
      // gateReason (ChatSurface.tsx) / policyText (comp/approval.go): the
      // matched rule's human-readable cause displaces the bare rule index
      // once one is available -- required here, not just the fallback.
      assert.match(
        shellApprovalText,
        /Asking because policy "[^"]+" \(shell command "rm" matched command_ask_always\) requires approval/i,
        "the approval card should lead with the matched rule's cause, not a bare rule index",
      );
      assert.doesNotMatch(
        shellApprovalText,
        /\(rule \d+\)/,
        "once a human-readable detail is available it must displace the rule index, not sit beside it",
      );
      await shoot(page, "10b-approval-detail-gate");
      const shellDenyButton = chat.getByRole("button", { name: "Deny" });
      await shellDenyButton.click();
      {
        const deadline2b = Date.now() + 60_000;
        let stillSending = true;
        while (Date.now() < deadline2b) {
          const label = await sendButton.innerText().catch(() => "");
          stillSending = /sending/i.test(label);
          if (!stillSending) break;
          await sleep(1000);
        }
        assert.ok(!stillSending, "the denied shell turn should still settle (submit button unstuck)");
      }

      // ---------------------------------------------------------------
      // Phase 5 (vscode-implementation-plan.md §5): walk-away, in-panel
      // half. The full acceptance test (close VS Code, answer from the CLI,
      // reopen) is a separate two-process run --
      // scripts/run-walkaway-acceptance.sh -- since one vscode-test process
      // is one VS Code window for its whole life and can't close/reopen
      // itself. This proves the part that CAN run inside this single
      // process: a gated call left unanswered past the fast window
      // (localtools.ApprovalParkWindow, 30s) must render as suspended, not
      // silently vanish, and the same card must still resume the run when
      // answered in-panel.
      // ---------------------------------------------------------------
      await composer.fill(
        "Use your write_file tool right now to create a file named artifacts/contenox-suspend-test.txt " +
          "(the artifacts directory already exists) containing exactly: contenox suspend test. " +
          "Call the tool immediately, do not ask first.",
      );
      await composer.press("Enter");
      const suspendAllowButton = chat.getByRole("button", { name: "Allow" });
      await suspendAllowButton.waitFor({ state: "visible", timeout: 90_000 });
      console.log("Phase 5: approval card up, waiting past the 30s park window without answering it");
      await sleep(40_000);

      // Once the park window elapses, the agent itself gives up on the RPC
      // (see ChatWebviewViewProvider.handleSendMessage's
      // approvalAutoCancelled comment) -- the original card (with its "Allow"
      // button) is gone, replaced by the durable-ask-backed ParkedApprovalCard.
      const suspendedStill = /sending/i.test(await sendButton.innerText().catch(() => ""));
      assert.ok(!suspendedStill, "the turn should have ended (suspended) once the park window elapsed");
      const suspendedText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
      console.log(`card text once suspended: ${suspendedText.slice(0, 500)}`);
      assert.ok(/parked/i.test(suspendedText), 'expected the card to render "Parked" once the run suspended');
      assert.ok(/safe to close/i.test(suspendedText), 'expected the suspended banner to say it is safe to close VS Code');
      const parkedApproveButton = chat.getByRole("button", { name: "Approve" });
      await parkedApproveButton.waitFor({ state: "visible", timeout: 10_000 });
      const parkedRejectButton = chat.getByRole("button", { name: "Deny" });
      assert.ok(await parkedRejectButton.isVisible(), "the parked card should offer Deny alongside Approve");
      await shoot(page, "19-suspended");

      // Phase 6 "operator inbox": while this approval is genuinely parked
      // (durable row), the inbox tree and its activity-bar badge must show
      // it -- this is where walk-away lands when no panel is open at all, so
      // it must be true even though a panel happens to be open here too.
      await vscode.commands.executeCommand("contenox.inbox.refresh");
      await sleep(2000);
      await vscode.commands.executeCommand("contenox.inbox.focus");
      await sleep(paintMs);
      await shoot(page, "24-inbox-badge");
      const sidebarInboxText = (await page.locator("body").innerText()).replace(/\s+/g, " ");
      console.log(`sidebar text with inbox visible: ${sidebarInboxText.slice(0, 600)}`);
      assert.ok(
        /pending approvals/i.test(sidebarInboxText),
        "the Contenox Inbox view should be present in the activity bar sidebar",
      );
      await vscode.commands.executeCommand("contenox.openChat");
      await sleep(paintMs);

      // The resume affordance: ParkedApprovalCard's Approve button shells
      // `contenox approvals respond --approve` (there is no live RPC left to
      // answer any other way) and re-checks the durable ask store, which
      // must resume the checkpointed run to completion.
      await parkedApproveButton.click();
      {
        const deadline5 = Date.now() + 150_000;
        let stillResolving = true;
        while (Date.now() < deadline5) {
          const text = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
          stillResolving = /checkpointed and is safe to close/i.test(text);
          if (!stillResolving) break;
          await sleep(2000);
        }
        assert.ok(!stillResolving, "resuming a suspended turn in-panel should clear the parked banner once it completes");
      }
      await sleep(1000);
      const resumedText = (await chat.locator("body").innerText()).replace(/\s+/g, " ");
      assert.ok(
        !/checkpointed and is safe to close/i.test(resumedText),
        "the parked banner must clear once the run resumes and completes",
      );
      console.log(`card text after in-panel resume: ${resumedText.slice(0, 500)}`);
      await shoot(page, "19b-resumed-in-panel");
      try {
        fs.rmSync(
          path.resolve(__dirname, "..", "..", "test", "fixtures", "smoke-workspace", "artifacts", "contenox-suspend-test.txt"),
          { force: true },
        );
      } catch {
        // best-effort
      }

      // ---------------------------------------------------------------
      // Phase 3 (vscode-implementation-plan.md §5): real context
      // attachment -- visible/removable chips, resource_link on the wire,
      // editor selection attached as a chip rather than pasted text.
      // ---------------------------------------------------------------

      // B1: @file picker -> chip appears -> removing it removes the chip ->
      // re-attach and send -> assert the actual outgoing content blocks
      // (not just the UI) include a resource_link for the attached file.
      await composer.fill("Please look at @READ");
      await sleep(700);
      const mentionMenu = chat.getByRole("listbox", { name: "Attach file or symbol" });
      await mentionMenu.waitFor({ state: "visible", timeout: 10_000 });
      const readmeOption = mentionMenu.getByRole("option").filter({ hasText: "README.md" }).first();
      await readmeOption.waitFor({ state: "visible", timeout: 10_000 });
      await readmeOption.click();
      await sleep(300);

      const attachmentList = chat.getByRole("list", { name: "Attached context" });
      await attachmentList.waitFor({ state: "visible", timeout: 10_000 });
      let chipTexts = await attachmentList.getByRole("listitem").allInnerTexts();
      console.log(`attachment chips after @file pick: ${chipTexts.join(" | ")}`);
      assert.ok(
        chipTexts.some((text) => text.includes("README.md")),
        "expected a README.md attachment chip after picking it via @",
      );
      const composerValueAfterMention = await composer.inputValue();
      assert.ok(
        !composerValueAfterMention.includes("@READ"),
        "the @mention token should be removed from the composer once an attachment is picked",
      );
      await shoot(page, "12-attachment-chip");

      // Removing it removes the chip.
      await chat.getByRole("button", { name: "Remove README.md" }).first().click();
      await sleep(300);
      chipTexts = await attachmentList
        .getByRole("listitem")
        .allInnerTexts()
        .catch(() => []);
      assert.ok(
        !chipTexts.some((text) => text.includes("README.md")),
        "removing the chip should remove it from the attachment list",
      );
      await shoot(page, "13-attachment-removed");

      // Re-attach and actually send it -- assert the outgoing wire shape via
      // AcpChatClient's test-only hook (ContenoxTestApi.lastPromptBlocksForTest).
      await composer.fill("@READ");
      await sleep(700);
      await mentionMenu.waitFor({ state: "visible", timeout: 10_000 });
      await mentionMenu.getByRole("option").filter({ hasText: "README.md" }).first().click();
      await sleep(300);
      await composer.fill("Summarize the attached file in five words or fewer.");
      await composer.press("Enter");
      {
        const deadline4 = Date.now() + 150_000;
        let stillSending = true;
        while (Date.now() < deadline4) {
          const label = await sendButton.innerText().catch(() => "");
          stillSending = /sending/i.test(label);
          if (!stillSending) break;
          await sleep(1000);
        }
        assert.ok(!stillSending, "the attachment-bearing turn should finish");
      }
      const lastBlocks = api.lastPromptBlocksForTest() as Array<{ type: string; uri?: string; name?: string }>;
      console.log(`last prompt blocks: ${JSON.stringify(lastBlocks).slice(0, 500)}`);
      const resourceLinkBlock = lastBlocks.find((block) => block.type === "resource_link");
      assert.ok(resourceLinkBlock, "sending a file attachment should include a resource_link content block, not just a chip in the UI");
      assert.ok(
        resourceLinkBlock!.uri?.includes("README.md"),
        `expected the resource_link's uri to reference README.md, got ${resourceLinkBlock!.uri}`,
      );
      await shoot(page, "14-attachment-sent");

      // B2: an editor selection attached via "Add Selection to Chat" must
      // appear as a chip -- not as the selected text pasted into the
      // composer (the "string smuggle" this phase replaces).
      const readmeUri = vscode.Uri.file(
        path.resolve(__dirname, "..", "..", "test", "fixtures", "smoke-workspace", "README.md"),
      );
      const readmeDoc = await vscode.workspace.openTextDocument(readmeUri);
      const readmeEditor = await vscode.window.showTextDocument(readmeDoc, { preview: false });
      const firstLineText = readmeDoc.lineAt(0).text;
      readmeEditor.selection = new vscode.Selection(0, 0, 0, firstLineText.length);
      await vscode.commands.executeCommand("contenox.addSelectionToChat");
      await sleep(paintMs);

      const composerValueAfterSelection = await composer.inputValue();
      assert.equal(
        composerValueAfterSelection,
        "",
        "Add Selection to Chat should not paste the selection's text into the composer",
      );
      if (firstLineText.trim()) {
        assert.ok(
          !composerValueAfterSelection.includes(firstLineText.trim()),
          "the selected text must not appear literally in the composer",
        );
      }
      const selectionChips = await attachmentList.getByRole("listitem").allInnerTexts();
      console.log(`attachment chips after Add Selection to Chat: ${selectionChips.join(" | ")}`);
      assert.ok(
        selectionChips.some((text) => text.includes("README.md")),
        "the selection should appear as an attachment chip",
      );
      await shoot(page, "15-selection-chip");

      // Clean up so it doesn't leak into later screenshots/sends. Re-queries
      // fresh each loop (rather than iterating a captured snapshot) since
      // removing an item shifts the remaining ones' indices.
      for (let guard = 0; guard < 10; guard += 1) {
        const items = await attachmentList.getByRole("listitem").all();
        if (items.length === 0) break;
        const removeButton = items[0].getByRole("button");
        if (!(await removeButton.isVisible().catch(() => false))) break;
        await removeButton.click();
        await sleep(200);
      }

      // ---------------------------------------------------------------
      // Phase 4 (vscode-implementation-plan.md §5): code out of the panel --
      // Copy / Insert at cursor / Apply to file on every rendered code block.
      // ---------------------------------------------------------------
      const scratchPath = path.resolve(
        __dirname,
        "..",
        "..",
        "test",
        "fixtures",
        "smoke-workspace",
        "artifacts",
        "contenox-insert-scratch.js",
      );
      fs.mkdirSync(path.dirname(scratchPath), { recursive: true });
      fs.writeFileSync(scratchPath, "", "utf8");
      const scratchDoc = await vscode.workspace.openTextDocument(vscode.Uri.file(scratchPath));
      await vscode.window.showTextDocument(scratchDoc, { preview: false, viewColumn: vscode.ViewColumn.Beside });

      const waitForTurnToFinish = async (budgetMs: number, label: string) => {
        const deadline = Date.now() + budgetMs;
        let stillSending = true;
        while (Date.now() < deadline) {
          const text = await sendButton.innerText().catch(() => "");
          stillSending = /sending/i.test(text);
          if (!stillSending) break;
          await sleep(1000);
        }
        assert.ok(!stillSending, `${label} turn should finish`);
      };

      // Verbatim-reproduction framing (mirrors the "hello from contenox" /
      // A4 write_file prompts above, which real models here reliably comply
      // with) rather than an open-ended formatting instruction -- more
      // robust against a weak local model dropping the triple backticks.
      await composer.fill(
        "Reply with exactly the following and nothing else, including the triple backtick fences:\n\n" +
          "```javascript\nconsole.log('contenox visual test');\n```",
      );
      await composer.press("Enter");
      await waitForTurnToFinish(150_000, "the code-block");
      // waitForTurnToFinish only observes the submit button's label; the
      // streaming bubble (StreamingMessageView, which renders its own
      // article[aria-label="Contenox"] with the same code-block toolbar) is
      // unmounted and replaced by the settled `messages`-based one a moment
      // later. Give that transition a beat so `.last()` below resolves to
      // the final, stable node rather than racing it.
      await sleep(2000);

      const lastAssistantMessage = chat.locator('article[aria-label="Contenox"]').last();
      // Distinct accessible names from the message-level "Copy" button
      // (ChatMessage's own copyText affordance) -- see chatTranscript.tsx's
      // CodeBlockToolbar comment. `.first()`: a compliant response has
      // exactly one code block, but this only needs *a* code block to expose
      // the actions, so don't fail on Playwright strict-mode if the model
      // happens to emit more than one.
      const insertButton = lastAssistantMessage.getByRole("button", { name: "Insert code at cursor" }).first();
      const applyButton = lastAssistantMessage.getByRole("button", { name: "Apply code to file" }).first();
      const copyButton = lastAssistantMessage.getByRole("button", { name: "Copy code" }).first();
      await insertButton.waitFor({ state: "visible", timeout: 30_000 });
      assert.ok(await applyButton.isVisible(), "a code block should expose an Apply action");
      assert.ok(await copyButton.isVisible(), "a code block should expose a Copy action");
      await shoot(page, "16-codeblock-actions");

      await copyButton.click();
      await sleep(300);
      const copiedConfirmation = lastAssistantMessage.getByRole("button", { name: "Copied code" }).first();
      const copyConfirmed = await copiedConfirmation.isVisible().catch(() => false);
      console.log(`copy confirmation visible: ${copyConfirmed}`);
      assert.ok(copyConfirmed, "clicking Copy should show a Copied confirmation");

      await insertButton.click();
      await sleep(1000);
      const scratchText = scratchDoc.getText();
      console.log(`scratch document text after Insert: ${scratchText}`);
      assert.ok(
        scratchText.includes("console.log('contenox visual test')"),
        `Insert at cursor should have written the code block into the active editor's document, got: ${scratchText}`,
      );
      await scratchDoc.save();
      await shoot(page, "17-codeblock-inserted");

      // Apply is diff-first: clicking it must open a real diff editor tab
      // before anything is written to disk (reusing DiffStore/vscode.diff --
      // ChatWebviewViewProvider.handleApplyCodeBlock). Point it at a real,
      // already-open file (README.md) so target resolution doesn't need a
      // manual path prompt.
      await vscode.window.showTextDocument(readmeDoc, { preview: false });
      await sleep(300);
      await applyButton.click();
      const applyDeadline = Date.now() + 20_000;
      let diffTab: vscode.Tab | undefined;
      while (Date.now() < applyDeadline && !diffTab) {
        diffTab = vscode.window.tabGroups.all
          .flatMap((group) => group.tabs)
          .find((tab) => tab.label.includes("Apply code block"));
        if (!diffTab) await sleep(500);
      }
      assert.ok(diffTab, "clicking Apply should open a diff-first review (\"Apply code block: ...\") before writing anything");
      console.log(`Apply opened diff tab: ${diffTab?.label}`);
      await shoot(page, "18-codeblock-apply-diff");

      // Decline the confirmation -- this suite must not leave README.md
      // modified. Best-effort click on the native confirmation's Cancel
      // button (lives in the workbench, not the webview iframe); falls back
      // to just hiding toasts if the native dialog isn't found in time --
      // either way the real assertion below is that the file is untouched.
      if (diffTab) {
        await vscode.window.tabGroups.close(diffTab);
      }
      const nativeCancelButton = page.getByRole("button", { name: "Cancel" });
      if (await nativeCancelButton.first().isVisible({ timeout: 5000 }).catch(() => false)) {
        await nativeCancelButton.first().click();
      } else {
        await vscode.commands.executeCommand("notifications.hideToasts");
      }
      const readmeBytesAfterApplyAttempt = await vscode.workspace.fs.readFile(readmeUri);
      const readmeTextAfterApplyAttempt = Buffer.from(readmeBytesAfterApplyAttempt).toString("utf8");
      assert.ok(
        !readmeTextAfterApplyAttempt.includes("contenox visual test"),
        "declining the Apply confirmation must leave the real file untouched",
      );

      await vscode.commands.executeCommand("contenox.refreshSessions");
      await vscode.commands.executeCommand("contenox.sessions.focus");
      await sleep(paintMs);
      await shoot(page, "04-sessions-tree");
      const sidebarText = (await page.locator("body").innerText()).replace(/\s+/g, " ");
      assert.ok(
        sidebarText.includes("hello from contenox") || sidebarText.includes(sessionId),
        "the sessions tree (History view) should list the session just chatted in, by title or id",
      );

      // Independent of the tree: load the same session's transcript through
      // the dedicated command, which calls AcpChatClient.loadSession()
      // directly -- this is a fresh session/load round trip against the
      // running `contenox acp` process, not a read of any extension-host
      // in-memory state (ChatWebviewViewProvider's own session cache is a
      // separate object never touched by this command).
      await vscode.commands.executeCommand("contenox.openSessionTranscript", sessionId);
      await sleep(paintMs);
      const transcriptDoc = vscode.workspace.textDocuments.find(
        (doc) => doc.uri.scheme === "contenox-session" && doc.uri.toString().includes(encodeURIComponent(sessionId)),
      );
      assert.ok(transcriptDoc, "session/load should have produced a reloadable transcript document");
      const transcriptText = transcriptDoc!.getText();
      console.log(`reloaded transcript (first 400 chars): ${transcriptText.slice(0, 400)}`);
      assert.ok(
        transcriptText.includes("hello from contenox") || transcriptText.includes("Reply with the exact"),
        "the transcript reloaded via session/load should contain this test's prompt/response",
      );
      await shoot(page, "05-session-transcript");

      // Runtime controls over ACP (vscode-implementation-plan.md Phase 1
      // "runtime controls over ACP"): the pickers must come from ACP
      // (initialize's workspaceConfigOptions / the current session's
      // config options) and show a real, resolved provider/model -- the
      // known bug this replaces rendered "vertex-google (not configured)"
      // because vscodeagent's bespoke listProviders/listModels never went
      // through acpsvc.Transport.
      await vscode.commands.executeCommand("contenox.controls.focus");
      await sleep(paintMs);
      const runtime = await runtimeFrame(page);
      await shoot(page, "06-runtime-controls");
      const runtimeText = (await runtime.locator("body").innerText()).replace(/\s+/g, " ").trim();
      console.log(`runtime controls text: ${runtimeText.slice(0, 400)}`);
      assert.ok(!/not configured/i.test(runtimeText), `runtime controls should not show "(not configured)": ${runtimeText}`);
      const modelValue = await runtime.locator("select#model").inputValue();
      const modelLabel = await runtime.locator("select#model option:checked").innerText();
      assert.ok(modelValue, "the model select should have a real selected value from ACP, not be empty");
      assert.notEqual(modelLabel.trim(), "not available", "the model select should offer real ACP-resolved options");
    } finally {
      // Task C: this suite must not pollute the maintainer's real
      // ~/.contenox session list. Delete every session it created via a
      // real session/delete, bypassing contenox.deleteSession's modal
      // confirmation (which would hang a headless run). Runs even when an
      // assertion above threw. Verify with `./bin/contenox session list`
      // from the repo root after a run.
      for (const id of createdSessionIds) {
        try {
          await api.deleteSessionForTest(id);
          console.log(`cleanup: deleted session ${id}`);
        } catch (error) {
          console.warn(`cleanup: failed to delete session ${id}: ${error}`);
        }
      }
      // A4's approved write and Phase 4's Insert scratch file both land in
      // the gitignored artifacts/ dir of the fixture workspace, but clean up
      // anyway so repeated runs don't accumulate stray files.
      for (const name of ["contenox-visual-test.txt", "contenox-insert-scratch.js"]) {
        try {
          fs.rmSync(
            path.resolve(__dirname, "..", "..", "test", "fixtures", "smoke-workspace", "artifacts", name),
            { force: true },
          );
        } catch {
          // best-effort
        }
      }
      await browser.close().catch(() => undefined);
    }
  });
});
