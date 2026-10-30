// Encoding of the addons parameter for the authorization page.
//
// The device code begin step can only request a fixed set of basic permissions. To have extra
// permissions or callbacks checked on the authorization page, they must be encoded into the addons
// query parameter of the authorization page URL:
//
//	JSON  →  gzip  →  base64 (URL-safe, no padding)
//
// This encoding is defined by the Feishu registration protocol; it is not our choice.
//
// A handy property: Go's gzip.Writer writes zero for the header mtime, so the encoding of the same
// input is byte-for-byte deterministic, and unit tests can assert the final string directly.
// (Python's gzip embeds the current timestamp, so expected values generated with it change every
// time.)

package lark

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// encodeAddons encodes extra permissions and callbacks into the addons parameter of the
// authorization page.
// When both are empty it returns an empty string, meaning the parameter is not needed.
func encodeAddons(tenantScopes, callbacks []string) (string, error) {
	tenantScopes = uniqueNonEmpty(tenantScopes)
	callbacks = uniqueNonEmpty(callbacks)
	if len(tenantScopes) == 0 && len(callbacks) == 0 {
		return "", nil
	}
	addons := make(map[string]any, 2)
	if len(tenantScopes) > 0 {
		addons["scopes"] = map[string][]string{"tenant": tenantScopes}
	}
	if len(callbacks) > 0 {
		addons["callbacks"] = map[string][]string{"items": callbacks}
	}
	payload, err := json.Marshal(addons)
	if err != nil {
		return "", fmt.Errorf("序列化 addons 失败: %w", err)
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("压缩 addons 失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("压缩 addons 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(compressed.Bytes()), nil
}
