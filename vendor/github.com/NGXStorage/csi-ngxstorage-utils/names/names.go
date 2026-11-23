package names

import (
	"fmt"
	"strings"
)

// ModifyVolName applies the NGX Storage CSI name normalization contract.
// It removes configured separators, restores the PVC prefix by default, and
// limits the result to 30 characters while preserving a deterministic suffix.
func ModifyVolName(name string, withoutPVCPrefix ...bool) string {
	name = strings.NewReplacer("-", "", "_", "", ".", "", "/", "", ":", "", " ", "").Replace(name)
	if !strings.Contains(name, "pvc") && !(len(withoutPVCPrefix) != 0 && withoutPVCPrefix[0]) {
		name = "pvc" + name
	}
	if len(name) > 30 {
		name = name[:22] + hashNameSuffix(name)
	}
	return name
}

func hashNameSuffix(name string) string {
	h := uint32(2166136261)
	for i := 0; i < len(name); i++ {
		h = (h ^ uint32(name[i])) * 16777619
	}
	return fmt.Sprintf("%06x", h)
}
