// Package taskengine provides a configurable workflow system for building AI-powered task chains.
// It supports LLM interactions, external integrations via hooks, complex branching logic,
// and robust error handling with retries and timeouts.
//
// Key Features:
// - Multiple task handlers (condition keys, parsing, hooks, no-op)
// - Type-safe data passing between tasks
// - Conditional branching with various operators
// - External system integration via hooks
// - Comprehensive debugging and monitoring
// - Template-based prompt generation
//
// taskengine enables building AI workflows where tasks are linked in sequence, supporting
// conditional branching, numeric or scored evaluation, range resolution, and optional
// integration with external systems (hooks). Each task can invoke an LLM prompt or a custom
// hook function depending on its type.
//
// Hooks are pluggable interfaces that allow tasks to perform side effects — calling APIs,
// saving data, or triggering custom business logic — outside of prompt-based processing.
//
// Typical use cases:
//   - Chatbot systems
//   - Data processing pipelines
//   - Automated decision-making systems
//   - Dynamic content generation (e.g. marketing copy, reports)
//   - AI agent orchestration with branching logic
//   - Decision trees based on LLM outputs
//   - Automation pipelines involving prompts and external system calls
package taskengine
