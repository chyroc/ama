package core

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// VerifyCredentialViews concurrently verifies a bounded credential collection while
// preserving input order. Verify receives only an index, so plaintext credentials and
// provider-specific verification results remain in the downstream implementation.
func VerifyCredentialViews(
	ctx context.Context, count int,
	verify func(context.Context, int) apimodel.IntegrationCredentialView,
	panicView func(int) apimodel.IntegrationCredentialView,
	logger *slog.Logger,
) []apimodel.IntegrationCredentialView {
	views := make([]apimodel.IntegrationCredentialView, count)
	if count == 0 {
		return views
	}
	if logger == nil {
		logger = slog.Default()
	}
	if verify == nil {
		for i := range views {
			if panicView != nil {
				views[i] = panicView(i)
			} else {
				views[i].Status = apimodel.CredentialStatusUnknown
			}
		}
		return views
	}
	done := make(chan struct{}, count)
	for i := 0; i < count; i++ {
		go func(index int) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.ErrorContext(ctx, "验证凭据时 panic", slog.Any("panic", recovered))
					if panicView != nil {
						views[index] = panicView(index)
					} else {
						views[index].Status = apimodel.CredentialStatusUnknown
					}
				}
				done <- struct{}{}
			}()
			views[index] = verify(ctx, index)
		}(i)
	}
	for range views {
		<-done
	}
	return views
}
