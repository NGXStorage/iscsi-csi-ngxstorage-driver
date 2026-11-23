package iscsi

import (
	"fmt"
	"strconv"
	"strings"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
)

// TargetIQN returns the backend iSCSI target IQN.
func TargetIQN(target *ngxstorage.ISCSITarget) string {
	if target == nil {
		return ""
	}
	return target.TargetIQN
}

// TargetLUNNumber parses a non-negative LUN mapping number.
func TargetLUNNumber(target *ngxstorage.ISCSITarget, lunID string) (int, error) {
	for _, lun := range target.Luns {
		if lun.ID == lunID {
			n, err := strconv.Atoi(strings.TrimSpace(lun.Number))
			if err != nil {
				return -1, fmt.Errorf("parse LUN number %q: %w", lun.Number, err)
			}
			if n < 0 {
				return -1, fmt.Errorf("parse LUN number %q: must be non-negative", lun.Number)
			}
			return n, nil
		}
	}
	return -1, fmt.Errorf("LUN %s not mapped to target %s", lunID, target.ID)
}
