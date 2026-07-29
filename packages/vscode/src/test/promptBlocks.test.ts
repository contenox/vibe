import * as assert from "node:assert/strict";
import { buildPromptBlocks } from "../acp/AcpChatClient";
import { WireAttachment } from "../chat/webviewProtocol";

// Phase 3 "real context attachment" (vscode-implementation-plan.md §5):
// asserts the actual outgoing ACP content-block shape for each attachment
// kind, independent of the (slow, model-calling) visual suite -- a
// deterministic complement to it, not a replacement (the visual suite still
// proves the real @file picker produces the same shape end to end via
// AcpChatClient.getLastPromptBlocksForTest).
suite("buildPromptBlocks", () => {
  test("a file attachment becomes a resource_link only -- the agent dereferences it", () => {
    const attachment: WireAttachment = {
      id: "att-1",
      kind: "file",
      name: "foo.ts",
      description: "src/foo.ts",
      uri: "file:///workspace/src/foo.ts",
      languageId: "typescript",
      text: "export const foo = 1;",
    };
    const blocks = buildPromptBlocks("what does this do?", [attachment]);

    const resourceLink = blocks.find((block) => block.type === "resource_link");
    assert.ok(resourceLink, "expected a resource_link content block for the file attachment");
    assert.equal((resourceLink as { uri: string }).uri, attachment.uri);
    assert.equal((resourceLink as { name: string }).name, attachment.name);

    // A whole file is a pointer: inlining its contents would double the token
    // cost when the agent can read_file it on demand.
    assert.ok(
      !blocks.some((block) => block.type === "text" && (block as { text: string }).text.includes("export const foo = 1;")),
      "a whole-file attachment must not inline its contents",
    );

    const promptBlock = blocks.find(
      (block) => block.type === "text" && (block as { text: string }).text === "what does this do?",
    );
    assert.ok(promptBlock, "expected the user's message to still be sent as its own text block");
  });

  test("an image attachment becomes a single image block, not a resource_link", () => {
    const attachment: WireAttachment = {
      id: "att-2",
      kind: "image",
      name: "pasted-image.png",
      mimeType: "image/png",
      data: "aGVsbG8=",
    };
    const blocks = buildPromptBlocks("look at this", [attachment]);
    const imageBlocks = blocks.filter((block) => block.type === "image");
    assert.equal(imageBlocks.length, 1);
    assert.equal((imageBlocks[0] as { data: string }).data, "aGVsbG8=");
    assert.equal((imageBlocks[0] as { mimeType: string }).mimeType, "image/png");
    assert.ok(
      !blocks.some((block) => block.type === "resource_link"),
      "an image attachment should not also produce a resource_link block",
    );
  });

  test("an attachment without a uri (e.g. git diff) sends only a text block", () => {
    const attachment: WireAttachment = {
      id: "att-3",
      kind: "git_diff",
      name: "Git changes",
      text: "diff --git a/x b/x",
    };
    const blocks = buildPromptBlocks("review this", [attachment]);
    assert.ok(!blocks.some((block) => block.type === "resource_link"));
    assert.ok(blocks.some((block) => block.type === "text" && (block as { text: string }).text.includes("diff --git")));
  });

  test("no attachments still sends the plain message", () => {
    const blocks = buildPromptBlocks("hello", []);
    assert.equal(blocks.length, 1);
    assert.equal(blocks[0].type, "text");
  });
});
