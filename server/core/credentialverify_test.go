package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestVerifyCredentialViewsPreservesOrderAndContainsPanic(t *testing.T) {
	views := VerifyCredentialViews(context.Background(), 3, func(_ context.Context, index int) apimodel.IntegrationCredentialView {
		if index == 1 {
			panic("one verifier failed")
		}
		return apimodel.IntegrationCredentialView{ID: "credential_" + string(rune('a'+index)), Status: apimodel.CredentialStatusOK}
	}, func(index int) apimodel.IntegrationCredentialView {
		return apimodel.IntegrationCredentialView{ID: "fallback", Status: apimodel.CredentialStatusUnknown}
	}, nil)
	if len(views) != 3 || views[0].ID != "credential_a" || views[1].Status != apimodel.CredentialStatusUnknown || views[2].ID != "credential_c" {
		t.Fatalf("unexpected verification views: %#v", views)
	}
}

func TestVerifyCredentialViewsEmptyReturnsNonNil(t *testing.T) {
	if views := VerifyCredentialViews(context.Background(), 0, nil, nil, nil); views == nil || len(views) != 0 {
		t.Fatalf("empty views = %#v, want non-nil empty slice", views)
	}
}
