package libacp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingClient records every session/update it receives and can be made to
// return an error from SessionUpdate to prove the wrapper forwards it.
type recordingClient struct {
	libacp.UnimplementedClient
	seen   []libacp.SessionID
	retErr error

	permCalled bool
}

func (c *recordingClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.seen = append(c.seen, n.SessionID)
	return c.retErr
}

func (c *recordingClient) RequestPermission(context.Context, libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	c.permCalled = true
	return libacp.RequestPermissionResponse{}, nil
}

func TestUnit_FilterSessionUpdates_Sequences(t *testing.T) {
	const live = libacp.SessionID("live")

	cases := []struct {
		name string
		seq  []libacp.SessionID
		want []libacp.SessionID
	}{
		{
			name: "all live pass through",
			seq:  []libacp.SessionID{live, live, live},
			want: []libacp.SessionID{live, live, live},
		},
		{
			name: "stale updates from an abandoned session dropped",
			seq:  []libacp.SessionID{"old", "old", live},
			want: []libacp.SessionID{live},
		},
		{
			name: "interleaved stale updates dropped, live kept in order",
			seq:  []libacp.SessionID{live, "old", live, "other", live},
			want: []libacp.SessionID{live, live, live},
		},
		{
			name: "empty session id treated as non-live",
			seq:  []libacp.SessionID{"", live},
			want: []libacp.SessionID{live},
		},
		{
			name: "nothing live",
			seq:  []libacp.SessionID{"a", "b", "c"},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingClient{}
			filtered := libacp.FilterSessionUpdates(live, inner)
			for _, sid := range tc.seq {
				require.NoError(t, filtered.SessionUpdate(context.Background(), libacp.SessionNotification{SessionID: sid}))
			}
			assert.Equal(t, tc.want, inner.seen)
		})
	}
}

func TestUnit_FilterSessionUpdates_ForwardsInnerError(t *testing.T) {
	sentinel := errors.New("render failed")
	inner := &recordingClient{retErr: sentinel}
	filtered := libacp.FilterSessionUpdates("live", inner)

	// A live update reaches inner, so its error propagates.
	err := filtered.SessionUpdate(context.Background(), libacp.SessionNotification{SessionID: "live"})
	assert.ErrorIs(t, err, sentinel)

	// A stale update never reaches inner, so it is a silent no-op (nil).
	err = filtered.SessionUpdate(context.Background(), libacp.SessionNotification{SessionID: "stale"})
	assert.NoError(t, err)
}

func TestUnit_FilterSessionUpdates_PassesThroughOtherMethods(t *testing.T) {
	inner := &recordingClient{}
	filtered := libacp.FilterSessionUpdates("live", inner)

	_, err := filtered.RequestPermission(context.Background(), libacp.RequestPermissionRequest{})
	require.NoError(t, err)
	assert.True(t, inner.permCalled, "non-SessionUpdate methods must pass straight through to inner")
}
