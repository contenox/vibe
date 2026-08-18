---
title: "Trusted binaries"
description: Pin the allowlisted commands in a HITL policy to a real path and a SHA256, so a name cannot be substituted underneath it.
order: 4
---

# Trusted Binaries

A HITL policy's `command_prefix_allowlist` says `go build` may run without asking. It pins a **name**. This page is about the gap that leaves, and how to close it with two declarations: where a binary may come from, and what it must hash to.

## Why a name is not a binary

`PATH` decides what a name means. When the policy allows `go` and something earlier on `PATH` contains a file called `go`, that file is what runs — the allow rule blessed the name, and the name now points somewhere else. Any directory the agent (or anything the agent ran) can write to is enough: a build scratch dir, a project-local `node_modules/.bin`, a `~/bin` the operator forgot about. Resolving the name and comparing it to a resolution of the same name does not help, because both resolve through the same substituted `PATH`. The only answer that holds is to resolve the name to an **absolute real path** and check that path against something the operator declared out of band.

That is what `trusted_binaries` is. It has two independent halves:

| Half | Key | Question it answers |
| --- | --- | --- |
| Identity | `dirs` | May a name resolve from this directory at all? |
| Integrity | `hashes` | Is the file at this path still the one I declared? |

Both are opt-in, and both can only **withdraw** an allow. A policy without a `trusted_binaries` block behaves exactly as it did before this existed. A policy with one refuses more, never less: a withdrawn allow falls through to the policy's approve floor, so the call stops for a human rather than failing.

> **Note:**
> The failure mode is always refusal. There is no warn-and-run mode, and no "record the hash on first use" convenience mode — that would weaken precisely what the declaration exists for.

## Finding the real path

Declare the path the command *actually resolves to*, with symlinks followed. The `contenox hitl trust` verb does this for you, but here is how to confirm it by hand.

**Linux / macOS**

```bash
command -v go              # what PATH resolves the name to
readlink -f "$(command -v go)"   # follow symlinks to the real file (Linux)
realpath "$(command -v go)"      # same, where realpath is available
```

A symlinked toolchain is the common case, and it matters:

```console
$ command -v go
/usr/bin/go
$ readlink -f "$(command -v go)"
/usr/lib/go-1.22/bin/go
```

`/usr/lib/go-1.22/bin/go` is the path to declare. Declaring `/usr/bin/go` would never match, and the refusal message would name the real path so you could correct it.

**Windows**

`where.exe` is Windows' own resolution oracle. It lists every match in `PATHEXT` order, and the first line is what will run:

```console
C:\> where.exe git
C:\Program Files\Git\cmd\git.exe
```

`PATHEXT` is real and wider than most people expect — on a stock Windows 11 box it is `.COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC;.CPL`. A policy naming `foo` can resolve to `foo.CMD`, or to `foo.JS`:

```console
C:\> where.exe foo
C:\Users\Public\pathexttest\foo.CMD
C:\Users\Public\pathexttest\foo.JS
```

contenox resolves the same way `where.exe` does and picks the same first match, then canonicalizes the case of the result — so the declaration reads `foo.CMD`, the name actually on disk.

## Computing the hash

| Platform | Command |
| --- | --- |
| Linux | `sha256sum /usr/lib/go-1.22/bin/go` |
| macOS | `shasum -a 256 /usr/local/go/bin/go` |
| Windows | `(Get-FileHash -Algorithm SHA256 'C:\Program Files\Git\cmd\git.exe').Hash` |

All three produce the same digest for the same bytes. `Get-FileHash` prints uppercase hex and the others lowercase; contenox compares case-insensitively, so either is fine in the policy file.

## Declaring path and hash

The declarations live in the policy file, next to the rules they gate — versioned, diffable, and validated by `contenox vet`. You will normally write them with the verb rather than by hand:

```bash
contenox hitl trust go git grep
```

which resolves each name exactly as the evaluator will, and splices the result into the policy:

```json
{
  "trusted_binaries": {
    "dirs": [
      "/usr/bin",
      "/usr/lib/go-1.22/bin"
    ],
    "hashes": {
      "/usr/bin/git": "22fead8244ef3a7225fb800099a4e43eca8bcec0466774917669599c2f19a05a",
      "/usr/bin/grep": "20a35a4c18f2fe9e6305c1cbf866b9c0fcc3f957132ae66028c99ec353bb0a80",
      "/usr/lib/go-1.22/bin/go": "89b81bd72c27404ccfd701c136b7e3ace9a4ccb26d96d97e874b48829aed27a1"
    }
  },
  "default_action": "approve",
  "rules": [
    {
      "tools": "local_shell",
      "tool": "local_shell",
      "action": "allow",
      "when": [
        {
          "key": "command",
          "op": "command_prefix_allowlist",
          "value": "go build,go test,grep,git status"
        }
      ]
    },
    {
      "tools": "local_shell",
      "tool": "local_shell",
      "action": "approve"
    }
  ]
}
```

With this in place, `go build ./...` runs unattended only while `go` still resolves to `/usr/lib/go-1.22/bin/go` and that file still hashes to the declared digest. A `go` planted earlier on `PATH` resolves outside `dirs` and is refused; a swapped `/usr/lib/go-1.22/bin/go` fails the digest and is refused. Either way the call stops at the approve floor, and the approval card carries the reason rather than appearing unexplained:

```text
────────────────────────────────────────────────────
  HITL approval required
  Tools : local_shell
  Tool  : local_shell
  Reason: binary at /tmp/scratch/go is not under any trusted_binaries.dirs entry — allow refused; declare its directory after verifying what it is
  Args  :
    command: go build ./...
```

Rules for the two keys:

- **`dirs`** entries must be **absolute** and are matched at any depth. Symlinks are resolved on both sides, so `/var` vs `/private/var` on macOS is not a spurious refusal.
- **`hashes`** keys must be the **absolute real path** (post-symlink), and values must be 64 hex characters.
- Declaring **any** hash makes the pin **strict** for that policy: a command with no declared hash is refused. This is deliberate — a partial pin that waves through everything undeclared pins nothing.
- Declaring **only** `hashes` still stops `PATH` substitution, because the substituted binary has no declared hash. `dirs` is the broader brush: it refuses whole neighbourhoods without enumerating files.
- A malformed block fails the **whole policy** to load, and contenox falls back to its rule-free, approve-everything default. That is fail-closed by design — you get approval prompts, not silent unreviewed execution. `contenox vet` catches it first.

> **Note:**
> Declarations describe **one host**. Absolute paths and digests are machine-specific, and `/usr/bin` is not even an absolute path on Windows. A policy file shared across platforms needs a per-platform copy of the `trusted_binaries` block — this is why contenox ships no declarations in its envelopes: seeding one would ship a false claim.

## Where it sits in an envelope

`trusted_binaries` is a **pass-through** block. An
[envelope](/docs/reference/agents-config/#envelopesname) carries it verbatim into
the rendered policy — the transpiler validates its shape and changes nothing
about it, because there is nothing to compile: the block is already the two lists
the evaluator reads.

```toml
[envelopes.mine.trusted_binaries]
dirs = ["/usr/bin", "/usr/lib/go-1.22/bin"]

[envelopes.mine.trusted_binaries.hashes]
"/usr/lib/go-1.22/bin/go" = "89b81bd72c27404ccfd701c136b7e3ace9a4ccb26d96d97e874b48829aed27a1"
```

Under `extends` it merges per leaf key, so a child adding one hash keeps the
parent's `dirs`.

**But an envelope is usually the wrong place for it.** Digests describe one host,
and `agents.toml` is a file you share across machines and check into a repo.
`contenox hitl trust` therefore refuses to write into a rendered envelope at all
— that file is rewritten from `agents.toml` on the next run, so a hash declared
there would be silently discarded:

```console
$ contenox hitl trust --policy hitl-policy-default.json go
~/.contenox/.generated/hitl-policy-default.json is rendered from [envelopes.default]
in agents.toml and is rewritten on every run — a hash declared there would be
silently discarded.
Copy it to ~/.contenox/hitl-policy-default.json first (a top-level copy shadows
the rendered one), then re-run; or pass --policy with an explicit path
```

That copy is the intended home: a per-host file, owned by whoever runs that host,
sitting ahead of the render on the search path. Put the axes in the envelope and
the digests in the copy.

## The upgrade workflow

`go`, `npm`, `git` and friends change hashes constantly, and that is not an attack. When a declared binary changes, the next call naming it is refused with:

```text
binary at /usr/lib/go-1.22/bin/go does not match the declared hash — re-declare after verifying the upgrade, or investigate
```

The instruction is the workflow. Establish that the change was a legitimate upgrade — you ran the package manager, the version moved, the vendor's checksum matches — and then re-declare:

```bash
contenox hitl trust --refresh          # re-read every declared binary
contenox hitl trust go                 # or just the one that moved
```

`--refresh` re-reads each declared path and rewrites its digest, reporting each change so the diff is reviewable before you commit it. If you cannot explain the change, do not refresh it: that is the case the declaration exists for.

Other verbs:

```bash
contenox hitl trust --list             # every declaration and its state here
contenox hitl trust --remove go        # drop a declaration
contenox hitl trust --policy ./ops/host.json go        # a specific file
```

## How vet and doctor report drift

`contenox vet` checks every declaration against the host it runs on. A drifted entry is a **warning**, not a failure — the envelope is still valid, and the runtime's answer for it is already a refusal:

```console
$ contenox vet
ok   /home/you/.contenox/hitl-policy-default.json
WARN /home/you/.contenox/hitl-policy-default.json
     trusted_binaries.hashes: /usr/lib/go-1.22/bin/go: does not match the declared hash (on disk 949978a3…) — re-declare after verifying the upgrade, or investigate
```

`contenox doctor` reports the same drift under the policy category, because a stale declaration is invisible from the inside: you see an approval card for a command that used to run unattended, with no hint that a binary changed underneath it. Doctor names the file, the entries, and `contenox hitl trust --refresh`.

The states either can report: `missing` (declared, not present), `mismatch` (present, different bytes), `unreadable` (present, cannot be read — calls naming it are refused), and `outside_dirs` (declared outside every `dirs` entry, so its hash can never be reached).

## Per-platform guarantees

Linux-shaped guarantees are not promised to all three platforms. What each layer actually delivers:

| | Linux | macOS | Windows |
| --- | --- | --- | --- |
| **Identity** — name resolved to an absolute real path, checked against `dirs` | yes | yes | yes (`PATHEXT` search, matching `where.exe`; case-insensitive comparison) |
| **Integrity** — SHA256 pin, refusal on mismatch | yes | yes | yes |
| **Structural shell reading** — compound lines like `git status && go build` read instead of blanket-refused | yes (POSIX `sh`) | yes (POSIX `sh`) | **no** — `local_shell` spawns PowerShell or `cmd`, which the analyzer does not parse and deliberately refuses to guess at |
| **Kernel-enforced sandbox** (see [Agent sandbox](/docs/guide/confinement/sandbox/)) | yes (Landlock) | no | no |

The Windows row on structural reading is a floor, not a hole: an unread line keeps the tokenizer's verdict, which is the stricter one. Identity and integrity apply on Windows through the plain-argv path, so an allowlisted `go build` there is still pinned to a real binary.

## What this does not protect

Stated plainly, because a security control that overstates itself is worse than none.

- **The `(path, size, mtime)` hash cache.** Hashing a 100MB toolchain binary on every tool call is not affordable, so a digest is cached and recomputed only when the file's size or modification time changes. Someone who can rewrite the binary can also restore its size and mtime (`touch -r`), and the cache would then serve the stale digest for the life of the process. The key deliberately omits inode, because Windows has none and a portable key beats a POSIX-only one.
- **A hostile operator, or anything running as root.** contenox does not defend you against your own root shell. If an attacker can write to `/usr/bin`, edit the policy file, or set `LD_PRELOAD` on the contenox process itself, they are already past every control on this page.
- **A compromised kernel, or a compromised contenox process.** Out of scope entirely.
- **Time-of-check to time-of-use.** The binary is verified at policy-evaluation time and executed moments later. A swap inside that window is not detected — and requires write access to a declared trusted directory, which is the previous bullet.

The assumption this rests on is stated rather than defended: **a properly set up system** — sane permissions, the operator not running as root, trusted directories not writable by the agent. Under that assumption, path plus hash covers the realistic cases: a `PATH`-planted alias, a tampered tool, a supply-chain swap of a binary you build against. Outside it, nothing here helps, and pretending otherwise would be the actual failure.

## See also

- [HITL Policies](/docs/guide/hitl/) — the policy file, its rules and tiers
- [Agent sandbox](/docs/guide/confinement/sandbox/) — kernel-enforced confinement, and where it is available
- [Environment scrubbing](/docs/guide/confinement/environment/) — keeping credentials out of the shell an agent drives
- [Agent threat model](/docs/guide/confinement/why/) — what the whole envelope is and is not for
