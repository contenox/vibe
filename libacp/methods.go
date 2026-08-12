package libacp

import (
	"context"
	"encoding/json"
	"strings"
)

const ProtocolVersion = 1

const (
	MethodInitialize   = "initialize"
	MethodAuthenticate = "authenticate"
	MethodLogout       = "logout"

	MethodSessionNew             = "session/new"
	MethodSessionLoad            = "session/load"
	MethodSessionResume          = "session/resume"
	MethodSessionClose           = "session/close"
	MethodSessionDelete          = "session/delete"
	MethodSessionList            = "session/list"
	MethodSessionPrompt          = "session/prompt"
	MethodSessionCancel          = "session/cancel"
	MethodSessionUpdate          = "session/update"
	MethodSessionSetMode         = "session/set_mode"
	MethodSessionSetConfigOption = "session/set_config_option"
	// MethodSessionSetModel is the UNSTABLE, experimental model-picker method:
	// switch a session's active model (see SetSessionModelRequest / SessionModelState).
	MethodSessionSetModel = "session/set_model"

	MethodSessionRequestPermission = "session/request_permission"

	// MethodCancelRequest is the protocol-level "$/cancel_request"
	// notification: either side may signal it no longer awaits the response to
	// an in-flight request; "$/"-prefixed methods are always safe to ignore.
	MethodCancelRequest = "$/cancel_request"

	MethodFSReadTextFile  = "fs/read_text_file"
	MethodFSWriteTextFile = "fs/write_text_file"

	MethodTerminalCreate      = "terminal/create"
	MethodTerminalOutput      = "terminal/output"
	MethodTerminalWaitForExit = "terminal/wait_for_exit"
	MethodTerminalKill        = "terminal/kill"
	MethodTerminalRelease     = "terminal/release"
)

// ExtensionMethodPrefix is the reserved namespace for custom "extension"
// methods and notifications (any method name starting with underscore);
// "$/"-prefixed methods (MethodCancelRequest) are never extension-eligible.
const ExtensionMethodPrefix = "_"

// IsExtensionMethod reports whether method is eligible for dispatch through
// an ExtRequestHandler/ExtNotificationHandler: non-empty and starting with
// ExtensionMethodPrefix.
func IsExtensionMethod(method string) bool {
	return strings.HasPrefix(method, ExtensionMethodPrefix)
}

// ExtRequestHandler handles an inbound extension request (method not in the
// core ACP set but IsExtensionMethod), returning a raw JSON result or an
// *Error.
type ExtRequestHandler func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *Error)

// ExtNotificationHandler handles an inbound extension notification,
// fire-and-forget.
type ExtNotificationHandler func(ctx context.Context, method string, params json.RawMessage)
