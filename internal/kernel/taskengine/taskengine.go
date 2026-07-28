// Package taskengine orchestrates an agent: it drives LLM turns, tool calls,
// and routing in a loop, defined as a JSON chain you version in git. The unit
// of execution is the conversation; the TaskEvent stream is the contract
// clients consume (see docs/development/engine-events.md). Data is shaped
// into a turn or produced by a tool call, never mutated invisibly where no
// event would see it.
package taskengine
