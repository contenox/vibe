// Lazy loader for the ESM-only @agentclientprotocol/sdk from this
// CommonJS extension host (see docs/development/internal/acp-client-ts-spike.md).
//
// A static `import` here would compile (under tsc's "module": "nodenext")
// to a synchronous `require()` of a "type": "module" package, which throws
// ERR_REQUIRE_ESM in the tsc-only dev/test path (dist/extension.js is not
// bundled there). Dynamic `import()` is real ESM interop: it works
// unbundled (tsc output, Node loads the SDK's own ESM files) and inside
// the esbuild-bundled dist/extension.js (packaged), where esbuild has
// already inlined the SDK as CommonJS and rewrites the import() to resolve
// it locally instead of hitting Node's module loader at all.
import type * as Acp from "@agentclientprotocol/sdk" with { "resolution-mode": "import" };

let sdkPromise: Promise<typeof Acp> | undefined;

export function loadAcpSdk(): Promise<typeof Acp> {
  if (!sdkPromise) {
    sdkPromise = import("@agentclientprotocol/sdk");
  }
  return sdkPromise;
}
