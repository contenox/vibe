package libacp_test

import (
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_JSONRPCErrorCodesMatchACPSpec(t *testing.T) {
	require.Equal(t, -32700, libacp.ErrParseError)
	require.Equal(t, -32600, libacp.ErrInvalidRequest)
	require.Equal(t, -32601, libacp.ErrMethodNotFound)
	require.Equal(t, -32602, libacp.ErrInvalidParams)
	require.Equal(t, -32603, libacp.ErrInternalError)
	require.Equal(t, -32000, libacp.ErrAuthRequired)
	require.Equal(t, -32002, libacp.ErrResourceNotFound)
}
