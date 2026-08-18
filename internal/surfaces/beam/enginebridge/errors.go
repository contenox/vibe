package enginebridge

import "errors"

var (
	// ErrClosed reports a call on a Bridge whose Close has already run.
	ErrClosed = errors.New("enginebridge: bridge is closed")

	// ErrPromptInFlight reports a second SubmitPrompt before the session's
	// previous turn ended; wait for TurnEnded/TurnFailed or Cancel first.
	ErrPromptInFlight = errors.New("enginebridge: a prompt is already in flight for this session")

	// ErrEmptyPrompt reports a SubmitPrompt with nothing in it.
	ErrEmptyPrompt = errors.New("enginebridge: empty prompt")

	// ErrShellDisabled reports the runtime was built without shell sessions
	// (Deps.ShellSessions nil): the feature is absent, not broken.
	ErrShellDisabled = errors.New("enginebridge: shell sessions are not enabled")

	// ErrUncleanShutdown reports Close gave up joining a goroutine within
	// shutdownJoinTimeout; the caller must not close the bus or database
	// afterwards (see Close).
	ErrUncleanShutdown = errors.New("enginebridge: unclean shutdown, resources abandoned")
)
