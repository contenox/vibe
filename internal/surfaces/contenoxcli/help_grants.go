package contenoxcli

// These blocks restate the grammar internal/services/agentdecl decodes; they are
// help text, not a second parser.
const (
	toolGrantGrammar = `TOOL GRANTS. A tools allowlist is read the same way everywhere: "*" admits
every connected toolset with no exceptions, "!name" removes one wherever it
appears, a bare name grants exactly that one, and an empty list grants nothing.
"native-" and "decl-" are namespaces — they keep a declared source from
colliding with an in-process toolset — never a hidden exclusion: "*" admits
those like anything else.`

	toolGrantLine = `Tool grants read the same everywhere: "*" admits every connected toolset with no
exceptions, "!name" removes one, a bare name grants exactly that one, and an
empty list grants nothing.`

	askWaitGrammar = `BOUNDING AN ASK. Any grant, in its table form, carries the wait its approve
rules get:

  shell = { grant = "approve", timeout = "30m", on_timeout = "deny" }
  merge = { grant = "approve", timeout = "never" }

timeout is a duration written the way Go writes one — 90s, 30m, 2h, 1h30m —
whole seconds, positive, at most 168h; or one of never, forever, indefinite,
which is an ask with no deadline at all: it stays pending until somebody
answers it, across restarts and for as long as that takes. "deny" is the only
on_timeout this build can express, and an omitted on_timeout resolves the same
way; naming one beside an indefinite timeout is refused, because nothing can
expire. Write no timeout at all and the rule carries no deadline of its own:
the ask falls to this host's approval ceiling — 'contenox config set
approval-ceiling <duration|never>', seven days until you set it. A wait on a
grant that never asks is refused, naming the envelope and the axis.`

	askWaitLine = `An ask this envelope raises can carry a wait — timeout = "30m", on_timeout =
"deny" beside the grant, or timeout = "never" for an ask that waits until it is
answered. Raised without one it falls to this host's approval ceiling
('contenox config set approval-ceiling', seven days until you set it).`
)
