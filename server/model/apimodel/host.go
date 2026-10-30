// HTTP contract entities for the host executor.
//
// Kept apart from service/host: persistmodel.Host is the stored shape (times are time.Time),
// while this is the wire shape for the frontend/daemon (times are ISO strings with exactly three
// decimals; see the NowISO/FormatISO convention in this file).
package apimodel

// Host is the public shape of one registered host.
//
// OwnerEmail is not in this struct: GET /api/hosts lists the caller's own machines, so returning
// the owner email is pointless and adds one more leak surface. The workspace binding list (WorkspaceHost) may
// contain machines bound by **someone else**,
// so it needs to show the owner; OwnerEmail is added at that level.
type Host struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Status     string `json:"status"`
	Online     bool   `json:"online"`
	GNUToolsOK bool   `json:"gnuToolsOk"`
	Toolcheck  string `json:"toolcheck,omitempty"`
	// NativeToolsOK means the daemon supports the native tool protocol (brew GNU tools no longer needed).
	NativeToolsOK bool   `json:"nativeToolsOk"`
	DaemonVersion string `json:"daemonVersion,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastSeenAt    string `json:"lastSeenAt,omitempty"`
}

// WorkspaceHost is a workspace binding (a row on the binding page needs both the machine and the binding info).
type WorkspaceHost struct {
	Host
	// OwnerEmail is the lowercase email of the machine owner (the registrant). In the two-level model a workspace
	// admin can assign machines bound by others to an agent, so the frontend needs it to tell apart machines with the
	// same alias and to label "whose machine".
	// Member emails are already visible in member management, so this matches that; GET /api/hosts does not return it.
	OwnerEmail string `json:"ownerEmail,omitempty"`
	// AddedByEmail is who bound this machine into the workspace (lowercase email); it may not be the same person
	// as OwnerEmail (machine registrant vs binding operator). For old bindings without this column it is an empty
	// string, which the frontend shows as "not recorded".
	AddedByEmail string `json:"addedByEmail,omitempty"`
	// BoundAt is when it was bound into the workspace.
	BoundAt string `json:"boundAt,omitempty"`
}
