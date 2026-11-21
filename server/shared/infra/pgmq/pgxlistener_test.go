package pgmq

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDialPGXRequiresUsableDSN(t *testing.T) {
	_, err := DialPGX("invalid-dsn")(context.Background())
	if err == nil {
		t.Fatal("invalid DSN must fail instead of returning a usable listener")
	}
}

// The concrete pgx.Listener methods remain covered by the queue integration tests.
var _ Listener = pgxListener{conn: (*pgx.Conn)(nil)}
