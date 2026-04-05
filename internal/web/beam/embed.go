package beam

import "embed"

// Dist holds the Beam SPA. beam_ui_embed_stamp.txt is a content hash of dist/
// (rewritten by scripts/beam_embed_stamp.sh after vite build). Embedding it
// alongside dist/ ensures the Go toolchain always recompiles this package when
// the UI bundle changes, so bin/contenox does not serve a stale embed.
//
//go:embed dist beam_ui_embed_stamp.txt
var Dist embed.FS
