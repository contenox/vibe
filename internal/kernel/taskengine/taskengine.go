// Package taskengine orchestrates an agent: it drives LLM turns, tool calls and
// routing in a loop, defined as a JSON chain. The TaskEvent stream is the
// contract clients consume.
package taskengine
