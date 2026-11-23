package ngxstorage

import "regexp"

var canonicalVolumeIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,40}$`)

// IsCanonicalVolumeID reports whether id matches the volume-ID format
// accepted by the NGX API v2 volume_id filters.
func IsCanonicalVolumeID(id string) bool {
	return canonicalVolumeIDPattern.MatchString(id)
}
