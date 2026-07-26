// Package openrouter is a catalog provider for OpenRouter (openrouter.ai),
// which exposes 300+ models from many providers through a single
// OpenAI-compatible endpoint. It reuses the shared chatcompletions codec;
// only the base URL, model-list parsing, and API-key transport are specific
// to OpenRouter.
package openrouter
