package helper

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	names "github.com/NGXStorage/csi-ngxstorage-utils/names"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"
	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

// MinVolumeBytes is the default minimum volume size (1 GiB) when the request
// does not specify a capacity range.
const MinVolumeBytes int64 = 1 << 30

const (
	ISCSIDeleteVerifyTimeout  = 60 * time.Second
	ISCSIDeleteVerifyInterval = 2 * time.Second
)

const (
	iscsiUnmapVerifyAttempts = 30
	iscsiUnmapVerifyInterval = time.Second
)

// Reconciler owns backend lookup/create/reconcile/cleanup orchestration for
// LUNs, targets and auth groups. Mutations survive lost responses through
// name-keyed readback; the CSI retry policy stays here, not in the SDK.
type Reconciler struct {
	Client         *ngxstorage.Client
	PoolOwner      string
	PortalGroup    string
	VolumeIDPrefix string
	// DeleteVerifyTimeout/DeleteVerifyInterval bound the read-after-delete
	// verification for LUNs, snapshots, targets and auth groups; zero values
	// use the package defaults. Overridable in tests for deterministic budgets.
	DeleteVerifyTimeout  time.Duration
	DeleteVerifyInterval time.Duration
	Log                  *zap.Logger
}

func (r *Reconciler) log() *zap.Logger {
	if r.Log != nil {
		return r.Log
	}
	return zap.NewNop()
}

// createLUNReconciled provisions a name-keyed LUN and survives a lost
// response: the backend enforces unique names, so a re-list by name resolves
// the outcome before any retry.
func (r *Reconciler) CreateLUNReconciled(ctx context.Context, name string, sizeGB int64, opts ngxstorage.LUNCreateOptions) (*ngxstorage.LUN, error) {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		lun, err := r.Client.LUNs().CreateWithOptions(ctx, name, sizeGB, opts)
		if err == nil {
			return lun, nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return nil, err
		}
		if existing, found, lerr := r.LUNByName(ctx, name); lerr == nil && found {
			r.log().Warn("create LUN reconciled after transport failure",
				zap.String("name", name), zap.String("lun_id", existing.ID))
			return existing, nil
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// lunByName returns the LUN with the exact name.
func (r *Reconciler) LUNByName(ctx context.Context, name string) (*ngxstorage.LUN, bool, error) {
	luns, err := r.Client.LUNs().List(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range luns {
		if luns[i].Name == name {
			return &luns[i], true, nil
		}
	}
	return nil, false, nil
}

// lunByNameStrict returns the canonical LUN with the given name. Backend list
// errors are propagated so a failed idempotency probe never becomes a create.
func (r *Reconciler) LUNByNameStrict(ctx context.Context, name string) (*ngxstorage.LUN, error) {
	luns, err := r.Client.LUNs().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range luns {
		if luns[i].Name == name {
			return &luns[i], nil
		}
	}
	return nil, nil
}

// VerifyLUNDeleted polls the backend until the LUN is authoritatively
// NotFound, bounded by the request context and ISCSIDeleteVerifyTimeout.
//   - present LUN: keep polling within budget; budget exhausted -> error
//     (CSI success must not be claimed while the backend object exists).
//   - ambiguous GET error (403/timeout/5xx/transport): retry within budget;
//     NEVER treated as absence; budget exhausted -> error.
//   - authoritative NotFound: success (idempotent).
//
// Lock/ownership rules are enforced by the caller; this method only reads.
func (r *Reconciler) VerifyLUNDeleted(ctx context.Context, lunID string) error {
	timeout := r.DeleteVerifyTimeout
	if timeout <= 0 {
		timeout = ISCSIDeleteVerifyTimeout
	}
	interval := r.DeleteVerifyInterval
	if interval <= 0 {
		interval = ISCSIDeleteVerifyInterval
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		_, err := r.Client.LUNs().Get(vctx, lunID)
		switch {
		case err == nil:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: LUN %s still present after a successful delete response (backend acknowledged but did not delete)", lunID)
		case lunAbsenceProven(err):
			return nil
		default:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: LUN %s absence GET ambiguous (not absence proof): %v", lunID, err)
		}
		select {
		case <-vctx.Done():
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return status.Errorf(codes.DeadlineExceeded,
				"delete verification budget exhausted for LUN %s; absence NOT proven: %v", lunID, lastErr)
		case <-time.After(interval):
		}
	}
}

// lunAbsenceProven reports whether err is an AUTHORITATIVE backend absence
// signal for the LUN: the canonical NotFound code (5011), or a code-less
// HTTP 404 from the LUN endpoint. It is deliberately stricter than the
// general ClassifyError mapping (which treats any statusCode==404 as
// NotFound for CO error reporting): a bare HTTP 404 carrying a DIFFERENT
// explicit API code is not absence proof — a gateway/proxy 404 or an
// unrelated error surfaced with 404 must never be read as a successful
// delete. No other HTTP error status (403/5xx) or transport failure proves
// absence; those stay ambiguous and fail closed.
func lunAbsenceProven(err error) bool {
	var apiErr *ngxstorage.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 404 {
		return false
	}
	return apiErr.Code == ngxstorage.CodeNotFound || apiErr.Code == 0
}

// VerifySnapshotDeleted polls the backend until the snapshot is authoritatively
// NotFound, bounded by the request context and ISCSIDeleteVerifyTimeout.
//   - present snapshot: keep polling within budget; budget exhausted -> error
//     (CSI success must not be claimed while the backend object exists).
//   - ambiguous GET error (403/timeout/5xx/transport): retry within budget;
//     NEVER treated as absence; budget exhausted -> error.
//   - authoritative NotFound: success (idempotent).
//
// Lock/ownership rules are enforced by the caller; this method only reads.
func (r *Reconciler) VerifySnapshotDeleted(ctx context.Context, snapshotID string) error {
	timeout := r.DeleteVerifyTimeout
	if timeout <= 0 {
		timeout = ISCSIDeleteVerifyTimeout
	}
	interval := r.DeleteVerifyInterval
	if interval <= 0 {
		interval = ISCSIDeleteVerifyInterval
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		_, err := r.Client.Snapshots().Get(vctx, snapshotID)
		switch {
		case err == nil:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: snapshot %s still present after a successful delete response (backend acknowledged but did not delete)", snapshotID)
		case snapshotAbsenceProven(err):
			return nil
		default:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: snapshot %s absence GET ambiguous (not absence proof): %v", snapshotID, err)
		}
		select {
		case <-vctx.Done():
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return status.Errorf(codes.DeadlineExceeded,
				"delete verification budget exhausted for snapshot %s; absence NOT proven: %v", snapshotID, lastErr)
		case <-time.After(interval):
		}
	}
}

// snapshotAbsenceProven reports whether err is an AUTHORITATIVE backend
// absence signal for the snapshot: HTTP 404 carrying the canonical NotFound
// code (5011), or a code-less HTTP 404 from the snapshot endpoint. A 404 with
// a different explicit code, any other HTTP error status (403/5xx) or a
// transport failure never proves absence; those stay ambiguous and fail
// closed.
func snapshotAbsenceProven(err error) bool {
	var apiErr *ngxstorage.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 404 {
		return false
	}
	return apiErr.Code == ngxstorage.CodeNotFound || apiErr.Code == 0
}

// VerifyTargetDeleted polls the backend until the iSCSI target is
// authoritatively NotFound, bounded by the request context and
// ISCSIDeleteVerifyTimeout.
//   - present target: keep polling within budget; budget exhausted -> error
//     (CSI success must not be claimed while the backend object exists).
//   - ambiguous GET error (403/timeout/5xx/transport): retry within budget;
//     NEVER treated as absence; budget exhausted -> error.
//   - authoritative NotFound: success (idempotent).
//
// Lock/ownership rules are enforced by the caller; this method only reads.
func (r *Reconciler) VerifyTargetDeleted(ctx context.Context, targetID string) error {
	timeout := r.DeleteVerifyTimeout
	if timeout <= 0 {
		timeout = ISCSIDeleteVerifyTimeout
	}
	interval := r.DeleteVerifyInterval
	if interval <= 0 {
		interval = ISCSIDeleteVerifyInterval
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		_, err := r.Client.ISCSITargets().Get(vctx, targetID)
		switch {
		case err == nil:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: iSCSI target %s still present after a successful delete response (backend acknowledged but did not delete)", targetID)
		case targetAbsenceProven(err):
			return nil
		default:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: iSCSI target %s absence GET ambiguous (not absence proof): %v", targetID, err)
		}
		select {
		case <-vctx.Done():
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return status.Errorf(codes.DeadlineExceeded,
				"delete verification budget exhausted for iSCSI target %s; absence NOT proven: %v", targetID, lastErr)
		case <-time.After(interval):
		}
	}
}

// targetAbsenceProven reports whether err is an AUTHORITATIVE backend absence
// signal for the iSCSI target: HTTP 404 carrying a canonical target not-found
// code (5011 NotFound or 611 TargetDeleted), or a code-less HTTP 404 from the
// target endpoint. The IQN-scoped not-found code (521) describes an initiator
// membership error, not target absence, and is deliberately excluded. A 404
// with any other explicit code, any other HTTP error status (403/5xx) or a
// transport failure never proves absence; those stay ambiguous and fail
// closed.
func targetAbsenceProven(err error) bool {
	var apiErr *ngxstorage.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 404 {
		return false
	}
	return apiErr.Code == ngxstorage.CodeNotFound || apiErr.Code == ngxstorage.CodeTargetDeleted || apiErr.Code == 0
}

// VerifyAuthGroupDeleted polls the backend until the auth group is
// authoritatively NotFound, bounded by the request context and
// ISCSIDeleteVerifyTimeout.
//   - present group: keep polling within budget; budget exhausted -> error
//     (an orphan group can retain stale CHAP credentials and IQNs).
//   - ambiguous GET error (403/timeout/5xx/transport): retry within budget;
//     NEVER treated as absence; budget exhausted -> error.
//   - authoritative NotFound: success (idempotent).
//
// Lock/ownership rules are enforced by the caller; this method only reads.
func (r *Reconciler) VerifyAuthGroupDeleted(ctx context.Context, authGroupID string) error {
	timeout := r.DeleteVerifyTimeout
	if timeout <= 0 {
		timeout = ISCSIDeleteVerifyTimeout
	}
	interval := r.DeleteVerifyInterval
	if interval <= 0 {
		interval = ISCSIDeleteVerifyInterval
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		_, err := r.Client.AuthGroups().Get(vctx, authGroupID)
		switch {
		case err == nil:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: iSCSI auth group %s still present after a successful delete response (backend acknowledged but did not delete)", authGroupID)
		case authGroupAbsenceProven(err):
			return nil
		default:
			lastErr = status.Errorf(codes.Internal,
				"delete verification: iSCSI auth group %s absence GET ambiguous (not absence proof): %v", authGroupID, err)
		}
		select {
		case <-vctx.Done():
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return status.Errorf(codes.DeadlineExceeded,
				"delete verification budget exhausted for iSCSI auth group %s; absence NOT proven: %v", authGroupID, lastErr)
		case <-time.After(interval):
		}
	}
}

// authGroupAbsenceProven reports whether err is an AUTHORITATIVE backend
// absence signal for the auth group: HTTP 404 carrying the canonical NotFound
// code (5011), or a code-less HTTP 404 from the auth-group endpoint. The
// IQN-scoped not-found code (521) describes an initiator membership error,
// not group absence, and is deliberately excluded. A 404 with any other
// explicit code, any other HTTP error status (403/5xx) or a transport failure
// never proves absence; those stay ambiguous and fail closed.
func authGroupAbsenceProven(err error) bool {
	var apiErr *ngxstorage.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 404 {
		return false
	}
	return apiErr.Code == ngxstorage.CodeNotFound || apiErr.Code == 0
}

// addLUNToTargetReconciled maps a LUN to a target and survives a lost
// response by re-reading the target: a present mapping is success.
func (r *Reconciler) AddLUNToTargetReconciled(ctx context.Context, targetID, lunID, owner string) error {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		err := r.Client.ISCSITargets().AddLUN(ctx, targetID, lunID, owner)
		if err == nil {
			return nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return err
		}
		if target, gerr := r.Client.ISCSITargets().Get(ctx, targetID); gerr == nil {
			if TargetHasLUN(target, lunID) {
				r.log().Warn("LUN map reconciled after transport failure",
					zap.String("target_id", targetID), zap.String("lun_id", lunID))
				return nil
			}
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return serr
		}
	}
	return lastErr
}

// removeLUNReconciled unmaps a LUN and survives a lost response by re-reading
// the target: an absent mapping is success.
func (r *Reconciler) RemoveLUNReconciled(ctx context.Context, targetID, lunID string) error {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		err := r.Client.ISCSITargets().RemoveLUN(ctx, targetID, lunID, r.PoolOwner)
		if err == nil {
			return nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return err
		}
		if target, gerr := r.Client.ISCSITargets().Get(ctx, targetID); gerr == nil {
			if !TargetHasLUN(target, lunID) {
				r.log().Warn("LUN unmap reconciled after transport failure",
					zap.String("target_id", targetID), zap.String("lun_id", lunID))
				return nil
			}
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return serr
		}
	}
	return lastErr
}

// WaitForISCSIUnmap waits until the LUN is provably unmapped from the target
// before the caller may delete the target. A 200 unmap ack is not proof the
// mapping is gone: the backend readback must show the LUN absent from the
// target's mapping list AND the LUN no longer reporting an iSCSI protocol
// mapping. Bounded by iscsiUnmapVerifyAttempts/iscsiUnmapVerifyInterval and
// the request context; any uncertainty fails closed:
//   - authoritative target absence: converged (no mapping can persist).
//   - authoritative LUN absence: converged (the LUN cannot stay mapped).
//   - ambiguous target/LUN GET error: fail closed immediately; a read error
//     is never convergence proof.
//   - still mapped at budget exhaustion: error.
func (r *Reconciler) WaitForISCSIUnmap(ctx context.Context, targetID, lunID string) error {
	for attempt := 1; attempt <= iscsiUnmapVerifyAttempts; attempt++ {
		target, err := r.Client.ISCSITargets().Get(ctx, targetID)
		if err != nil {
			if targetAbsenceProven(err) {
				return nil
			}
			return fmt.Errorf("get iSCSI target %s: %w", targetID, err)
		}
		targetMapped := TargetHasLUN(target, lunID)

		lun, err := r.Client.LUNs().Get(ctx, lunID)
		if err != nil {
			if lunAbsenceProven(err) {
				return nil
			}
			return fmt.Errorf("get LUN %s: %w", lunID, err)
		}
		lunMapped := strings.TrimSpace(lun.Protocols["iscsi"]) != ""
		if !targetMapped && !lunMapped {
			return nil
		}

		if attempt == iscsiUnmapVerifyAttempts {
			break
		}
		timer := time.NewTimer(iscsiUnmapVerifyInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("LUN %s is still mapped to iSCSI target %s", lunID, targetID)
}

// registerIQNReconciled adds an initiator to an auth group and survives a
// lost response by re-reading the group: a present IQN is success. Adding the
// same IQN twice is a backend no-op.
func (r *Reconciler) RegisterIQNReconciled(ctx context.Context, authGroupID, iqn, alias string) error {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		_, err := r.Client.AuthGroups().AddIQN(ctx, authGroupID, iqn, alias, r.PoolOwner)
		if err == nil {
			return nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return err
		}
		if group, gerr := r.Client.AuthGroups().Get(ctx, authGroupID); gerr == nil {
			if AuthGroupHasIQN(group, iqn) {
				r.log().Warn("initiator registration reconciled after transport failure",
					zap.String("auth_group_id", authGroupID), zap.String("iqn", iqn))
				return nil
			}
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return serr
		}
	}
	return lastErr
}

// createAuthGroupReconciled creates a name-keyed auth group and reconciles a
// lost response by re-listing by name.
func (r *Reconciler) CreateAuthGroupReconciled(ctx context.Context, name, owner string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		id, err := r.Client.AuthGroups().Create(ctx, name, owner)
		if err == nil {
			return id, nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return "", err
		}
		if id, ok, lerr := r.AuthGroupByName(ctx, name); lerr == nil && ok {
			r.log().Warn("auth group create reconciled after transport failure",
				zap.String("name", name), zap.String("auth_group_id", id))
			return id, nil
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return "", serr
		}
	}
	return "", lastErr
}

// createISCSITargetReconciled creates a name-keyed target and reconciles a
// lost response by re-listing by name.
func (r *Reconciler) CreateISCSITargetReconciled(ctx context.Context, name, authGroupID, portalGroupID, owner string) (*ngxstorage.ISCSITarget, error) {
	var lastErr error
	for attempt := 1; attempt <= utils.ReconcileAttempts; attempt++ {
		err := r.Client.ISCSITargets().Create(ctx, name, authGroupID, portalGroupID, owner)
		if err == nil {
			target, found, lookupErr := r.TargetByName(ctx, name)
			if lookupErr != nil {
				return nil, fmt.Errorf("lookup created iSCSI target %q: %w", name, lookupErr)
			}
			if !found {
				return nil, fmt.Errorf("created iSCSI target %q was not returned by name lookup", name)
			}
			return target, nil
		}
		lastErr = err
		if !ngxstorage.IsTransportError(err) {
			return nil, err
		}
		if existing, found, lerr := r.TargetByName(ctx, name); lerr == nil && found {
			r.log().Warn("iSCSI target create reconciled after transport failure",
				zap.String("name", name), zap.String("target_id", existing.ID))
			return existing, nil
		}
		if serr := utils.ReconcileBackoff(ctx, attempt); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// targetByName returns the iSCSI target with the exact name.
func (r *Reconciler) TargetByName(ctx context.Context, name string) (*ngxstorage.ISCSITarget, bool, error) {
	targets, err := r.Client.ISCSITargets().List(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range targets {
		if targets[i].Name == name {
			return &targets[i], true, nil
		}
	}
	return nil, false, nil
}

// The owner must match the pool owner; the backend rejects mismatches (code 661).
func (r *Reconciler) FindOrCreateAuthGroup(ctx context.Context, name, owner string) (string, error) {
	if id, ok, err := r.AuthGroupByName(ctx, name); err != nil {
		return "", err
	} else if ok {
		return id, nil
	}
	id, err := r.CreateAuthGroupReconciled(ctx, name, owner)
	if err == nil {
		return id, nil
	}
	if !ngxstorage.IsAlreadyExists(err) {
		return "", err
	}
	// Race: another request created the group; re-list and match by name.
	if id, ok, listErr := r.AuthGroupByName(ctx, name); listErr != nil {
		return "", listErr
	} else if ok {
		return id, nil
	}
	return "", err
}

// authGroupByName resolves an auth group ID by its name, if present.
func (r *Reconciler) AuthGroupByName(ctx context.Context, name string) (string, bool, error) {
	groups, err := r.Client.AuthGroups().List(ctx)
	if err != nil {
		return "", false, err
	}
	for _, group := range groups {
		if group.Name == name {
			return group.ID, true, nil
		}
	}
	return "", false, nil
}

// findISCSITargetByName returns full target detail. The lightweight target list
// is never trusted for auth-group/LUN decisions.
func (r *Reconciler) FindISCSITargetByName(ctx context.Context, name string) (*ngxstorage.ISCSITarget, bool, error) {
	targets, err := r.Client.ISCSITargets().List(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, target := range targets {
		if target.Name != name {
			continue
		}
		detail, err := r.Client.ISCSITargets().Get(ctx, target.ID)
		if err != nil {
			return nil, false, err
		}
		return detail, true, nil
	}
	return nil, false, nil
}

// findISCSITargetForVolume prefers the deterministic per-volume target name but
// also recognizes a legacy target by its mapped LUN so upgrades do not strand
// existing mappings. Full target details are always fetched before mutation.
func (r *Reconciler) FindISCSITargetForVolume(ctx context.Context, volumeHandle, lunID string) (*ngxstorage.ISCSITarget, bool, error) {
	targetName := names.ModifyVolName(volumeHandle)
	targets, err := r.Client.ISCSITargets().List(ctx)
	if err != nil {
		return nil, false, err
	}
	var legacy *ngxstorage.ISCSITarget
	for _, listed := range targets {
		detail, err := r.Client.ISCSITargets().Get(ctx, listed.ID)
		if err != nil {
			// The backend can return a stale target ID from List after that
			// target has already been removed. A detail-level 5011 proves this
			// entry is absent; keep scanning so it cannot strand unpublish.
			if ngxstorage.IsNotFound(err) {
				continue
			}
			return nil, false, err
		}
		if detail.Name == targetName {
			return detail, true, nil
		}
		for _, lun := range detail.Luns {
			if lun.ID == lunID {
				legacy = detail
				break
			}
		}
	}
	return legacy, legacy != nil, nil
}

// findMappedISCSITarget returns the target that currently maps the LUN, if
// any. It deliberately scans every target instead of trusting the deterministic
// per-volume target name so legacy mappings cannot be missed during delete.
func (r *Reconciler) FindMappedISCSITarget(ctx context.Context, lunID string) (string, bool, error) {
	targets, err := r.Client.ISCSITargets().List(ctx)
	if err != nil {
		return "", false, err
	}
	for _, listed := range targets {
		detail, err := r.Client.ISCSITargets().Get(ctx, listed.ID)
		if err != nil {
			// A listed target that is already gone cannot map this LUN.
			// Preserve fail-closed behavior for every other detail error.
			if ngxstorage.IsNotFound(err) {
				continue
			}
			return "", false, err
		}
		for _, mapped := range detail.Luns {
			if mapped.ID == lunID {
				return detail.ID, true, nil
			}
		}
	}
	return "", false, nil
}

// resolveOrCreateSharedISCSITarget implements target-first lifecycle. Existing
// targets keep and reuse their auth group; a new auth group is created only
// when the per-volume target itself is absent. The caller must hold the
// per-volume distributed Lease.
func (r *Reconciler) ResolveOrCreateSharedISCSITarget(ctx context.Context, name, portalGroupID, owner string) (*ngxstorage.ISCSITarget, string, error) {
	if target, found, err := r.FindISCSITargetByName(ctx, name); err != nil {
		return nil, "", err
	} else if found {
		if target.Owner != "" && target.Owner != owner {
			return nil, "", status.Errorf(codes.FailedPrecondition,
				"iSCSI target %s owner %s does not match volume owner %s", target.ID, target.Owner, owner)
		}
		if target.AuthGroupID == "" {
			return nil, "", status.Errorf(codes.FailedPrecondition,
				"existing iSCSI target %s has no auth group", target.ID)
		}
		return target, target.AuthGroupID, nil
	}

	// New 11.1 resources are volume/target-scoped, not node-scoped. A crash
	// between group and target creation is recovered by deterministic reuse on
	// the next retry rather than creating another group.
	authGroupID, err := r.FindOrCreateAuthGroup(ctx, name, owner)
	if err != nil {
		return nil, "", err
	}
	target, err := r.CreateISCSITargetReconciled(ctx, name, authGroupID, portalGroupID, owner)
	if err == nil {
		if target == nil || target.ID == "" {
			return nil, "", status.Error(codes.Internal, "create iSCSI target returned empty id")
		}
		return target, authGroupID, nil
	}
	if !ngxstorage.IsAlreadyExists(err) {
		return nil, "", err
	}
	// Defensive retry path for a confirmed already-exists response. With the
	// distributed volume lock this normally means recovery from prior state.
	target, found, lookupErr := r.FindISCSITargetByName(ctx, name)
	if lookupErr != nil {
		return nil, "", lookupErr
	}
	if !found {
		return nil, "", err
	}
	if target.Owner != "" && target.Owner != owner {
		return nil, "", status.Errorf(codes.FailedPrecondition,
			"iSCSI target %s owner %s does not match volume owner %s", target.ID, target.Owner, owner)
	}
	if target.AuthGroupID == "" {
		return nil, "", status.Errorf(codes.FailedPrecondition,
			"existing iSCSI target %s has no auth group", target.ID)
	}
	return target, target.AuthGroupID, nil
}
func (r *Reconciler) AuthGroupReferencedByOtherTarget(ctx context.Context, authGroupID, excludingTargetID string) (bool, error) {
	targets, err := r.Client.ISCSITargets().List(ctx)
	if err != nil {
		return false, err
	}
	for _, listed := range targets {
		if listed.ID == excludingTargetID {
			continue
		}
		detail, err := r.Client.ISCSITargets().Get(ctx, listed.ID)
		if err != nil {
			// A stale list entry is not a live reference. Only a canonical
			// not-found is ignored; transport/auth/server errors remain fatal.
			if ngxstorage.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if detail.AuthGroupID == authGroupID {
			return true, nil
		}
	}
	return false, nil
}

// cleanupOrphanedVolumeScopedAuthGroup removes the deterministic volume-scoped
// auth group left behind when a target lifecycle failed after group creation
// and its retry found the target already gone. Legacy node-scoped groups are
// never touched: ownership requires the exact 11.1 volume-scoped name AND no
// remaining target reference. The already-held Lease is re-asserted immediately
// before the destructive delete because the lookup reads above can take long
// enough for the Lease to move to another holder.
func (r *Reconciler) CleanupOrphanedVolumeScopedAuthGroup(ctx context.Context, lock coord.Lock, operation, volumeHandle string) error {
	groupID, ok, err := r.AuthGroupByName(ctx, names.ModifyVolName(volumeHandle))
	if err != nil {
		return status.Errorf(codes.Unavailable, "list iSCSI auth groups for cleanup: %v", err)
	}
	if !ok {
		return nil
	}
	inUse, err := r.AuthGroupReferencedByOtherTarget(ctx, groupID, "")
	if err != nil {
		return status.Errorf(codes.Internal, "verify iSCSI auth group references before cleanup: %v", err)
	}
	if inUse {
		return nil
	}
	if err := lock.AssertOwned(ctx); err != nil {
		return utils.CoordinationError("verify "+operation+" before auth group cleanup", err)
	}
	if err := r.Client.AuthGroups().Delete(ctx, groupID); err != nil && !ngxstorage.IsNotFound(err) {
		return status.Errorf(codes.Internal, "delete orphaned iSCSI auth group: %v", err)
	}
	// Read-after-delete: a 200 delete ack is not proof of backend absence.
	// An orphan group that survives the ack retains stale CHAP credentials
	// and IQNs, so cleanup success requires authoritative NotFound.
	if err := r.VerifyAuthGroupDeleted(ctx, groupID); err != nil {
		return err
	}
	r.log().Info("cleanup: deleted orphaned volume-scoped iSCSI auth group (backend absence verified)",
		zap.String("auth_group_id", groupID), zap.String("volume_id", volumeHandle))
	return nil
}

// SelectPortalGroup resolves the portal group used for target creation. When
// a portal-group name is configured, that exact group must exist: a renamed
// or removed configured group fails closed instead of silently selecting an
// unrelated group whose portals would be handed to the CO. With no configured
// name the first available group is used (startup validated ownership).
func (r *Reconciler) SelectPortalGroup(ctx context.Context) (id string, ips []string, err error) {
	groups, err := r.Client.PortalGroups().List(ctx)
	if err != nil {
		return "", nil, err
	}
	if len(groups) == 0 {
		return "", nil, status.Error(codes.NotFound, "no iSCSI portal groups available")
	}
	var chosen *ngxstorage.PortalGroup
	for i := range groups {
		if r.PortalGroup != "" && groups[i].Name != r.PortalGroup {
			continue
		}
		chosen = &groups[i]
		break
	}
	if chosen == nil {
		return "", nil, status.Errorf(codes.NotFound,
			"configured iSCSI portal group %q not found; refusing to select a different group", r.PortalGroup)
	}
	detail, err := r.Client.PortalGroups().Get(ctx, chosen.ID)
	if err != nil {
		return "", nil, err
	}
	return detail.ID, detail.IPAddresses(), nil
}

// GetSnapshot returns the canonical backend snapshot for the requested ID.
// SnapshotByID looks up one snapshot by canonical ID and prepares its CSI
// proto. A nil snapshot with nil error means absence; the RPC owner makes
// the NotFound decision. Backend errors return unwrapped for classification.
func (r *Reconciler) SnapshotByID(ctx context.Context, snapshotID string) (*csi.Snapshot, error) {
	snap, err := r.Client.Snapshots().Get(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if snap == nil || snap.ID == "" {
		return nil, nil
	}
	if snap.ID != snapshotID {
		return nil, status.Error(codes.Internal, "get snapshot: backend response has an invalid id")
	}
	return SnapshotProto(r.VolumeIDPrefix, snap), nil
}

// resizeRestoredLUN completes an interrupted snapshot restore before publication.
func (r *Reconciler) ResizeRestoredLUN(ctx context.Context, lun *ngxstorage.LUN, req *csi.CreateVolumeRequest, snapshot *ngxstorage.Snapshot, assertOwned func() error) (*ngxstorage.LUN, error) {
	if snapshot == nil {
		return lun, nil
	}
	identityRequest := &csi.CreateVolumeRequest{
		Parameters:    req.GetParameters(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1, LimitBytes: req.GetCapacityRange().GetLimitBytes()},
	}
	if err := ValidateExistingCreateVolume(lun, identityRequest, snapshot); err != nil {
		return nil, err
	}
	required := req.GetCapacityRange().GetRequiredBytes()
	if required <= 0 {
		required = MinVolumeBytes
	}
	current := int64(lun.Size)
	if current >= required {
		return lun, nil
	}
	for _, target := range lun.Protocols {
		if strings.TrimSpace(target) != "" {
			return nil, status.Error(codes.FailedPrecondition, "undersized restore is already mapped; refusing create-time resize")
		}
	}
	const gib = int64(1 << 30)
	targetGB := (required-1)/gib + 1
	if targetGB > (1<<63-1)/gib || current%gib != 0 {
		return nil, status.Error(codes.OutOfRange, "restore capacity cannot be represented in whole GiB")
	}
	if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && targetGB*gib > limit {
		return nil, status.Error(codes.OutOfRange, "restored volume would exceed capacity limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := assertOwned(); err != nil {
		return nil, utils.CoordinationError("verify restore resize lock", err)
	}
	incrementGB := targetGB - current/gib
	if err := r.Client.LUNs().Expand(ctx, lun.ID, incrementGB); err != nil {
		return nil, utils.BackendStatusError("expand restored LUN", err)
	}
	updated, err := r.Client.LUNs().Get(ctx, lun.ID)
	if err != nil {
		return nil, utils.BackendStatusError("read expanded restore", err)
	}
	if updated == nil || updated.ID != lun.ID || updated.Name != lun.Name || int64(updated.Size) != targetGB*gib {
		return nil, status.Error(codes.Internal, "restored LUN resize readback mismatch")
	}
	if err := ValidateExistingCreateVolume(updated, req, snapshot); err != nil {
		return nil, err
	}
	r.log().Info("CreateVolume(): restored LUN expanded", zap.String("lun_id", lun.ID),
		zap.Int64("before_bytes", current), zap.Int64("after_bytes", int64(updated.Size)), zap.Int64("increment_gib", incrementGB))
	return updated, nil
}

// targetHasLUN reports whether the target still lists the LUN mapping.
func TargetHasLUN(target *ngxstorage.ISCSITarget, lunID string) bool {
	if target == nil {
		return false
	}
	for i := range target.Luns {
		if target.Luns[i].ID == lunID {
			return true
		}
	}
	return false
}

// authGroupHasIQN reports whether the auth group already lists the IQN.
func AuthGroupHasIQN(group *ngxstorage.AuthGroup, iqn string) bool {
	if group == nil {
		return false
	}
	for i := range group.Initiators {
		if group.Initiators[i].IQN == iqn || group.Initiators[i].Initiator == iqn {
			return true
		}
	}
	return false
}
func VerifyLUNMutableReadback(lun *ngxstorage.LUN, expected map[string]interface{}) error {
	for key, want := range expected {
		var got string
		switch key {
		case "deduplication":
			got = string(lun.Deduplication)
		case "compression":
			got = string(lun.Compression)
		case "thin_provision":
			got = string(lun.ThinProvision)
		case "flash_cache":
			got = string(lun.FlashCache)
		case "dram_cache":
			got = string(lun.DramCache)
		case "io_type":
			got = lun.IoType
		case "qos_priority":
			got = strconv.FormatInt(int64(lun.QosPriority), 10)
		default:
			return status.Errorf(codes.Internal, "modified LUN readback has unsupported field %q", key)
		}
		if got != fmt.Sprint(want) {
			return status.Errorf(codes.Internal,
				"modified LUN readback mismatch for %s: got=%q want=%q", key, got, fmt.Sprint(want))
		}
	}
	return nil
}
func CompatibleBlockSize(actual, requested string) bool {
	actual = strings.ToLower(strings.TrimSpace(actual))
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" || requested == "auto" || actual == requested {
		return true
	}
	if strings.HasSuffix(requested, "k") {
		kib, err := strconv.ParseInt(strings.TrimSuffix(requested, "k"), 10, 64)
		if err == nil {
			return actual == strconv.FormatInt(kib*1024, 10)
		}
	}
	return false
}
func ValidateExistingCreateVolume(lun *ngxstorage.LUN, req *csi.CreateVolumeRequest, sourceSnapshot *ngxstorage.Snapshot) error {
	if lun == nil || lun.ID == "" {
		return status.Error(codes.AlreadyExists, "same-name LUN has invalid backend identity")
	}
	actualBytes := int64(lun.Size)
	requiredBytes := MinVolumeBytes
	limitBytes := int64(0)
	if req.GetCapacityRange() != nil {
		if req.GetCapacityRange().GetRequiredBytes() > 0 {
			requiredBytes = req.GetCapacityRange().GetRequiredBytes()
		}
		limitBytes = req.GetCapacityRange().GetLimitBytes()
	}
	if actualBytes <= 0 || actualBytes < requiredBytes || (limitBytes > 0 && actualBytes > limitBytes) {
		return status.Errorf(codes.AlreadyExists,
			"same-name LUN has incompatible capacity: actual=%d required=%d limit=%d",
			actualBytes, requiredBytes, limitBytes)
	}

	cloneMarker := strings.TrimSpace(lun.Clone)
	if sourceSnapshot == nil {
		if cloneMarker != "" {
			return status.Error(codes.AlreadyExists, "same-name LUN was created from a different content source")
		}
	} else {
		expectedMarkers := map[string]struct{}{
			sourceSnapshot.ID:   {},
			sourceSnapshot.Name: {},
		}
		if sourceSnapshot.VolumeName != "" && sourceSnapshot.Name != "" {
			expectedMarkers[sourceSnapshot.VolumeName+"@"+sourceSnapshot.Name] = struct{}{}
		}
		if _, ok := expectedMarkers[cloneMarker]; !ok {
			return status.Errorf(codes.AlreadyExists,
				"same-name LUN content source is incompatible with snapshot %q", sourceSnapshot.ID)
		}
	}

	opts, err := LUNCreateOptionsFromParams(req.GetParameters())
	if err != nil {
		return err
	}
	if opts.Blocksize != "" && !CompatibleBlockSize(lun.BlockSize, opts.Blocksize) {
		return status.Errorf(codes.AlreadyExists, "same-name LUN blocksize=%q, requested=%q", lun.BlockSize, opts.Blocksize)
	}
	if opts.Deduplication != "" && string(lun.Deduplication) != opts.Deduplication {
		return status.Errorf(codes.AlreadyExists, "same-name LUN deduplication=%q, requested=%q", lun.Deduplication, opts.Deduplication)
	}
	if opts.Compression != "" && string(lun.Compression) != opts.Compression {
		return status.Errorf(codes.AlreadyExists, "same-name LUN compression=%q, requested=%q", lun.Compression, opts.Compression)
	}
	if opts.ThinProvision != "" && string(lun.ThinProvision) != opts.ThinProvision {
		return status.Errorf(codes.AlreadyExists, "same-name LUN thin_provision=%q, requested=%q", lun.ThinProvision, opts.ThinProvision)
	}
	if opts.IoType != "" && lun.IoType != opts.IoType {
		return status.Errorf(codes.AlreadyExists, "same-name LUN io_type=%q, requested=%q", lun.IoType, opts.IoType)
	}
	if opts.QosPriority != 0 && int64(lun.QosPriority) != int64(opts.QosPriority) {
		return status.Errorf(codes.AlreadyExists, "same-name LUN qos_priority=%d, requested=%d", lun.QosPriority, opts.QosPriority)
	}
	return nil
}
func CreateVolumeResponse(prefix, lunID string, capacityBytes int64, name, fsType, owner string, req *csi.CreateVolumeRequest) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      VolumeID(prefix, lunID),
		CapacityBytes: capacityBytes,
		ContentSource: req.GetVolumeContentSource(),
		VolumeContext: map[string]string{
			"lun_id":            lunID,
			"name":              name,
			"fstype":            fsType,
			"owner":             owner,
			"mount_permissions": req.GetParameters()["mount_permissions"],
		},
	}}
}
func CreateSnapshotResponse(prefix string, snap *ngxstorage.Snapshot, sourceVolumeID string, fallbackSize int64) *csi.CreateSnapshotResponse {
	proto := SnapshotProto(prefix, snap)
	if proto.SourceVolumeId == "" {
		proto.SourceVolumeId = sourceVolumeID
	}
	if proto.SizeBytes == 0 {
		proto.SizeBytes = fallbackSize
	}
	return &csi.CreateSnapshotResponse{Snapshot: proto}
}
func SnapshotProto(prefix string, snap *ngxstorage.Snapshot) *csi.Snapshot {
	result := &csi.Snapshot{SnapshotId: snap.ID, ReadyToUse: snap.Ready}
	if snap.VolumeID != "" {
		result.SourceVolumeId = VolumeID(prefix, snap.VolumeID)
	}
	if created, err := strconv.ParseInt(strings.TrimSpace(snap.Created), 10, 64); err == nil && created >= 0 {
		result.CreationTime = timestamppb.New(time.Unix(created, 0))
	}
	if size, err := strconv.ParseInt(strings.TrimSpace(snap.Size), 10, 64); err == nil && size >= 0 {
		result.SizeBytes = size
	}
	return result
}
