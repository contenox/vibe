// Package libevents holds the consumer-side state of an event log: durable
// cursors, firing claims with recorded outcomes, listener subscriptions, and
// staged events held until a due time. It does not hold the event log itself.
// Every mutator takes a libdbexec.Exec so a claim or listener change can
// share the caller's transaction; stores are scoped at construction.
package libevents
