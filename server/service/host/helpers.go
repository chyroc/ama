package host

import (
	"errors"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// The only place to get the time when the wiring layer did not inject a clock. If tests need to
// control time, add a now func field on Service; business methods all go through this, so there is one place to change.
func timeNow() time.Time { return time.Now() }

// isHostNotFound is the one check for "does not exist".
//
// ⚠️ It must accept both sentinels: this package's ErrNotFound wraps persistmodel.ErrNotFound
// (see the definition in service.go; the HTTP layer's fail uses it), but the Store implementation returns
// the **bare** persistmodel.ErrNotFound (store_pg_host.go returns it directly). errors.Is
// does not match backwards along the wrap chain: if we only check this package's sentinel, the bare error
// from the storage layer is treated as a "DB failure",
// so "not bound / machine does not exist" wrongly takes the degrade-and-allow path instead of failing clearly.
func isHostNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, persistmodel.ErrNotFound)
}

func errAttr(err error) slog.Attr { return slog.Any("err", err) }
func slogCallID(id string) slog.Attr {
	return slog.String("call", id)
}
func slogTermID(id string) slog.Attr {
	return slog.String("term", id)
}
func slogFrameType(t string) slog.Attr {
	return slog.String("frame", t)
}
