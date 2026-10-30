// Pagination shape of the product suggestion list.
//
// Moved from core so the orchestration layer's signatures do not need to import core. Fields and json tags are unchanged.

package apimodel

// SuggestionPage is the response shape of the list endpoint.
//
// It carries the page receipt and ViewerIsStaff, same idea as FeedbackPage: callers (the frontend and
// data-pulling scripts) need to know which page they got and which UI to render.
type SuggestionPage struct {
	// Suggestions is non-nil; when empty it is [] (the frontend and scripts iterate it directly).
	Suggestions []Suggestion `json:"suggestions"`
	Limit       int          `json:"limit"`
	Offset      int          `json:"offset"`
	// ViewerIsStaff says whether the caller is platform staff.
	//
	// **The server provides it; the frontend does not guess**: the frontend cannot see the allowlist, and
	// guessing gives "there is a status button in the UI, and clicking it returns 403". No omitempty: false
	// is a meaningful value,
	// and a missing key would make the frontend handle undefined as a third case besides false.
	ViewerIsStaff bool `json:"viewerIsStaff"`
}
