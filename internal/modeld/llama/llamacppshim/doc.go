// Package llamacppshim owns the direct llama.cpp C API boundary for modeld.
//
// It is intentionally small: product code talks to modeld/llama.Session, while
// this package exposes the native substrate used by the llama.cpp adapter.
package llamacppshim
