package opcua

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

func TestPublishTimeoutHasKeepAliveMargin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval time.Duration
		count    uint32
		want     time.Duration
	}{
		{"field idle", 50 * time.Millisecond, 3000, 300 * time.Second},
		{"short keepalive", 10 * time.Millisecond, 10, 1100 * time.Millisecond},
		{"zero revised count", time.Millisecond, 0, time.Second},
		{"capped", time.Second, math.MaxUint32, uasc.MaxTimeout},
		{"duration overflow", time.Duration(math.MaxInt64), math.MaxUint32, uasc.MaxTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subscription{c: &stubClient{}, RevisedPublishingInterval: tt.interval, RevisedMaxKeepAliveCount: tt.count}
			require.Equal(t, tt.want, s.publishTimeout())
		})
	}
}

func TestSubscriptionLifetimeBadTimeoutIsStillDelivered(t *testing.T) {
	client, err := NewClient("opc.tcp://unused:0")
	require.NoError(t, err)
	notifs := make(chan *PublishNotificationData, 1)
	sub := &Subscription{SubscriptionID: 1, Notifs: notifs}
	// A Good Publish carrying subscription expiry is not a request timeout.
	client.notifySubscription(context.Background(), sub, &ua.NotificationMessage{NotificationData: []*ua.ExtensionObject{
		ua.NewExtensionObject(&ua.StatusChangeNotification{Status: ua.StatusBadTimeout}),
	}})
	require.Equal(t, ua.StatusBadTimeout, (<-notifs).Value.(*ua.StatusChangeNotification).Status)
}
