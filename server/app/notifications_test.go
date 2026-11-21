package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type notificationTestConnection struct {
	notifications []*pgconn.Notification
	index         int
	waitErr       error
	closed        chan struct{}
	closeOnce     sync.Once
}

func (c *notificationTestConnection) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (c *notificationTestConnection) WaitForNotification(context.Context) (*pgconn.Notification, error) {
	if c.index < len(c.notifications) {
		notification := c.notifications[c.index]
		c.index++
		return notification, nil
	}
	if c.waitErr != nil {
		return nil, c.waitErr
	}
	return nil, context.Canceled
}
func (c *notificationTestConnection) Close(context.Context) error {
	c.closeOnce.Do(func() {
		if c.closed != nil {
			close(c.closed)
		}
	})
	return nil
}

func TestRunNotificationListenerReconnectsAndFiltersChannel(t *testing.T) {
	firstClosed := make(chan struct{})
	secondClosed := make(chan struct{})
	connections := []*notificationTestConnection{
		{notifications: []*pgconn.Notification{{Channel: "other", Payload: "ignore"}}, waitErr: errors.New("lost connection"), closed: firstClosed},
		{notifications: []*pgconn.Notification{{Channel: "wake_channel", Payload: "host_example"}}, closed: secondClosed},
	}
	calls := 0
	wake := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := RunNotificationListener(ctx, NotificationListenerConfig{
			Channel: "wake_channel", RetryDelay: time.Millisecond,
			Connect: func(context.Context) (NotificationConnection, error) {
				conn := connections[calls]
				calls++
				return conn, nil
			},
			Wake: func(id string) { wake <- id; cancel() },
		}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case got := <-wake:
		if got != "host_example" {
			t.Fatalf("Wake payload = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not reconnect and deliver the notification")
	}
	<-firstClosed
	<-secondClosed
	if calls != 2 {
		t.Fatalf("Connect calls = %d, want one per reconnect", calls)
	}
}
