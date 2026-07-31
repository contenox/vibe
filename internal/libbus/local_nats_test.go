package libbus_test

import (
	"context"
	"testing"

	libbus "github.com/contenox/contenox/internal/libbus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestUnit_StartupNATSCluster(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
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
