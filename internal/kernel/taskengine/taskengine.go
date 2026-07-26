// Package taskengine orchestrates an agent: it drives LLM turns, tool calls,
// and routing in a loop, defined as a JSON chain you version in git. Handlers
// are chat_completion, execute_tool_calls, tools, route, raise_error, and noop;
// transitions route control flow (equals, contains, starts_with, ends_with,
// default, edge_traversed_at_least).
//
// It is an agent control-flow engine, not a dataflow/workflow engine. The unit
// of execution is the conversation, and the TaskEvent stream is the contract
// clients consume. It deliberately does not transform structured data across
// steps: data is shaped into a turn (templates, observable in the conversation)
// or produced by a tool call (observable as an event) — never mutated
// invisibly on the forward path where no event would see it.
package taskengine
