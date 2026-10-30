// Public shape of user preferences.

package apimodel

// UserPreferences is one person's preferences, used directly as the HTTP response body.
//
// # Neither field has omitempty
//
// This is one more case of the hard rule at the top of model/apimodel, and getting it wrong here has a concrete cost:
// omitempty would drop the whole `"language": ""` key (empty string is the zero value), so the frontend gets
// undefined, `prefs.language` becomes undefined, `MESSAGES[undefined]` is undefined, and
// **every UI string falls back to the translation key itself** (t() falls back to returning the key as is).
// The page shows no error; it is just full of strings like "nav.overview".
//
// The Service makes sure both fields are always valid enum values (normalize is the fallback), so the empty
// string should never appear in the first place, but the contract still requires the keys to be present.
type UserPreferences struct {
	// Language is the UI language, zh-CN / en-US.
	Language string `json:"language"`
	// Appearance is the appearance, light / dark / system.
	Appearance string `json:"appearance"`
}
