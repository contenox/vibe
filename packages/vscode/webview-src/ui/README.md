# Vendored chat UI primitives

Vendored copy of the chat subset of `@contenox/ui` (`packages/ui/src` at commit
`e2a09836`), taken when the extension was revived after the repository dropped
its `packages/` JS workspace for the Go TUI surface (`internal/surfaces/beamtui`).

Scope is the closure `webview-src/chat/ChatSurface.tsx` imports: the `chat.ts`
barrel, its 13 exported components, and 10 transitive modules. Nothing else in
the repository consumes them, so the webview keeps its own copy and builds
without a cross-package prebuild.

`index.css` holds the Tailwind v4 `@theme` tokens the components style against.
`webview-src/webview.css` imports it, then remaps those tokens onto VS Code's
semantic theme variables so the webview follows the editor theme.
