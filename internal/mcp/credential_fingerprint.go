package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// CredentialFingerprint identifies a set of credential values without retaining
// them, so an observation can record WHICH material made it safe rather than
// keeping a second long-lived copy of that material.
//
// Order-independent, because the token store enumerates a map. Length-prefixed,
// because ["ab","c"] and ["a","bc"] would otherwise produce the same digest and
// a rotation between those two shapes would read as no change at all.
func CredentialFingerprint(values []string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	digest := sha256.New()
	for _, value := range sorted {
		fmt.Fprintf(digest, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
