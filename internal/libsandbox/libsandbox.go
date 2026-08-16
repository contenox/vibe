// Package libsandbox confines a spawned foreign agent to "the wall": every path
// out of its tools and workspace — filesystem, network, environment — is absent
// by construction. If confinement cannot be built, the agent is not spawned.
package libsandbox
