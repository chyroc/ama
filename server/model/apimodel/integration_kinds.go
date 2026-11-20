package apimodel

import "strings"

// RegisterServiceAccountIntegration declares a downstream provider whose credential represents
// an application rather than a person. Registrations are additive; there is deliberately no way
// to turn an existing source back into personal authorization during process startup.
func RegisterServiceAccountIntegration(id string) {
	if id = strings.TrimSpace(id); id != "" {
		serviceAccountIntegrations[id] = true
	}
}
