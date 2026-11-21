package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestSendEventsThroughWindowRetriesOnlyTransient(t *testing.T) {
	calls := 0
	transient := errors.New("requires_action")
	got, err := SendEventsThroughWindow(context.Background(), func() (int, error) {
		calls++
		if calls < 3 {
			return 0, transient
		}
		return 7, nil
	}, func(err error) bool { return errors.Is(err, transient) },
		time.Second, time.Millisecond, "sesn-example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || got != 7 || calls != 3 {
		t.Fatalf("unexpected retry result: value=%d calls=%d err=%v", got, calls, err)
	}
}

func TestSendEventsThroughWindowDoesNotRetryPermanent(t *testing.T) {
	permanent := errors.New("bad request")
	calls := 0
	_, err := SendEventsThroughWindow(context.Background(), func() (int, error) {
		calls++
		return 0, permanent
	}, func(error) bool { return false }, time.Second, time.Millisecond,
		"sesn-example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("permanent errors must return immediately: calls=%d err=%v", calls, err)
	}
}
