// This file does only one thing: lets the 33 kinds of session events be filtered and sorted, while returning them to the frontend **as is**.
//
// # Why there are no Go structs
//
// Ark defines 34 event variants (7 client upstream kinds + 26 server push kinds; the frontend comment says "33 kinds",
// but the union actually has 34 members). They share one envelope (id / type / processed_at /
// session_thread_id), but their payloads differ a lot; even the **position** of the same key differs between variants
// (in user.tool_confirmation result comes before tool_use_id; in span.outcome_evaluation_end
// result is in the middle).
//
// And in this round of the contract events are **read-only**: there is only listRunEvents, no writes at all. Since the server does not need to
// understand the payload, there is no reason to build 34 parallel structs: that would only bring mismatches (one wrong omitempty key changes the shape),
// and would need maintenance with each Ark upgrade. So events are stored and returned as json.RawMessage as is, decoding only the
// three fields needed for filtering.
//
// This matches the existing convention of shared/infra/ma: "return values are always the SDK's native structs, no second mapping"
// (see "Design trade-offs" in server/shared/infra/ma/README.md).
//
// # Passing through as is must avoid c.JSON
//
// Hertz's c.JSON follows standard library behavior and escapes < > & (into < > &), while JS's
// JSON.stringify **does not escape**. Tested: the same RawMessage comes out changed after c.JSON.
// Skill bodies are user-editable Markdown and will contain these three characters sooner or later. So the array must be built
// by hand with AppendJSONArray, then sent with c.Data.

package apimodel

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// RunEvent is one session event: the raw JSON plus the three fields needed for filtering.
//
// Raw is the authoritative data; the other fields are only indexes decoded from Raw and **take no part in serialization**
// (all have json:"-"): only Raw is emitted on return, guaranteeing byte-for-byte equality with Ark/the frontend.
type RunEvent struct {
	Raw json.RawMessage `json:"-"`

	// Type is the event type, like agent.tool_use, used for the types allowlist filter.
	Type string `json:"-"`
	// Sequence is the monotonically increasing sequence number within the session, used for sorting (the list endpoint sorts ascending by it).
	Sequence int `json:"-"`
	// ThreadID maps to session_thread_id, used for filtering by thread.
	// It is a pointer because null has meaning: session-level events (session.status_*) have null for this field.
	ThreadID *string `json:"-"`
	// EventID is the event id issued by Ark (sevt-... / call_... / evt-...), used for **deduplication**.
	//
	// One Ark session can have several subscribers at once, and each writes a copy of every frame it receives into the database;
	// sequence is issued fresh by the Store each time, so the same event takes two different sequence numbers and both rows get stored happily.
	// This field is the basis of that unique index; see run_events_event_id_idx in repository/schema/core_pg.sql.
	//
	// It may be empty: frames that cannot be decoded (the fallback branch of NewRunEventFromArk below) have no id,
	// and such frames are not deduplicated.
	EventID string `json:"-"`
}

// eventEnvelope is the minimal struct used to decode index fields from the raw event.
type eventEnvelope struct {
	Type     string  `json:"type"`
	Sequence int     `json:"sequence"`
	ThreadID *string `json:"session_thread_id"`
	ID       string  `json:"id"`
}

// UnmarshalJSON keeps the raw bytes and decodes the index fields at the same time.
func (e *RunEvent) UnmarshalJSON(data []byte) error {
	var env eventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	// Make a copy: the data passed in by the json package may be reused after return.
	e.Raw = append(json.RawMessage(nil), data...)
	e.Type = env.Type
	e.Sequence = env.Sequence
	e.ThreadID = env.ThreadID
	e.EventID = env.ID
	return nil
}

// MarshalJSON emits the stored bytes as is.
//
// Note: this only applies when the caller serializes with the standard library, and the standard library still does HTML escaping. When really
// returning to the frontend we use AppendJSONArray + c.Data, which does not go through here.
func (e RunEvent) MarshalJSON() ([]byte, error) {
	if len(e.Raw) == 0 {
		return []byte("null"), nil
	}
	return e.Raw, nil
}

// AppendJSONArray builds a set of events into the bytes of a JSON array by hand.
//
// Building by hand avoids the standard library's HTML escaping; see the comment at the top of this file. An empty slice gives [],
// not null: listRunEvents returns RunEvent[], and the frontend calls .map() directly.
func AppendJSONArray(events []RunEvent) []byte {
	// Estimate the capacity to avoid repeated growth
	size := 2
	for _, e := range events {
		size += len(e.Raw) + 1
	}
	buf := make([]byte, 0, size)
	buf = append(buf, '[')
	for i, e := range events {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, e.Raw...)
	}
	return append(buf, ']')
}

// NewRunEventFromArk fills in one raw event pushed by Ark into the shape the frontend needs.
//
// # Why fill in instead of passing through as is
//
// Tested: Ark's events (whether SSE or GET /sessions/{id}/events) **never carry sequence**:
//
//	{"id":"sevt-...","type":"agent.message","content":[...],"processed_at":"..."}
//
// But the frontend treats sequence as required: timeline.ts:497 sorts by it, useConversationStream uses it to
// assemble history, and the primary key of run_events on the PG side is (run_id, sequence). Without it JS gets
// undefined, `a.sequence - b.sequence` gives NaN, and the order of the whole timeline becomes undefined.
//
// So the server fills one in by arrival order: Ark's event stream is ordered itself, and "arrival order" is exactly
// a faithful expression of the total order within the session. It is filled in by **inserting a "sequence" before the first key of the raw text**,
// not by decoding into a map and marshaling again: the latter would reorder keys, drop fields Ark adds in the future,
// and trigger the standard library's HTML escaping (skill bodies and tool inputs will contain < > & sooner or later).
//
// # session_thread_id must be filled in too
//
// Ark does not send this key at all for session-level events (session.status_*), while the frontend's TS type is
// `session_thread_id: string | null`: **it must be present, and may be null**. A missing key makes
// destructuring in RunEventBase get undefined, and filtering by thread behaves unpredictably. So when missing, fill in null.
//
// When it cannot be filled in (not a JSON object, or already has sequence), return it as is and only decode the index fields.
func NewRunEventFromArk(raw []byte, sequence int) RunEvent {
	trimmed := bytes.TrimSpace(raw)

	var env eventEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		// If it cannot be decoded, keep it as is: losing one event is better than breaking the whole stream; the frontend ignores frames it does not recognize.
		return RunEvent{Raw: append(json.RawMessage(nil), trimmed...), Sequence: sequence}
	}

	patched := trimmed
	if env.Sequence == 0 {
		patched = injectKeys(patched, sequence, !hasKey(patched, "session_thread_id"))
		env.Sequence = sequence
	}

	return RunEvent{
		Raw:      append(json.RawMessage(nil), patched...),
		Type:     env.Type,
		Sequence: env.Sequence,
		ThreadID: env.ThreadID,
		EventID:  env.ID,
	}
}

// injectKeys inserts sequence (and optionally session_thread_id:null) at the very front of a JSON object.
//
// Pure byte splicing, no decoding or reordering: escapes, key order and whitespace in Ark's raw text all stay as is.
func injectKeys(obj []byte, sequence int, withThreadID bool) []byte {
	if len(obj) < 2 || obj[0] != '{' {
		return obj
	}

	var ins bytes.Buffer
	ins.WriteByte('{')
	ins.WriteString(`"sequence":`)
	ins.WriteString(strconv.Itoa(sequence))
	if withThreadID {
		ins.WriteString(`,"session_thread_id":null`)
	}
	// An empty object {} has no keys inside, so no comma may follow, otherwise we get {"sequence":1,}.
	if len(bytes.TrimSpace(obj[1:len(obj)-1])) > 0 {
		ins.WriteByte(',')
	}
	ins.Write(obj[1:])
	return ins.Bytes()
}

// hasKey reports whether a JSON object has a given top-level key.
//
// Decoding into map[string]json.RawMessage is only to see if the key exists, without touching values: the value bytes are still
// kept as is by the caller.
func hasKey(obj []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
