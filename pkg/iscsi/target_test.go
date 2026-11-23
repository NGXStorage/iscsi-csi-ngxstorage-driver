package iscsi

import (
	"encoding/json"
	"testing"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
)

func TestTargetLUNNumberRejectsMalformedAndNegativeValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
		fail bool
	}{
		{name: "valid with whitespace", raw: " 12 ", want: 12},
		{name: "suffix is not silently accepted", raw: "12x", fail: true},
		{name: "negative is rejected", raw: "-1", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var target ngxstorage.ISCSITarget
			payload, _ := json.Marshal(map[string]any{
				"id": "target-1", "luns": []map[string]string{{"id": "lun-1", "number": tt.raw}},
			})
			if err := json.Unmarshal(payload, &target); err != nil {
				t.Fatal(err)
			}
			got, err := TargetLUNNumber(&target, "lun-1")
			if tt.fail {
				if err == nil {
					t.Fatalf("TargetLUNNumber(%q) = %d, want error", tt.raw, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("TargetLUNNumber(%q) = %d, %v; want %d", tt.raw, got, err, tt.want)
			}
		})
	}
}
