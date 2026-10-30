// The integration management (Integrations) domain: a provider registry + a four-state status model
// + two integration sources whose "status is queried live and not stored".
//
// # Why this package does not include Feishu user authorization
//
// The sources fall into two kinds, decided by **whether the credential is stored in our own
// database**:
//
//	not stored (this package)  ama
//	stored (still in core)     lark-user
//
// The former's credential authority is on the platform side (see "Status is always queried live" in
// the file header of integration.go), so it **needs not a single Store method**, and this package
// therefore has zero dependency on core.Store. lark-user holds a refresh_token and must move together
// with its Store declarations, which is the next step.
//
// It is split along this line rather than by file length because this line is also exactly the
// boundary of the **conflict surface**: core/store.go is a file every domain goes through and changes
// most often.
//
// # ⚠️ This package handles credentials; two iron rules
//
//  1. **Plain-text credentials never go into logs, error messages, or any field of
//     IntegrationState** (including the two free-text fields Summary / FixHint; the easiest to miss
//     is "putting the upstream error into Summary on the way", and upstream errors may carry
//     credentials). To show one, it must go through maskCredential.
//  2. **Plain-text credentials never go into storemodel.** The precedent is ArkBindingKey: it has a
//     plain-text APIKey field, so it is deliberately kept out of storemodel: "putting it in a package
//     called storemodel would bake the wrong hint 'plain-text keys belong to the storage layer' into
//     the package name".
//
// The full domain knowledge (the four-state model, how many places to change to add a source, the
// traps of each source) is in core/README-integrations.md, which did not move: it covers the whole
// domain, and the domain now spans two packages.
package integration

import "github.com/chyroc/ama/server/model/apimodel"

// nowISO is the only entry point for timestamps produced by this domain: a UTC ISO string with
// exactly three decimals.
//
// ⚠️ **Do not change it to time.RFC3339**: that drops `.000`, so the same moment has two spellings in
// the database/contract, and when sorting as strings `.`(0x2E) < `Z`(0x5A), so mixing the two shapes
// changes the sort result directly. See "Timestamps: unit and format" in CLAUDE.md.
//
// The core side has a function with the same name and implementation (core/service.go); both just
// forward to apimodel.NowISO, so there are not "two clocks".
func nowISO() string { return apimodel.NowISO() }
