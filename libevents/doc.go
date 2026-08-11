// Package libevents is the consumer-side state of an event log: durable
// cursors (where a named consumer has read to), firing claims (which
// (trigger, event) pairs have already run, with their recorded outcome),
// listener subscriptions (who wants which event types, filtered how, waking
// what), and staged events (payloads held back until a due time).
//
// It deliberately does not carry the event log itself. A log is append-only
// rows with a monotonic NID minted inside the append transaction; each
// importer already has one, shaped for its own retention. What must not fork
// between importers is the consumer semantics — the claim that is the primary
// key's guarantee, the stale-claim takeover, the reversible outcome row —
// because a consumer written against one definition must behave identically
// against the other.
//
// Every mutator takes a libdbexec.Exec so a claim or a listener change can
// share the caller's transaction: a claim inside a transaction releases
// itself on rollback, which is the default claim style for effects that are
// themselves database writes. Claims that must outlive a transaction —
// external sends, agent runs — commit first and record their outcome after,
// and only those need the stale-claim bound, which is a required constructor
// parameter because a copied constant rots when the workloads it was derived
// from change.
//
// Stores are scoped at construction: the scope value is an unconditional
// predicate in every statement, so no filter can widen a read past it. An
// importer whose consumers are deliberately cross-scope passes the empty
// scope and keeps the scope dimension on its events instead.
package libevents
