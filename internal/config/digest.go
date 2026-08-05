package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// digest hashes the configuration's canonical JSON encoding.
//
// JSON rather than the source YAML: encoding/json emits struct fields in
// declaration order and sorts map keys, so the encoding is canonical without a
// separate sorting pass, and cosmetic YAML edits — key order, comments,
// whitespace — cannot change the result. The digest therefore tracks what the
// configuration means, not how it was typed.
func digest(cfg Config) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encoding configuration for digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
