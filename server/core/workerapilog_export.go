package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// NewInstrumentedWorkerAPI wraps an Ark selfhosted API with method, duration,
// outcome, and status logs. Request and response bodies are never logged.
func NewInstrumentedWorkerAPI(api selfhosted.API, logger *slog.Logger) selfhosted.API {
	return newInstrumentedWorkerAPI(api, logger)
}

// ArkStatusOf returns the HTTP status carried by an Ark API error. A deadline
// has a request-timeout placeholder; unrelated errors return zero.
func ArkStatusOf(err error) int {
	var apiErr *selfhosted.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusRequestTimeout
	}
	return 0
}
