// Package feishumodel holds storage-layer constants and pure functions **shared by both Store implementations** of the feishu module.
//
// # Why a separate package
//
// The feishu PG implementation is in repository/dao/feishudao; MemStore is in the feishu package. Both must
// use **the same** status literals and the same sort/trim rules, otherwise they drift apart, and drift does not show as an error;
// it shows as "works fine in memory, behaves differently in production", the hardest kind of problem to track down.
//
// It lives here and not in model/persistmodel: that package holds core's storage types (RunQuery,
// FeedbackRecord and so on) and has nothing to do with feishu. Each module's storage layer gets its own package,
// so that adding something on either side does not mean touching a big shared package.
//
// # Scope
//
// Only things that "both Store implementations need and that depend on no type from the feishu package" go here.
// The test is: **can it compile without importing feishu**. Business rules (scopeForMessage,
// deriveStatus and the like) stay out: they have only one implementation, so there is no drift risk, and they stay in feishu.
package feishumodel

import "time"

// Processing result of an inbound message, written into messageBinding.Status.
//
// ⚠️ **These three are literals stored in the database**; changing them changes the data format: existing rows keep the old values,
// and querying by the new values finds nothing (no error, just "all the messages suddenly vanished").
const (
	// InboundPending is being processed.
	InboundPending = "pending"
	// InboundProcessed is done.
	InboundProcessed = "processed"
	// InboundFailed failed; retried on redelivery.
	InboundFailed = "failed"
)

// Claim status of an unfinished turn, written into InflightTurnRecord.Status.
//
// **It is a different thing from conversation status**: how far the conversation got is driven by Ark events in the runs table; these
// two values only make the startup scans of multiple instances mutually exclusive. Every instance scans, and without CAS two instances would
// write to the same card at once, and the user would see the content jump back and forth.
const (
	// InflightOpen is unfinished and can be claimed.
	InflightOpen = "inflight"
	// InflightResuming has been claimed by some instance and is being resumed.
	InflightResuming = "resuming"
)

// SessionTTL is how long a registration session lives.
//
// Both Store implementations use it as an upper bound on write (the validity Feishu gives may be longer),
// so it is a rule shared by the storage layer, not an implementation detail of one side.
const SessionTTL = 10 * time.Minute
