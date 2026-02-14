package libbus_test

import (
	"context"
	"testing"

	libbus "github.com/contenox/vibe/libbus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestStartupNATSCluster(t *testing.T) {
	ctx := context.TODO()
	url, container, cleanup, err := libbus.SetupNatsInstance(ctx)
	defer cleanup()
	require.NoError(t, err)
	require.True(t, container.IsRunning())
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	err = nc.Publish("foo", []byte("Hello World"))
	require.NoError(t, err)
}
