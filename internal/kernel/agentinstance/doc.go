// Package agentinstance spawns and owns running ACP agent instances on a
// server-rooted context, independent of any client connection, and lets multiple
// viewers attach to a session's event stream. Exactly one attached controller per
// session answers its permission and terminal requests.
package agentinstance
