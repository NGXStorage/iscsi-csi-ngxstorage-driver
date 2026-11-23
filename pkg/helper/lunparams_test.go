package helper

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLUNCreateOptionsFromParams(t *testing.T) {
	opts, err := LUNCreateOptionsFromParams(map[string]string{
		"blocksize":      "128k",
		"deduplication":  "on",
		"compression":    "on",
		"thin_provision": "on",
		"io_type":        "sequential",
		"qos_priority":   "64",
	})
	if err != nil {
		t.Fatalf("valid create params: %v", err)
	}
	if opts.Blocksize != "128k" {
		t.Fatalf("blocksize = %q, want 128k", opts.Blocksize)
	}
	if opts.Deduplication != "on" {
		t.Fatalf("deduplication = %q, want on", opts.Deduplication)
	}
	if opts.Compression != "on" {
		t.Fatalf("compression = %q, want on", opts.Compression)
	}
	if opts.ThinProvision != "on" {
		t.Fatalf("thin_provision = %q, want on", opts.ThinProvision)
	}
	if opts.IoType != "sequential" {
		t.Fatalf("io_type = %q, want sequential", opts.IoType)
	}
	if opts.QosPriority != 64 {
		t.Fatalf("qos_priority = %d, want 64", opts.QosPriority)
	}
}

func TestLUNCreateOptionsFromParamsEmpty(t *testing.T) {
	opts, err := LUNCreateOptionsFromParams(nil)
	if err != nil {
		t.Fatalf("empty params: %v", err)
	}
	if opts.Blocksize != "" || opts.Deduplication != "" || opts.QosPriority != 0 {
		t.Fatalf("empty params should produce zero options: %+v", opts)
	}
}

// Create parameters are validated at create time: a silently dropped or
// malformed value would provision a LUN that does not match the SC contract.
func TestLUNCreateOptionsFromParamsValidation(t *testing.T) {
	valid := []map[string]string{
		{"blocksize": "128k"},
		{"blocksize": "auto"},
		{"blocksize": "131072"},
		{"deduplication": "true", "compression": "0", "thin_provision": "off"},
		{"qos_priority": "4"},
		{"qos_priority": "128"},
		{"io_type": "transactional"},
		// Driver-consumed pass-through keys, not backend LUN create fields.
		{"fstype": "xfs", "mount_permissions": "0750"},
		// Empty values keep the backend defaults.
		{"blocksize": "", "deduplication": "", "io_type": "", "qos_priority": ""},
	}
	for _, params := range valid {
		if _, err := LUNCreateOptionsFromParams(params); err != nil {
			t.Fatalf("params %v should be accepted, got %v", params, err)
		}
	}
	invalid := []map[string]string{
		{"blocksize": "abc"},
		{"qos_priority": "3"},
		{"qos_priority": "abc"},
		{"deduplication": "maybe"},
		{"compression": "yes"},
		{"thin_provision": "2"},
		{"io_type": "random"},
		{"foo": "bar"},
	}
	for _, params := range invalid {
		if _, err := LUNCreateOptionsFromParams(params); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("params %v should be rejected, got %v", params, err)
		}
	}
}

func TestLUNCreateOptionsFromParamsNormalizesBooleans(t *testing.T) {
	opts, err := LUNCreateOptionsFromParams(map[string]string{
		"deduplication":  "TRUE",
		"compression":    "1",
		"thin_provision": "Off",
	})
	if err != nil {
		t.Fatalf("boolean normalization: %v", err)
	}
	if opts.Deduplication != "on" || opts.Compression != "on" || opts.ThinProvision != "off" {
		t.Fatalf("normalized options = %+v, want on/on/off", opts)
	}
}

func TestNormalizeLUNMutableParameters(t *testing.T) {
	out, err := NormalizeLUNMutableParameters(map[string]string{
		"deduplication":  "on",
		"compression":    "off",
		"thin_provision": "on",
		"flash_cache":    "off",
		"dram_cache":     "on",
		"io_type":        "transactional",
		"qos_priority":   "32",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out["deduplication"] != "on" {
		t.Fatalf("deduplication = %v, want on", out["deduplication"])
	}
	if out["compression"] != "off" {
		t.Fatalf("compression = %v, want off", out["compression"])
	}
	if out["qos_priority"] != "32" {
		t.Fatalf("qos_priority = %v, want 32", out["qos_priority"])
	}
}

func TestNormalizeLUNMutableParametersRejectsImmutable(t *testing.T) {
	_, err := NormalizeLUNMutableParameters(map[string]string{"blocksize": "4k"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("blocksize should be rejected as immutable, got %v", err)
	}
}

func TestNormalizeLUNMutableParametersRejectsBadValues(t *testing.T) {
	cases := []map[string]string{
		{"deduplication": "maybe"},
		{"io_type": "random"},
		{"qos_priority": "99"},
		{"": "x"},
	}
	for _, in := range cases {
		if _, err := NormalizeLUNMutableParameters(in); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("input %v should be rejected, got %v", in, err)
		}
	}
}
