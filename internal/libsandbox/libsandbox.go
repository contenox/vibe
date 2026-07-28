// Package libsandbox confines a spawned foreign agent to "the wall": every path
// out of its tools and workspace — filesystem, network, environment — is
// absent by construction, not merely denied by policy. Holes exist only for
// functional necessities (e.g. reading credentials to authenticate), never for
// permission. If confinement cannot be built, the agent must not be spawned.
package libsandbox
