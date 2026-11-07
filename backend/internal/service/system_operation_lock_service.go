package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	systemOperationLockScope = "admin.system.operations.global_lock"
	systemOperationLockKey   = "global-system-operation-lock"
)

var (
	ErrSystemOperationBusy     = infraerrors.Conflict("SYSTEM_OPERATION_BUSY", "another system operation is in progress")
	ErrSystemOperationLockLost = infraerrors.Conflict("SYSTEM_OPERATION_LOCK_LOST", "system operation lock ownership was lost")
)

// SystemOperationLockRepository is deliberately separate from request idempotency.
// An operation owner is a fresh, private token, never the retryable operation ID.
// Implementations must serialize the ownership callback with reclaim and release.
// The callback contains only the short binary swap, never network downloads.
type SystemOperationLockRepository interface {
	TryReclaimSystemOperationLock(ctx context.Context, id int64, fromStatus, previousOwner, newOwner string, newLockedUntil, newExpiresAt time.Time) (bool, error)
	RenewSystemOperationLock(ctx context.Context, id int64, owner string, newLockedUntil, newExpiresAt time.Time) (bool, error)
	ReleaseSystemOperationLock(ctx context.Context, id int64, owner string, succeeded bool, responseBody, failureReason string, expiresAt time.Time) (bool, error)
	WithSystemOperationLockOwnership(ctx context.Context, id int64, owner string, mutation func(context.Context) error) error
}

type systemOperationLockContextKey struct{}

type SystemOperationLock struct {
	recordID    int64
	operationID string
	ownerToken  string
	service     *SystemOperationLockService

	mu            sync.Mutex
	lockedUntil   time.Time
	ownershipLost bool
	released      bool
	lostCh        chan struct{}
	stopOnce      sync.Once
	stopCh        chan struct{}
}

func (l *SystemOperationLock) OperationID() string {
	if l == nil {
		return ""
	}
	return l.operationID
}

// Context carries the mutation fence and cancels downloads when ownership is lost.
// The caller must cancel the returned context when the operation finishes.
func (l *SystemOperationLock) Context(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(context.WithValue(parent, systemOperationLockContextKey{}, l))
	if l == nil {
		cancel()
		return ctx, cancel
	}
	go func() {
		select {
		case <-l.lostCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (l *SystemOperationLock) markLostLocked() {
	if l.ownershipLost {
		return
	}
	l.ownershipLost = true
	if l.lostCh != nil {
		close(l.lostCh)
	}
}

// EnsureOwned performs a synchronous owner CAS. Expired leases cannot revive.
// A transient store error blocks mutation, even while a renewal retry is possible.
func (l *SystemOperationLock) EnsureOwned(ctx context.Context) error {
	if l == nil {
		return ErrIdempotencyStoreUnavail
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ensureOwnedLocked(ctx)
}

func (l *SystemOperationLock) ensureOwnedLocked(ctx context.Context) error {
	if l.released || l.ownershipLost {
		return ErrSystemOperationLockLost
	}
	if l.service == nil || l.service.lockRepo == nil || l.ownerToken == "" {
		return ErrIdempotencyStoreUnavail
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	lockedUntil := now.Add(l.service.lease)
	ok, err := l.service.lockRepo.RenewSystemOperationLock(ctx, l.recordID, l.ownerToken, lockedUntil, now.Add(l.service.ttl))
	if err != nil {
		if !l.lockedUntil.After(time.Now()) {
			l.markLostLocked()
		}
		return ErrIdempotencyStoreUnavail.WithCause(err)
	}
	if !ok {
		l.markLostLocked()
		return ErrSystemOperationLockLost
	}
	l.lockedUntil = lockedUntil
	return nil
}

// withOwnership holds the same database row lock across the entire filesystem
// swap, including any restore. The local mutex also keeps our renewal/release
// calls from waiting on our own row lock or canceling the critical section.
func (l *SystemOperationLock) withOwnership(ctx context.Context, mutation func(context.Context) error) error {
	if l == nil {
		return ErrIdempotencyStoreUnavail
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureOwnedLocked(ctx); err != nil {
		return err
	}
	err := l.service.lockRepo.WithSystemOperationLockOwnership(ctx, l.recordID, l.ownerToken, mutation)
	if errors.Is(err, ErrSystemOperationLockLost) || !l.lockedUntil.After(time.Now()) {
		l.markLostLocked()
	}
	return err
}

func systemOperationLockFromContext(ctx context.Context) *SystemOperationLock {
	if ctx == nil {
		return nil
	}
	lock, _ := ctx.Value(systemOperationLockContextKey{}).(*SystemOperationLock)
	return lock
}

func ensureSystemOperationLockOwned(ctx context.Context) error {
	return systemOperationLockFromContext(ctx).EnsureOwned(ctx)
}

func withSystemOperationLockOwnership(ctx context.Context, mutation func(context.Context) error) error {
	return systemOperationLockFromContext(ctx).withOwnership(ctx, mutation)
}

type SystemOperationLockService struct {
	repo          IdempotencyRepository
	lockRepo      SystemOperationLockRepository
	lease         time.Duration
	renewInterval time.Duration
	ttl           time.Duration
}

func NewSystemOperationLockService(repo IdempotencyRepository, cfg IdempotencyConfig) *SystemOperationLockService {
	lease := cfg.ProcessingTimeout
	if lease <= 0 {
		lease = 30 * time.Second
	}
	renewInterval := lease / 3
	if renewInterval < time.Millisecond {
		renewInterval = time.Millisecond
	}
	ttl := cfg.SystemOperationTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	lockRepo, _ := repo.(SystemOperationLockRepository)
	return &SystemOperationLockService{repo: repo, lockRepo: lockRepo, lease: lease, renewInterval: renewInterval, ttl: ttl}
}

func (s *SystemOperationLockService) Acquire(ctx context.Context, operationID string) (*SystemOperationLock, error) {
	if s == nil || s.repo == nil || s.lockRepo == nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	if operationID == "" {
		return nil, infraerrors.BadRequest("SYSTEM_OPERATION_ID_REQUIRED", "operation id is required")
	}

	// request_fingerprint is VARCHAR(64). A 256-bit random token fits exactly
	// and distinguishes two attempts even when their public operation IDs match.
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, ErrIdempotencyStoreUnavail.WithCause(err)
	}
	ownerToken := hex.EncodeToString(token[:])
	now := time.Now()
	expiresAt := now.Add(s.ttl)
	lockedUntil := now.Add(s.lease)
	keyHash := HashIdempotencyKey(systemOperationLockKey)
	record := &IdempotencyRecord{Scope: systemOperationLockScope, IdempotencyKeyHash: keyHash,
		RequestFingerprint: ownerToken, Status: IdempotencyStatusProcessing,
		LockedUntil: &lockedUntil, ExpiresAt: expiresAt}
	owner, err := s.repo.CreateProcessing(ctx, record)
	if err != nil {
		return nil, ErrIdempotencyStoreUnavail.WithCause(err)
	}
	if !owner {
		existing, getErr := s.repo.GetByScopeAndKeyHash(ctx, systemOperationLockScope, keyHash)
		if getErr != nil {
			return nil, ErrIdempotencyStoreUnavail.WithCause(getErr)
		}
		if existing == nil {
			return nil, ErrIdempotencyStoreUnavail
		}
		if existing.Status == IdempotencyStatusProcessing && existing.LockedUntil != nil && existing.LockedUntil.After(now) {
			return nil, s.busyError("", existing.LockedUntil, now)
		}
		reclaimed, reclaimErr := s.lockRepo.TryReclaimSystemOperationLock(ctx, existing.ID, existing.Status,
			existing.RequestFingerprint, ownerToken, lockedUntil, expiresAt)
		if reclaimErr != nil {
			return nil, ErrIdempotencyStoreUnavail.WithCause(reclaimErr)
		}
		if !reclaimed {
			latest, getErr := s.repo.GetByScopeAndKeyHash(ctx, systemOperationLockScope, keyHash)
			if getErr != nil {
				return nil, ErrIdempotencyStoreUnavail.WithCause(getErr)
			}
			if latest != nil {
				return nil, s.busyError("", latest.LockedUntil, now)
			}
			return nil, ErrSystemOperationBusy
		}
		record.ID = existing.ID
	}
	if record.ID == 0 {
		return nil, ErrIdempotencyStoreUnavail
	}
	lock := &SystemOperationLock{recordID: record.ID, operationID: operationID, ownerToken: ownerToken,
		service: s, lockedUntil: lockedUntil, lostCh: make(chan struct{}), stopCh: make(chan struct{})}
	if err := lock.EnsureOwned(ctx); err != nil {
		return nil, err
	}
	go s.renewLoop(lock)
	return lock, nil
}

func (s *SystemOperationLockService) Release(ctx context.Context, lock *SystemOperationLock, succeeded bool, failureReason string) error {
	if s == nil || s.repo == nil || lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.released {
		return nil
	}
	if s.lockRepo == nil || lock.service != s || lock.ownerToken == "" {
		return ErrIdempotencyStoreUnavail
	}
	lock.released = true
	lock.stopOnce.Do(func() { close(lock.stopCh) })
	lock.markLostLocked()
	if ctx == nil {
		ctx = context.Background()
	}
	if failureReason == "" {
		failureReason = "SYSTEM_OPERATION_FAILED"
	}
	body, _ := json.Marshal(map[string]any{"operation_id": lock.operationID, "released": true})
	ok, err := s.lockRepo.ReleaseSystemOperationLock(ctx, lock.recordID, lock.ownerToken, succeeded,
		string(body), failureReason, time.Now().Add(s.ttl))
	if err != nil {
		return err
	}
	if !ok {
		return ErrSystemOperationLockLost
	}
	return nil
}

func (s *SystemOperationLockService) renewLoop(lock *SystemOperationLock) {
	ticker := time.NewTicker(s.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := lock.EnsureOwned(ctx)
			cancel()
			if err != nil {
				lock.mu.Lock()
				lost := lock.ownershipLost || lock.released
				lock.mu.Unlock()
				logger.LegacyPrintf("service.system_operation_lock", "[SystemOperationLock] renew failed operation_id=%s ownership_lost=%t err=%v", lock.operationID, lost, err)
				if lost {
					return
				}
				// Retry transient failures only while the known lease is live.
			}
		case <-lock.stopCh:
			return
		}
	}
}

func (s *SystemOperationLockService) busyError(operationID string, lockedUntil *time.Time, now time.Time) error {
	metadata := make(map[string]string)
	if operationID != "" {
		metadata["operation_id"] = operationID
	}
	if lockedUntil != nil {
		sec := int(lockedUntil.Sub(now).Seconds())
		if sec <= 0 {
			sec = 1
		}
		metadata["retry_after"] = strconv.Itoa(sec)
	}
	if len(metadata) == 0 {
		return ErrSystemOperationBusy
	}
	return ErrSystemOperationBusy.WithMetadata(metadata)
}
