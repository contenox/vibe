// Package libsandbox confines a spawned foreign agent to "the wall": a process
// that can act only through the tools it is given and the workspace it is
// handed, with every other path — the rest of the filesystem, the network, the
// inherited environment — absent by construction rather than merely discouraged.
//
// It is pure mechanism. It knows nothing about ACP, tools, human-in-the-loop, or
// any permission policy, and it makes no per-operation decisions: there is a
// wall and a short list of holes, and the wall does not deliberate. Whatever
// gates the agent's *sanctioned* actions lives at the tool layer, at the
// altitude where intent is legible; libsandbox governs the other surface — the
// code the agent runs inside its own process (its Bash, and everything its
// toolchain drags in, such as an `npm install` postinstall script) — which no
// tool gate can see, because it is not a tool call.
//
// # Necessity, not permission
//
// The only holes in the wall are functional necessities: a path the agent
// breaks without — it must read ~/.claude to authenticate, or reach a package
// registry to install — justified one by one, minimized, and read-only wherever
// it can be. A carve-out means "the agent cannot boot or work without this," it
// never means "policy allows this." The default answer is no hole. The loot
// paths a supply-chain attack hunts — ~/.ssh, ~/.aws, ~/.npmrc, ~/.contenox —
// are simply not reachable: they are not under the scoped HOME and not carved
// out, so there is nothing to guard.
//
// # No bypassing
//
// Side effects flow through the tools and the workspace; all other paths are
// meant to be absent, not policed. That is the invariant the wall exists to make
// true, and every attempt to reach the wall is (in later slices) recorded rather
// than swallowed — a well-behaved agent uses its tools and never touches it, so
// anything that does is the anomaly signal.
//
// # Enforcement is layered in later
//
// This slice establishes the spec (Spec), the on-disk necessity-list format
// (LoadCarveouts), the credential-leak fix (env-scrub: a minimal environment
// with a forced scoped HOME, so no inherited credential rides along), and the
// assembly path (Command) with a platform seam where the deny-by-construction
// mechanisms drop in. On Linux that seam is where Landlock, a routeless network
// namespace, and a mount namespace anchoring the scoped HOME will build the
// fs/net/process walls; the whole runtime is CGo-free, so those mechanisms are
// driven through golang.org/x/sys/unix and cmd.SysProcAttr, never libseccomp.
// Until they land, Command still scrubs the environment and pins the working
// directory and HOME on every platform — the leak is closed first — but the wall
// itself is not yet enforced. That gap is deliberate and staged, not an
// oversight: the API and the assembly path are settled here so the enforcement
// mechanics slot in without reshaping anything a caller sees.
package libsandbox
