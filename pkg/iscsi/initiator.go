package iscsi

import (
	"crypto/rand"
	"strings"
)

// SanitizeIQNAlias keeps only the alphanumeric characters allowed in an
// iSCSI alias component.
func SanitizeIQNAlias(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IQNAlias derives a bounded, collision-resistant initiator alias from the
// node ID: sanitized prefix plus a random suffix.
func IQNAlias(nodeID string) string {
	base := SanitizeIQNAlias(nodeID)
	if base == "" {
		base = "initiator"
	}
	const (
		maxAliasLen  = 30
		randomSuffix = 8
	)
	if len(base) > maxAliasLen-randomSuffix {
		base = base[:maxAliasLen-randomSuffix]
	}
	buf := make([]byte, randomSuffix)
	if _, err := rand.Read(buf); err != nil {
		return base
	}
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := range buf {
		buf[i] = charset[int(buf[i])%len(charset)]
	}
	return base + string(buf)
}
