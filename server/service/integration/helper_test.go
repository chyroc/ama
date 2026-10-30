package integration

import (
	"context"
	"log/slog"
)

// discardLogger is a silent logger for the cases.
//
// The definition with the same name on the core side is in credentialinject_test.go (that file was
// not moved), so one is added here: test helpers should follow the cases that use them.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// integrationActor is the logged-in identity in tests.
//
// **Use a real-looking email**: downstream code depends on the actor being an email (providers
// normalize it into a platform-side id), and "a" / "b" cannot test that normalization. The constant
// with the same name on the core side is still in integration_test.go (those cases need the Handler
// and were not moved).
const integrationActor = "alice@example.com"

// fakeTokenSource is a programmable source of person JWTs.
type fakeTokenSource struct {
	token string
	err   error
}

func (f *fakeTokenSource) PersonToken(ctx context.Context, actor string) (string, error) {
	return f.token, f.err
}
