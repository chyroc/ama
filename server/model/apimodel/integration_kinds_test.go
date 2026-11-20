package apimodel_test

import (
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestRegisterServiceAccountIntegration(t *testing.T) {
	const id = "example-service-account"
	if !apimodel.IsPersonalIntegration(id) {
		t.Fatal("unknown provider must default to personal authorization")
	}
	apimodel.RegisterServiceAccountIntegration(id)
	if !apimodel.IsServiceAccountIntegration(id) {
		t.Fatal("registered downstream provider must be a service account")
	}
	if !apimodel.IsPersonalIntegration("another-unknown-provider") {
		t.Fatal("registration must not change the conservative default for other providers")
	}
}
