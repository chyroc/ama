package core

import (
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// NamedCredentialRecord is a provider-neutral stored credential. Secret is plaintext only
// inside the server; the Store implementation owns encryption at rest. HTTP responses use
// View, which never includes Secret.
type NamedCredentialRecord struct {
	ID                string
	Actor             string
	Name              string
	PublicID          string
	Secret            string
	SecretMasked      string
	Site              string
	LastVerifiedAt    time.Time
	LastVerifyError   string
	LastVerifyLimited bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NamedCredentialVerification contains no secret or provider client. Limited is a valid
// credential with a narrower permission scope; Unknown is not evidence of a bad credential.
type NamedCredentialVerification struct {
	Valid   bool
	Unknown bool
	Limited bool
	Reason  string
}

func (v NamedCredentialVerification) ViewStatus() string {
	switch {
	case v.Valid && v.Limited:
		return apimodel.CredentialStatusLimited
	case v.Valid:
		return apimodel.CredentialStatusOK
	case v.Unknown:
		return apimodel.CredentialStatusUnknown
	default:
		return apimodel.CredentialStatusError
	}
}

func (r NamedCredentialRecord) StoredVerification() NamedCredentialVerification {
	if r.LastVerifyError != "" {
		return NamedCredentialVerification{Reason: r.LastVerifyError}
	}
	if !r.LastVerifiedAt.IsZero() {
		return NamedCredentialVerification{Valid: true, Limited: r.LastVerifyLimited}
	}
	return NamedCredentialVerification{Unknown: true, Reason: "还没验证过"}
}

func (r NamedCredentialRecord) View(v NamedCredentialVerification) apimodel.IntegrationCredentialView {
	view := apimodel.IntegrationCredentialView{
		ID: r.ID, Name: r.Name, PublicID: r.PublicID, SecretMasked: r.SecretMasked,
		Status: v.ViewStatus(), StatusReason: v.Reason,
		CreatedAt: apimodel.FormatISO(r.CreatedAt), Actor: r.Actor,
	}
	if !r.LastVerifiedAt.IsZero() {
		view.LastVerifiedAt = apimodel.FormatISO(r.LastVerifiedAt)
	}
	if v.Valid {
		view.StatusReason = ""
	}
	return view
}
