package coord

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var (
	ErrOwnershipLost  = errors.New("coord: lease ownership lost")
	ErrUnsafeTakeover = errors.New("coord: destructive lease takeover cannot be proven safe")
)

const (
	defaultLeaseDuration = 30 * time.Second
	defaultRenewInterval = 10 * time.Second
	defaultRetryInterval = 50 * time.Millisecond
	defaultTakeoverGrace = 5 * time.Second
)

// ProcessIdentity is the stable identity of one CSI process for the lifetime of
// its pod. Safety and operation class are supplied per lock acquisition.
type ProcessIdentity struct {
	Role     Role
	PodName  string
	PodUID   string
	NodeName string
}

// AcquireOptions classify one critical section. SafetyDestructive activates the
// stricter stale-holder takeover policy used around filesystem initialization.
type AcquireOptions struct {
	Safety    SafetyClass
	Operation OperationClass
}

// ManagerOptions configures one operation-lock domain.
type ManagerOptions struct {
	Client        kubernetes.Interface
	Namespace     string
	Config        Config
	Identity      ProcessIdentity
	LeaseDuration time.Duration
	RenewInterval time.Duration
	RetryInterval time.Duration
	TakeoverGrace time.Duration
	Now           func() time.Time
}

// Manager coordinates resource-scoped critical sections through pre-created
// coordination.k8s.io/v1 Lease objects.
type Manager struct {
	client        kubernetes.Interface
	namespace     string
	config        Config
	identity      ProcessIdentity
	leaseDuration time.Duration
	renewInterval time.Duration
	retryInterval time.Duration
	takeoverGrace time.Duration
	now           func() time.Time
	localLocks    sync.Map // map[leaseName]*sync.Mutex
}

// leaseLock is one held physical Lease. OwnershipLost closes when renewal
// proves the holder no longer owns the Lease or the API can no longer safely
// confirm it.
type leaseLock struct {
	manager    *Manager
	leaseName  string
	identity   string
	localMu    *sync.Mutex
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	lost       chan struct{}
	lostOnce   sync.Once
	release    sync.Once
	releaseErr error
}

// multiLock owns a deterministic set of unique physical Lease shards.
type multiLock struct {
	locks []*leaseLock
}

func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Client == nil {
		return nil, errors.New("coord: Kubernetes client is required")
	}
	if strings.TrimSpace(opts.Namespace) == "" || opts.Namespace != strings.TrimSpace(opts.Namespace) {
		return nil, errors.New("coord: namespace is required without surrounding whitespace")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	probeHolder := Holder{
		Role:      opts.Identity.Role,
		PodName:   opts.Identity.PodName,
		PodUID:    opts.Identity.PodUID,
		NodeName:  opts.Identity.NodeName,
		Safety:    SafetyOrdinary,
		Operation: OperationOther,
	}
	if err := probeHolder.Validate(); err != nil {
		return nil, fmt.Errorf("coord: process identity: %w", err)
	}

	leaseDuration := opts.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	renewInterval := opts.RenewInterval
	if renewInterval <= 0 {
		renewInterval = defaultRenewInterval
	}
	retryInterval := opts.RetryInterval
	if retryInterval <= 0 {
		retryInterval = defaultRetryInterval
	}
	takeoverGrace := opts.TakeoverGrace
	if takeoverGrace < 0 {
		return nil, errors.New("coord: takeover grace must not be negative")
	}
	if takeoverGrace == 0 {
		takeoverGrace = defaultTakeoverGrace
	}
	if renewInterval >= leaseDuration {
		return nil, fmt.Errorf("coord: renew interval %s must be shorter than lease duration %s", renewInterval, leaseDuration)
	}
	if leaseDuration < time.Second {
		return nil, errors.New("coord: lease duration must be at least one second")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Manager{
		client:        opts.Client,
		namespace:     opts.Namespace,
		config:        opts.Config,
		identity:      opts.Identity,
		leaseDuration: leaseDuration,
		renewInterval: renewInterval,
		retryInterval: retryInterval,
		takeoverGrace: takeoverGrace,
		now:           now,
	}, nil
}

func (m *Manager) holderFor(key string, opts AcquireOptions) (Holder, string, error) {
	h := Holder{
		Role:      m.identity.Role,
		PodName:   m.identity.PodName,
		PodUID:    m.identity.PodUID,
		NodeName:  m.identity.NodeName,
		Safety:    opts.Safety,
		Operation: opts.Operation,
	}
	identity, err := EncodeHolder(h, key)
	return h, identity, err
}

func (m *Manager) localLock(ctx context.Context, leaseName string) (*sync.Mutex, error) {
	value, _ := m.localLocks.LoadOrStore(leaseName, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	for {
		if mu.TryLock() {
			return mu, nil
		}
		timer := time.NewTimer(m.retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Acquire locks the physical Lease shard for one semantic resource key.
func (m *Manager) Acquire(ctx context.Context, key string, opts AcquireOptions) (Lock, error) {
	leaseName, err := m.config.LeaseName(key)
	if err != nil {
		return nil, err
	}
	lock, err := m.acquirePhysical(ctx, leaseName, key, opts)
	if err != nil {
		return nil, err
	}
	return lock, nil
}

func (m *Manager) acquirePhysical(ctx context.Context, leaseName, identityKey string, opts AcquireOptions) (*leaseLock, error) {
	_, encoded, err := m.holderFor(identityKey, opts)
	if err != nil {
		return nil, err
	}
	localMu, err := m.localLock(ctx, leaseName)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			localMu.Unlock()
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lease, err := m.client.CoordinationV1().Leases(m.namespace).Get(ctx, leaseName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("coord: get Lease %s: %w", leaseName, err)
		}

		canClaim, err := m.canClaim(ctx, lease, encoded, opts.Safety)
		if err != nil {
			return nil, err
		}
		if canClaim {
			claimed, err := m.claim(ctx, lease, encoded)
			if apierrors.IsConflict(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if claimed {
				lockCtx, cancel := context.WithCancel(context.Background())
				lock := &leaseLock{
					manager:   m,
					leaseName: leaseName,
					identity:  encoded,
					localMu:   localMu,
					cancel:    cancel,
					lost:      make(chan struct{}),
				}
				lock.wg.Add(1)
				go lock.renewLoop(lockCtx)
				owned = true
				return lock, nil
			}
		}

		timer := time.NewTimer(m.retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) canClaim(ctx context.Context, lease *coordinationv1.Lease, contender string, contenderSafety SafetyClass) (bool, error) {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return true, nil
	}
	if *lease.Spec.HolderIdentity == contender {
		return true, nil
	}

	expiry, err := leaseExpiry(lease)
	if err != nil {
		return false, err
	}
	now := m.now()
	if now.Before(expiry) {
		return false, nil
	}

	parsed, err := ParseHolder(*lease.Spec.HolderIdentity)
	if err != nil {
		return false, fmt.Errorf("%w: parse existing holder: %v", ErrUnsafeTakeover, err)
	}
	if parsed.Safety != SafetyDestructive && contenderSafety != SafetyDestructive {
		return true, nil
	}
	if now.Before(expiry.Add(m.takeoverGrace)) {
		return false, nil
	}
	return m.destructiveHolderGone(ctx, parsed)
}

func leaseExpiry(lease *coordinationv1.Lease) (time.Time, error) {
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return time.Time{}, fmt.Errorf("%w: held Lease %s has invalid leaseDurationSeconds", ErrUnsafeTakeover, lease.Name)
	}
	var base time.Time
	if lease.Spec.RenewTime != nil {
		base = lease.Spec.RenewTime.Time
	} else if lease.Spec.AcquireTime != nil {
		base = lease.Spec.AcquireTime.Time
	} else {
		return time.Time{}, fmt.Errorf("%w: held Lease %s has no acquire/renew time", ErrUnsafeTakeover, lease.Name)
	}
	return base.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second), nil
}

func (m *Manager) destructiveHolderGone(ctx context.Context, parsed ParsedHolder) (bool, error) {
	pod, err := m.client.CoreV1().Pods(m.namespace).Get(ctx, parsed.PodName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: get holder pod %s: %v", ErrUnsafeTakeover, parsed.PodName, err)
	}
	if string(pod.UID) != parsed.PodUID {
		return true, nil
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, nil
	}
	return false, nil
}

func (m *Manager) claim(ctx context.Context, lease *coordinationv1.Lease, identity string) (bool, error) {
	now := metav1.NewMicroTime(m.now())
	durationSeconds := int32(m.leaseDuration / time.Second)
	if durationSeconds < 1 {
		durationSeconds = 1
	}
	copy := lease.DeepCopy()
	previous := ""
	if copy.Spec.HolderIdentity != nil {
		previous = *copy.Spec.HolderIdentity
	}
	copy.Spec.HolderIdentity = ptr(identity)
	copy.Spec.LeaseDurationSeconds = &durationSeconds
	copy.Spec.RenewTime = &now
	if previous != identity {
		copy.Spec.AcquireTime = &now
		transitions := int32(1)
		if copy.Spec.LeaseTransitions != nil {
			transitions = *copy.Spec.LeaseTransitions + 1
		}
		copy.Spec.LeaseTransitions = &transitions
	}
	if _, err := m.client.CoordinationV1().Leases(m.namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("coord: claim Lease %s: %w", lease.Name, err)
	}
	return true, nil
}

func ptr[T any](v T) *T { return &v }

// AcquireMany acquires unique physical shards in lexical Lease-name order. This
// ordering, not semantic-key ordering, is the deadlock-freedom invariant.
func (m *Manager) AcquireMany(ctx context.Context, keys []string, opts AcquireOptions) (LockSet, error) {
	if len(keys) == 0 {
		return nil, errors.New("coord: at least one semantic lock key is required")
	}
	semantic := append([]string(nil), keys...)
	sort.Strings(semantic)
	groups := map[string][]string{}
	for _, key := range semantic {
		if key == "" {
			return nil, errors.New("coord: semantic lock key is required")
		}
		leaseName, err := m.config.LeaseName(key)
		if err != nil {
			return nil, err
		}
		groups[leaseName] = append(groups[leaseName], key)
	}
	leaseNames := make([]string, 0, len(groups))
	for leaseName := range groups {
		leaseNames = append(leaseNames, leaseName)
	}
	sort.Strings(leaseNames)

	multi := &multiLock{locks: make([]*leaseLock, 0, len(leaseNames))}
	for _, leaseName := range leaseNames {
		identityKey := strings.Join(groups[leaseName], "\x00")
		lock, err := m.acquirePhysical(ctx, leaseName, identityKey, opts)
		if err != nil {
			_ = multi.Release(context.Background())
			return nil, err
		}
		multi.locks = append(multi.locks, lock)
	}
	return multi, nil
}

func (l *leaseLock) LeaseName() string { return l.leaseName }

func (l *leaseLock) OwnershipLost() <-chan struct{} { return l.lost }

func (l *leaseLock) markLost() {
	l.lostOnce.Do(func() { close(l.lost) })
}

func (l *leaseLock) AssertOwned(ctx context.Context) error {
	lease, err := l.manager.client.CoordinationV1().Leases(l.manager.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("coord: assert Lease %s ownership: %w", l.leaseName, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != l.identity {
		l.markLost()
		return ErrOwnershipLost
	}
	return nil
}

func (l *leaseLock) renewLoop(ctx context.Context) {
	defer l.wg.Done()
	ticker := time.NewTicker(l.manager.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.renewOnce(ctx); err != nil {
				l.markLost()
				return
			}
		}
	}
}

func (l *leaseLock) renewOnce(ctx context.Context) error {
	for attempts := 0; attempts < 4; attempts++ {
		lease, err := l.manager.client.CoordinationV1().Leases(l.manager.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != l.identity {
			return ErrOwnershipLost
		}
		copy := lease.DeepCopy()
		now := metav1.NewMicroTime(l.manager.now())
		copy.Spec.RenewTime = &now
		if _, err := l.manager.client.CoordinationV1().Leases(l.manager.namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("coord: renew Lease conflict retry exhausted")
}

func (l *leaseLock) Release(ctx context.Context) error {
	l.release.Do(func() {
		l.cancel()
		l.wg.Wait()
		defer l.localMu.Unlock()
		l.releaseErr = l.releaseRemote(ctx)
	})
	return l.releaseErr
}

func (l *leaseLock) releaseRemote(ctx context.Context) error {
	for attempts := 0; attempts < 4; attempts++ {
		lease, err := l.manager.client.CoordinationV1().Leases(l.manager.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("coord: get Lease %s for release: %w", l.leaseName, err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != l.identity {
			return ErrOwnershipLost
		}
		copy := lease.DeepCopy()
		empty := ""
		copy.Spec.HolderIdentity = &empty
		now := metav1.NewMicroTime(l.manager.now())
		copy.Spec.RenewTime = &now
		if _, err := l.manager.client.CoordinationV1().Leases(l.manager.namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("coord: release Lease %s: %w", l.leaseName, err)
		}
		return nil
	}
	return errors.New("coord: release Lease conflict retry exhausted")
}

func (m *multiLock) PhysicalLockCount() int { return len(m.locks) }

func (m *multiLock) LeaseNames() []string {
	names := make([]string, 0, len(m.locks))
	for _, lock := range m.locks {
		names = append(names, lock.LeaseName())
	}
	return names
}

func (m *multiLock) AssertOwned(ctx context.Context) error {
	for _, lock := range m.locks {
		if err := lock.AssertOwned(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiLock) Release(ctx context.Context) error {
	var first error
	for i := len(m.locks) - 1; i >= 0; i-- {
		if err := m.locks[i].Release(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// PodUIDMatch is exported for adapter/preflight tests that must verify the exact
// pod identity used by destructive takeover checks.
func PodUIDMatch(pod *corev1.Pod, uid string) bool {
	return pod != nil && pod.UID == types.UID(uid)
}
