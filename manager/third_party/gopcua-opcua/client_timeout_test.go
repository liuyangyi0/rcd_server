package opcua

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

func TestPublishBadTimeoutDoesNotDisconnect(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(reconnect), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			peer := newResumePeer(t, ctx, resumePeerConfig{publishFault: ua.StatusBadTimeout, publishFaultCount: 3})
			client, err := NewClient(peer.endpoint, AutoReconnect(reconnect), RequestTimeout(time.Second))
			require.NoError(t, err)
			require.NoError(t, client.Connect(ctx))
			defer client.Close(context.Background())
			session, channel := client.Session(), client.SecureChannel()
			notifs := make(chan *PublishNotificationData, 8)
			_, err = client.Subscribe(ctx, &SubscriptionParameters{Interval: 10 * time.Millisecond}, notifs)
			require.NoError(t, err)
			require.EqualValues(t, 11, resumeValue(t, ctx, notifs))
			require.Never(t, func() bool { return client.State() != Connected || client.SecureChannel() != channel }, 100*time.Millisecond, time.Millisecond)
			require.NoError(t, client.UpdateNamespaces(ctx))
			require.Same(t, session, client.Session())
			peer.mu.Lock()
			defer peer.mu.Unlock()
			require.Equal(t, 1, peer.channels)
			require.Equal(t, 1, peer.sessionCreates)
			require.Equal(t, 1, peer.subscriptionCreates)
			require.GreaterOrEqual(t, peer.firstPublish, 4)
		})
	}
}

func TestCallBadTimeoutIsReturnedWithoutRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := newResumePeer(t, ctx, resumePeerConfig{callFault: ua.StatusBadTimeout})
	client, err := NewClient(peer.endpoint, AutoReconnect(false), RequestTimeout(time.Second))
	require.NoError(t, err)
	require.NoError(t, client.Connect(ctx))
	defer client.Close(context.Background())
	session, channel := client.Session(), client.SecureChannel()
	err = client.Send(ctx, &ua.CallRequest{}, func(ua.Response) error { return nil })
	require.ErrorIs(t, err, ua.StatusBadTimeout)
	require.Never(t, func() bool { return client.State() != Connected || client.SecureChannel() != channel }, 100*time.Millisecond, time.Millisecond)
	require.NoError(t, client.UpdateNamespaces(ctx))
	require.Same(t, session, client.Session())
	peer.mu.Lock()
	defer peer.mu.Unlock()
	require.Equal(t, 1, peer.callCount)
	require.Equal(t, 1, peer.channels)
}

func TestPublishSecureChannelFaultStillDisconnects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := newResumePeer(t, ctx, resumePeerConfig{publishFault: ua.StatusBadSecureChannelIDInvalid, publishFaultCount: 1})
	client, err := NewClient(peer.endpoint, AutoReconnect(false), RequestTimeout(time.Second))
	require.NoError(t, err)
	require.NoError(t, client.Connect(ctx))
	defer client.Close(context.Background())
	_, err = client.Subscribe(ctx, &SubscriptionParameters{Interval: 10 * time.Millisecond}, make(chan *PublishNotificationData, 8))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return client.State() != Connected }, time.Second, time.Millisecond)
}
