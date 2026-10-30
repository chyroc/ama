// orderedMap is an **order-preserving** JSON object.
//
// # Why not map[string]any
//
// Go's encoding/json **sorts keys by name** when serializing a map, while the readability of an OpenAPI document depends on
// declaration order: openapi / info / servers / paths reads well in this order, and alphabetical order would become
// components / info / openapi / paths / servers, with info in the middle.
//
// More importantly, the methods under paths: get / post / delete in alphabetical order is delete / get / post,
// putting "delete" before "query", which reads awkwardly.
//
// # Determinism is the precondition for reconciliation
//
// The real reason this file exists is that **the output must be byte-for-byte deterministic**: the spec is stored as a file in git, and reconciliation relies on
// "an empty diff after regenerating". If serialization order had any randomness, that reconciliation would fail forever,
// and the failure message would point to "the spec is outdated", not pointing to the real cause at all (the order is random).
//
// Map sorting is actually deterministic too, so it would not break reconciliation; switching to orderedMap is purely for readability.
// But once order preservation is done, it must be done thoroughly: anywhere that falls back to a map changes the order of that level.

package openapi

import (
	"bytes"
	"encoding/json"
)

// orderedMap is a JSON object serialized in insertion order.
//
// When the same key is set again it **keeps the position of the first insertion and updates the value**, so in pathsObject
// "create the path item first, then set methods one by one" does not change field order because of different method counts.
type orderedMap struct {
	keys   []string
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{values: map[string]any{}}
}

// set writes a key. If it already exists, only the value is updated, not the order.
func (m *orderedMap) set(key string, value any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// get gets a key. The second return value indicates presence.
func (m *orderedMap) get(key string) (any, bool) {
	v, ok := m.values[key]
	return v, ok
}

// len is the number of keys. Used to judge "is this container empty": an empty container should not be output as `{}`,
// which would add a components.parameters with no content to the spec.
func (m *orderedMap) len() int {
	return len(m.keys)
}

// MarshalJSON outputs in insertion order.
func (m *orderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		// **It must start its own Encoder and turn off HTML escaping**; json.Marshal cannot be used:
		// the latter unconditionally turns < > & into < and the like, while descriptions have literals like `<token>`,
		// which would be unreadable after escaping. The SetEscapeHTML(false) set by the top-level Generate does not reach here:
		// that switch only affects its own Encode call, and nested Marshalers do not inherit it.
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(m.values[key]); err != nil {
			return nil, err
		}
		// Encode adds a trailing newline; remove it, since this is inside an object, not the end of a line.
		buf.Write(bytes.TrimRight(vb.Bytes(), "\n"))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
