package coord

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const MaxHolderIdentityBytes = 127

type Role byte

const (
	RoleNode       Role = 'n'
	RoleController Role = 'c'
)

type SafetyClass byte

const (
	SafetyOrdinary    SafetyClass = 'o'
	SafetyDestructive SafetyClass = 'd'
)

type OperationClass byte

const (
	OperationOther      OperationClass = 'o'
	OperationFilesystem OperationClass = 'f'
	OperationExpand     OperationClass = 'e'
	OperationPublish    OperationClass = 'p'
	OperationDelete     OperationClass = 'd'
	OperationCreate     OperationClass = 'c'
	OperationSnapshot   OperationClass = 's'
	OperationModify     OperationClass = 'm'
)

// Holder identifies one process competing for an operation Lease. PodName and
// PodUID are kept exact because destructive takeover uses them for a direct Pod
// GET. NodeName is represented by a stable short hash in the encoded Lease value
// so the holder string remains bounded even with maximum-length Kubernetes names.
type Holder struct {
	Role      Role
	PodName   string
	PodUID    string
	NodeName  string
	Safety    SafetyClass
	Operation OperationClass
}

type ParsedHolder struct {
	Role      Role
	Safety    SafetyClass
	PodName   string
	PodUID    string
	NodeHash  string
	KeyHash   string
	Operation OperationClass
}

func (h Holder) Validate() error {
	if h.Role != RoleNode && h.Role != RoleController {
		return fmt.Errorf("coord: invalid holder role %q", byte(h.Role))
	}
	if h.Safety != SafetyOrdinary && h.Safety != SafetyDestructive {
		return fmt.Errorf("coord: invalid safety class %q", byte(h.Safety))
	}
	if !validOperationClass(h.Operation) {
		return fmt.Errorf("coord: invalid operation class %q", byte(h.Operation))
	}
	if h.PodName == "" || strings.Contains(h.PodName, "|") {
		return errors.New("coord: exact pod name is required and must not contain '|'")
	}
	if h.PodUID == "" || strings.Contains(h.PodUID, "|") {
		return errors.New("coord: exact pod UID is required and must not contain '|'")
	}
	if h.Role == RoleNode && h.NodeName == "" {
		return errors.New("coord: node holder requires node name")
	}
	return nil
}

func validOperationClass(op OperationClass) bool {
	switch op {
	case OperationOther, OperationFilesystem, OperationExpand, OperationPublish,
		OperationDelete, OperationCreate, OperationSnapshot, OperationModify:
		return true
	default:
		return false
	}
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

// EncodeHolder returns the compact, versioned Lease holderIdentity.
// Format: v1|role|safety|podName|podUID|nodeHash|keyHash|operation
func EncodeHolder(h Holder, semanticKey string) (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	if semanticKey == "" {
		return "", errors.New("coord: semantic lock key is required")
	}
	identity := fmt.Sprintf("v1|%c|%c|%s|%s|%s|%s|%c",
		h.Role,
		h.Safety,
		h.PodName,
		h.PodUID,
		shortHash(h.NodeName),
		shortHash(semanticKey),
		h.Operation,
	)
	if len(identity) > MaxHolderIdentityBytes {
		return "", fmt.Errorf("coord: holder identity is %d bytes, max %d", len(identity), MaxHolderIdentityBytes)
	}
	return identity, nil
}

func ParseHolder(identity string) (ParsedHolder, error) {
	parts := strings.Split(identity, "|")
	if len(parts) != 8 || parts[0] != "v1" {
		return ParsedHolder{}, errors.New("coord: invalid holder identity format")
	}
	if len(parts[1]) != 1 || len(parts[2]) != 1 || len(parts[7]) != 1 {
		return ParsedHolder{}, errors.New("coord: invalid holder identity enum encoding")
	}
	parsed := ParsedHolder{
		Role:      Role(parts[1][0]),
		Safety:    SafetyClass(parts[2][0]),
		PodName:   parts[3],
		PodUID:    parts[4],
		NodeHash:  parts[5],
		KeyHash:   parts[6],
		Operation: OperationClass(parts[7][0]),
	}
	if err := (Holder{
		Role:      parsed.Role,
		PodName:   parsed.PodName,
		PodUID:    parsed.PodUID,
		NodeName:  nodeNameForValidation(parsed.Role),
		Safety:    parsed.Safety,
		Operation: parsed.Operation,
	}).Validate(); err != nil {
		return ParsedHolder{}, fmt.Errorf("coord: invalid parsed holder: %w", err)
	}
	if len(parsed.NodeHash) != 8 || len(parsed.KeyHash) != 8 {
		return ParsedHolder{}, errors.New("coord: invalid holder hash encoding")
	}
	return parsed, nil
}

func nodeNameForValidation(role Role) string {
	if role == RoleNode {
		return "encoded-node"
	}
	return ""
}
