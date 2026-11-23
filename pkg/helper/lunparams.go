package helper

import (
	"regexp"
	"strconv"
	"strings"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// createBoolValue normalizes a boolean-ish create parameter to the backend's
// canonical "on"/"off" grammar. Empty input yields "" so the SDK omits the
// field and the backend applies its default; anything else is rejected.
func createBoolValue(key, value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", nil
	case "true", "on", "1":
		return "on", nil
	case "false", "off", "0":
		return "off", nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "create parameter %q must be a boolean", key)
	}
}

// lunBlocksizePattern matches the backend blocksize grammar: "auto", a plain
// byte count, or an Nk suffix (scenario SCs use "128k"; the backend records
// bytes and CompatibleBlockSize accepts all three forms).
var lunBlocksizePattern = regexp.MustCompile(`(?i)^(auto|[0-9]+k?)$`)

// LUNCreateOptionsFromParams maps StorageClass create parameters to SDK LUN
// create options. Canonical CSI keys: blocksize, deduplication, compression,
// thin_provision, io_type, qos_priority. Unknown keys and malformed values
// are InvalidArgument: a silently dropped parameter would provision a LUN
// that does not match the StorageClass contract. fstype and mount_permissions
// are driver-consumed pass-through keys (capability fstype and volume-context
// mount permissions), not backend LUN create fields.
func LUNCreateOptionsFromParams(params map[string]string) (ngxstorage.LUNCreateOptions, error) {
	var opts ngxstorage.LUNCreateOptions
	for rawKey, value := range params {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		switch key {
		case "blocksize":
			v := strings.TrimSpace(value)
			if v == "" {
				continue
			}
			if !lunBlocksizePattern.MatchString(v) {
				return opts, status.Errorf(codes.InvalidArgument, "create parameter %q has an unsupported value %q", key, value)
			}
			opts.Blocksize = v
		case "deduplication":
			v, err := createBoolValue(key, value)
			if err != nil {
				return opts, err
			}
			opts.Deduplication = v
		case "compression":
			v, err := createBoolValue(key, value)
			if err != nil {
				return opts, err
			}
			opts.Compression = v
		case "thin_provision":
			v, err := createBoolValue(key, value)
			if err != nil {
				return opts, err
			}
			opts.ThinProvision = v
		case "io_type":
			v := strings.TrimSpace(value)
			if v == "" {
				continue
			}
			if !lunIoTypePattern.MatchString(v) {
				return opts, status.Errorf(codes.InvalidArgument, "create parameter %q must be transactional or sequential", key)
			}
			opts.IoType = v
		case "qos_priority":
			v := strings.TrimSpace(value)
			if v == "" {
				continue
			}
			switch v {
			case "4", "8", "16", "32", "64", "128":
				// The whitelist guarantees a parseable integer.
				opts.QosPriority, _ = strconv.Atoi(v)
			default:
				return opts, status.Errorf(codes.InvalidArgument, "create parameter %q has an unsupported value %q", key, value)
			}
		case "fstype", "mount_permissions":
			// Consumed outside the LUN create payload: fstype by the
			// provisioner/capability, mount_permissions by CreateVolumeResponse
			// into the volume context.
		default:
			return opts, status.Errorf(codes.InvalidArgument, "unsupported create parameter %q", key)
		}
	}
	return opts, nil
}

// lunIoTypePattern matches the backend io_type grammar.
var lunIoTypePattern = regexp.MustCompile(`^(transactional|sequential)$`)

// NormalizeLUNMutableParameters validates a ControllerModifyVolume mutable
// parameter map for a block (LUN) volume and returns the canonical NGX PATCH
// payload. Only the LUN mutable fields (PATCH /api/v2/lun/{id}) are accepted;
// blocksize is immutable and rejected.
func NormalizeLUNMutableParameters(input map[string]string) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(input))
	for rawKey, value := range input {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" {
			return nil, status.Error(codes.InvalidArgument, "mutable parameter key is required")
		}
		switch key {
		case "deduplication", "compression", "thin_provision", "flash_cache", "dram_cache":
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "true", "on", "1":
				out[key] = "on"
			case "false", "off", "0", "":
				out[key] = "off"
			default:
				return nil, status.Errorf(codes.InvalidArgument, "mutable parameter %q must be a boolean", key)
			}
		case "io_type":
			if !lunIoTypePattern.MatchString(value) {
				return nil, status.Errorf(codes.InvalidArgument, "mutable parameter %q must be transactional or sequential", key)
			}
			out[key] = value
		case "qos_priority":
			switch value {
			case "4", "8", "16", "32", "64", "128":
				out[key] = value
			default:
				return nil, status.Errorf(codes.InvalidArgument, "mutable parameter %q has an unsupported value %q", key, value)
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported mutable parameter %q", key)
		}
	}
	return out, nil
}

// LUNMutableWhitelist lists the mutable LUN fields for CSI capability and
// documentation purposes.
var LUNMutableWhitelist = []string{
	"deduplication", "compression", "thin_provision",
	"flash_cache", "dram_cache", "io_type", "qos_priority",
}
