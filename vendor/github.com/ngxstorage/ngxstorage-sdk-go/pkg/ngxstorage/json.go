package ngxstorage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// UnmarshalJSON accepts both the numeric and quoted-string encodings emitted
// by different NGX releases while preserving Number as a string for callers.
func (l *FCTargetLUN) UnmarshalJSON(data []byte) error {
	type lunAlias FCTargetLUN
	decoded := struct {
		*lunAlias
		Number json.RawMessage `json:"number"`
	}{lunAlias: (*lunAlias)(l)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	number, err := flexNumberString(decoded.Number)
	if err != nil {
		return fmt.Errorf("ngxstorage: decode FC LUN number: %w", err)
	}
	l.Number = number
	return nil
}

// UnmarshalJSON accepts both the numeric and quoted-string encodings of the
// SCSI LUN number emitted by different NGX releases, preserving Number as a
// string for callers.
func (l *ISCSITargetLUN) UnmarshalJSON(data []byte) error {
	type lunAlias ISCSITargetLUN
	decoded := struct {
		*lunAlias
		Number json.RawMessage `json:"number"`
	}{lunAlias: (*lunAlias)(l)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	number, err := flexNumberString(decoded.Number)
	if err != nil {
		return fmt.Errorf("ngxstorage: decode iSCSI LUN number: %w", err)
	}
	l.Number = number
	return nil
}

// flexNumberString renders a numeric or quoted-string JSON token as a string.
func flexNumberString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", err
	}
	return number.String(), nil
}

// FlexBool decodes the canonical NGX boolean encodings: JSON boolean,
// "true"/"false" strings, numeric "1"/"0", and the live NFS export
// representation "1" enabled / "" disabled. All forms are accepted.
type FlexBool bool

// UnmarshalJSON accepts every documented NGX boolean encoding.
func (f *FlexBool) UnmarshalJSON(data []byte) error {
	parsed, err := flexBoolFromValue(decodeJSONValue(data))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// successFlag is the canonical mutation acknowledgement. It reuses FlexBool
// so every documented backend encoding (boolean, "1"/"0", "true"/"false")
// is accepted by mutation responses.
type successFlag = FlexBool

// FlexBytes decodes a canonical NGX byte value that the backend serializes
// either as a JSON number or as a decimal string. Null and empty values are
// treated as zero.
type FlexBytes int64

// UnmarshalJSON accepts numeric and string byte encodings.
func (f *FlexBytes) UnmarshalJSON(data []byte) error {
	parsed, err := flexBytesFromValue(decodeJSONValue(data))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// FlexOnOff decodes an NGX on/off attribute that the backend serializes
// inconsistently: LUN detail returns JSON booleans while list endpoints return
// "on"/"off" strings. Both forms normalize to "on"/"off".
type FlexOnOff string

// UnmarshalJSON accepts boolean and string ("on"/"off"/"true"/"false"/"1"/"0")
// encodings and normalizes them to "on"/"off".
func (f *FlexOnOff) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*f = "on"
		} else {
			*f = "off"
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("ngxstorage: FlexOnOff unsupported value %q", data)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "on", "1":
		*f = "on"
	case "false", "off", "0", "":
		*f = "off"
	default:
		*f = FlexOnOff(s)
	}
	return nil
}

// flexBoolFromValue decodes one already-parsed JSON value into a FlexBool.
func flexBoolFromValue(v interface{}) (FlexBool, error) {
	txt := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v)))
	switch txt {
	case "true", "1":
		return true, nil
	case "false", "0", "", "<nil>", "null":
		return false, nil
	default:
		return false, fmt.Errorf("ngxstorage: FlexBool unsupported value %q", v)
	}
}

// flexBytesFromValue decodes one already-parsed JSON value into a FlexBytes.
func flexBytesFromValue(v interface{}) (FlexBytes, error) {
	switch raw := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return FlexBytes(int64(raw)), nil
	case json.Number:
		n, err := raw.Int64()
		return FlexBytes(n), err
	case string:
		text := strings.TrimSpace(raw)
		if text == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("ngxstorage: FlexBytes unsupported value %q", raw)
		}
		return FlexBytes(n), nil
	default:
		return 0, fmt.Errorf("ngxstorage: FlexBytes unsupported value %q", v)
	}
}

// decodeJSONValue parses a raw JSON token into a value suitable for the
// flexible decoders. json.Unmarshal already produced interface{} values in
// struct contexts; this helper is used by the direct UnmarshalJSON methods.
func decodeJSONValue(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}
