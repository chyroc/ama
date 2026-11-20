package core

import (
	"context"
	"testing"
	"time"
)

func TestRevisionPurgerAcceptsNarrowPurgeFunction(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	purger := NewRevisionPurger(RevisionPurgerConfig{
		Every: 2 * time.Millisecond,
		Purge: func(_ context.Context, keep int) (int64, error) {
			calls++
			if keep != 3 {
				t.Fatalf("unexpected keep: %d", keep)
			}
			if calls >= 2 {
				cancel()
			}
			return 1, nil
		},
		Keep: 3,
	})
	if purger == nil {
		t.Fatal("narrow purge function should be enough")
	}
	if err := purger.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("purge should run at startup and on the interval: %d", calls)
	}
}
