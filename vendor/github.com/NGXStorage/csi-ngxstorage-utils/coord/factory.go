package coord

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const startupPreflightTimeout = 15 * time.Second

// Bounded client-side pacing for the coordination API client. Startup
// preflight issues up to 128 sequential Lease GETs (+1 Pod GET for node
// roles) and must fit inside startupPreflightTimeout. The client-go defaults
// (QPS 5, burst 10) are too slow for a full pool and can exceed the
// deadline. Higher burst/QPS drain the pool in a few seconds. RBAC is
// unchanged: exact-name get/update/patch only, no list/watch.
const coordinationClientQPS = 30
const coordinationClientBurst = 60

var inClusterConfig = rest.InClusterConfig
var newClientset = func(config *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(config)
}

// NewCoordinatorFromEnvironment constructs one release/protocol lock domain
// for drivers which run both controller and node roles.
// There is deliberately no disabled/local fallback for Kubernetes/OpenShift use.
func NewCoordinatorFromEnvironment(role string) (Coordinator, error) {
	prefix := strings.TrimSpace(os.Getenv("NGX_COORDINATION_LEASE_PREFIX"))
	if prefix == "" {
		return nil, fmt.Errorf("coordination: NGX_COORDINATION_LEASE_PREFIX is required")
	}
	count := DefaultShardCount
	if raw := strings.TrimSpace(os.Getenv("NGX_COORDINATION_SHARD_COUNT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("coordination: parse NGX_COORDINATION_SHARD_COUNT %q: %w", raw, err)
		}
		count = parsed
	}

	namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	podUID := strings.TrimSpace(os.Getenv("POD_UID"))
	if namespace == "" || podName == "" || podUID == "" {
		return nil, fmt.Errorf("coordination: POD_NAMESPACE, POD_NAME and POD_UID are required")
	}

	identityRole := RoleController
	nodeName := ""
	if role == "node" || role == "all" {
		identityRole = RoleNode
		nodeName = strings.TrimSpace(os.Getenv("KUBE_NODE_NAME"))
		if nodeName == "" {
			return nil, fmt.Errorf("coordination: KUBE_NODE_NAME is required for %s role", role)
		}
	} else if role != "controller" {
		return nil, fmt.Errorf("coordination: unsupported driver role %q", role)
	}

	return newInClusterCoordinator(namespace, podName, podUID, identityRole, nodeName, prefix, count)
}

// NewControllerCoordinatorFromEnvironment constructs a controller-only Lease
// domain for drivers that never acquire node-role Lease credentials. The
// combined "all" role is a test/local mode that still uses the controller
// identity.
func NewControllerCoordinatorFromEnvironment(role string) (Coordinator, error) {
	if role != "controller" && role != "all" {
		return nil, fmt.Errorf("coordination: unsupported controller-only coordination role %q", role)
	}

	prefix := strings.TrimSpace(os.Getenv("NGX_COORDINATION_LEASE_PREFIX"))
	if prefix == "" {
		return nil, fmt.Errorf("coordination: NGX_COORDINATION_LEASE_PREFIX is required")
	}
	count := DefaultShardCount
	if raw := strings.TrimSpace(os.Getenv("NGX_COORDINATION_SHARD_COUNT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("coordination: parse NGX_COORDINATION_SHARD_COUNT %q: %w", raw, err)
		}
		count = parsed
	}
	coordinationConfig := Config{Prefix: prefix, ShardCount: count}
	if err := coordinationConfig.Validate(); err != nil {
		return nil, fmt.Errorf("coordination: invalid Lease configuration: %w", err)
	}

	namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	podUID := strings.TrimSpace(os.Getenv("POD_UID"))
	if namespace == "" || podName == "" || podUID == "" {
		return nil, fmt.Errorf("coordination: POD_NAMESPACE, POD_NAME and POD_UID are required")
	}

	return newInClusterCoordinator(namespace, podName, podUID, RoleController, "", prefix, count)
}

func newInClusterCoordinator(namespace, podName, podUID string, role Role, nodeName, prefix string, count int) (Coordinator, error) {
	restConfig, err := inClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("coordination: create in-cluster config: %w", err)
	}
	restConfig.QPS = coordinationClientQPS
	restConfig.Burst = coordinationClientBurst
	client, err := newClientset(restConfig)
	if err != nil {
		return nil, fmt.Errorf("coordination: create Kubernetes client: %w", err)
	}
	manager, err := NewManager(ManagerOptions{
		Client:    client,
		Namespace: namespace,
		Config: Config{
			Prefix:     prefix,
			ShardCount: count,
		},
		Identity: ProcessIdentity{
			Role:     role,
			PodName:  podName,
			PodUID:   podUID,
			NodeName: nodeName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("coordination: create manager: %w", err)
	}
	return manager, nil
}

// PreflightCoordinator bounds startup API checks before the CSI socket is marked
// ready. Missing shards/RBAC/pod identity are therefore startup failures.
func PreflightCoordinator(ctx context.Context, coordinator Coordinator) error {
	if coordinator == nil {
		return fmt.Errorf("coordination: coordinator is required")
	}
	preflightCtx, cancel := context.WithTimeout(ctx, startupPreflightTimeout)
	defer cancel()
	return coordinator.Preflight(preflightCtx)
}
