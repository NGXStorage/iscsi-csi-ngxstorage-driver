package coord

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultShardCount is the production default for operation-lock Lease pools.
	DefaultShardCount = 128
	leaseSuffixLen    = len("-operation-lock-000")
	maxDNSSubdomain   = 253
)

// Config defines the fixed operation-lock Lease pool used by one driver release.
// Prefix and ShardCount are part of the distributed lock domain and must be
// identical on every controller/node instance that can contend on the same key.
type Config struct {
	Prefix     string
	ShardCount int
}

// Validate rejects coordination configurations that could create an ambiguous
// or unsupported lock domain.
func (c Config) Validate() error {
	prefix := strings.TrimSpace(c.Prefix)
	if prefix == "" {
		return errors.New("coord: lease prefix is required")
	}
	if prefix != c.Prefix {
		return errors.New("coord: lease prefix must not contain surrounding whitespace")
	}
	if problems := validation.IsDNS1123Subdomain(prefix); len(problems) != 0 {
		return fmt.Errorf("coord: lease prefix %q is not a valid DNS subdomain: %s", prefix, strings.Join(problems, "; "))
	}
	if len(prefix)+leaseSuffixLen > maxDNSSubdomain {
		return fmt.Errorf("coord: lease prefix %q is too long", prefix)
	}
	if !SupportedShardCount(c.ShardCount) {
		return fmt.Errorf("coord: unsupported shard count %d (supported: 32, 64, 128, 256)", c.ShardCount)
	}
	return nil
}

// SupportedShardCount reports whether count is an allowed lock-pool modulus.
func SupportedShardCount(count int) bool {
	switch count {
	case 32, 64, 128, 256:
		return true
	default:
		return false
	}
}

// ShardIndex maps a semantic resource key to a deterministic physical Lease
// shard. The hash function and byte order are part of the coordination contract;
// changing them requires an explicit lock-domain migration.
func (c Config) ShardIndex(key string) (int, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if key == "" {
		return 0, errors.New("coord: semantic lock key is required")
	}
	sum := sha256.Sum256([]byte(key))
	value := binary.BigEndian.Uint64(sum[:8])
	return int(value % uint64(c.ShardCount)), nil
}

// LeaseName resolves a semantic key to the exact pre-created Lease object name.
func (c Config) LeaseName(key string) (string, error) {
	index, err := c.ShardIndex(key)
	if err != nil {
		return "", err
	}
	return c.LeaseNameForIndex(index)
}

// LeaseNameForIndex returns the exact Lease name for a physical shard index.
func (c Config) LeaseNameForIndex(index int) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if index < 0 || index >= c.ShardCount {
		return "", fmt.Errorf("coord: shard index %d out of range [0,%d)", index, c.ShardCount)
	}
	return fmt.Sprintf("%s-operation-lock-%03d", c.Prefix, index), nil
}

// LeaseNames returns the complete declarative Lease pool in index order.
func (c Config) LeaseNames() ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	names := make([]string, 0, c.ShardCount)
	for i := 0; i < c.ShardCount; i++ {
		name, err := c.LeaseNameForIndex(i)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}
