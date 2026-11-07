package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Lock-specific methods mirror the production owner/status/lease CAS. Do not
// change the ordinary request-idempotency mock's existing methods to implement
// lock behavior: those APIs deliberately have different completion semantics.
func (r *inMemoryIdempotencyRepo) TryReclaimSystemOperationLock(ctx context.Context, id int64, fromStatus, previousOwner, newOwner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != fromStatus || rec.RequestFingerprint != previousOwner ||
			(rec.LockedUntil != nil && rec.LockedUntil.After(now)) || !newLockedUntil.After(now) {
			return false, nil
		}
		rec.Status = IdempotencyStatusProcessing
		rec.RequestFingerprint = newOwner
		rec.LockedUntil = &newLockedUntil
		rec.ExpiresAt = newExpiresAt
		rec.ResponseStatus, rec.ResponseBody, rec.ErrorReason = nil, nil, nil
		rec.UpdatedAt = now
		return true, nil
	}
	return false, nil
}

func (r *inMemoryIdempotencyRepo) RenewSystemOperationLock(ctx context.Context, id int64, owner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != IdempotencyStatusProcessing || rec.RequestFingerprint != owner ||
			rec.LockedUntil == nil || !rec.LockedUntil.After(now) || !newLockedUntil.After(now) {
			return false, nil
		}
		rec.LockedUntil = &newLockedUntil
		rec.ExpiresAt = newExpiresAt
		rec.UpdatedAt = now
		return true, nil
	}
	return false, nil
}

func (r *inMemoryIdempotencyRepo) ReleaseSystemOperationLock(ctx context.Context, id int64, owner string, succeeded bool, responseBody, failureReason string, expiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != IdempotencyStatusProcessing || rec.RequestFingerprint != owner {
			return false, nil
		}
		rec.Status = IdempotencyStatusFailedRetryable
		rec.ResponseStatus, rec.ResponseBody = nil, nil
		rec.ErrorReason = &failureReason
		if succeeded {
			rec.Status = IdempotencyStatusSucceeded
			status := 200
			rec.ResponseStatus, rec.ResponseBody, rec.ErrorReason = &status, &responseBody, nil
		}
		rec.LockedUntil = nil
		rec.ExpiresAt = expiresAt
		rec.UpdatedAt = time.Now()
		return true, nil
	}
	return false, nil
}

func (r *inMemoryIdempotencyRepo) WithSystemOperationLockOwnership(ctx context.Context, id int64, owner string, mutation func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != IdempotencyStatusProcessing || rec.RequestFingerprint != owner ||
			rec.LockedUntil == nil || !rec.LockedUntil.After(time.Now()) {
			return ErrSystemOperationLockLost
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return mutation(context.WithoutCancel(ctx))
	}
	return ErrSystemOperationLockLost
}

type idempotencyOnlySystemLockRepo struct{ IdempotencyRepository }

func TestSystemOperationLockRejectsRepositoryWithoutOwnerCapability(t *testing.T) {
	repo := &idempotencyOnlySystemLockRepo{newInMemoryIdempotencyRepo()}
	svc := NewSystemOperationLockService(repo, DefaultIdempotencyConfig())
	_, err := svc.Acquire(context.Background(), "op")
	require.Equal(t, infraerrors.Reason(ErrIdempotencyStoreUnavail), infraerrors.Reason(err))
}

func TestSystemOperationLockReusedRecordRenewsWithNewOwner(t *testing.T) {
	repo := newInMemoryIdempotencyRepo()
	svc := NewSystemOperationLockService(repo, DefaultIdempotencyConfig())
	first, err := svc.Acquire(context.Background(), "first")
	require.NoError(t, err)
	require.NoError(t, svc.Release(context.Background(), first, true, ""))
	second, err := svc.Acquire(context.Background(), "second")
	require.NoError(t, err)
	defer func() { _ = svc.Release(context.Background(), second, true, "") }()
	require.NotEqual(t, first.ownerToken, second.ownerToken)
	require.Len(t, second.ownerToken, 64)
	require.Equal(t, "second", second.OperationID())
	require.NoError(t, second.EnsureOwned(context.Background()))
	got, err := repo.GetByScopeAndKeyHash(context.Background(), systemOperationLockScope, HashIdempotencyKey(systemOperationLockKey))
	require.NoError(t, err)
	require.Equal(t, second.ownerToken, got.RequestFingerprint)
	_, err = svc.Acquire(context.Background(), "third")
	require.Equal(t, infraerrors.Reason(ErrSystemOperationBusy), infraerrors.Reason(err))
}

func TestSystemOperationLockSameOperationIDCannotReuseAnOldOwner(t *testing.T) {
	repo := newInMemoryIdempotencyRepo()
	svc := NewSystemOperationLockService(repo, DefaultIdempotencyConfig())
	first, err := svc.Acquire(context.Background(), "same-operation")
	require.NoError(t, err)
	first.stopOnce.Do(func() { close(first.stopCh) })
	repo.mu.Lock()
	for _, rec := range repo.data {
		expired := time.Now().Add(-time.Second)
		rec.LockedUntil = &expired
	}
	repo.mu.Unlock()
	second, err := svc.Acquire(context.Background(), "same-operation")
	require.NoError(t, err)
	defer func() { _ = svc.Release(context.Background(), second, true, "") }()
	require.NotEqual(t, first.ownerToken, second.ownerToken)
	require.Equal(t, first.OperationID(), second.OperationID())
	require.ErrorIs(t, first.EnsureOwned(context.Background()), ErrSystemOperationLockLost)
	require.ErrorIs(t, svc.Release(context.Background(), first, true, ""), ErrSystemOperationLockLost)
	require.NoError(t, second.EnsureOwned(context.Background()))
	got, err := repo.GetByScopeAndKeyHash(context.Background(), systemOperationLockScope, HashIdempotencyKey(systemOperationLockKey))
	require.NoError(t, err)
	require.Equal(t, IdempotencyStatusProcessing, got.Status)
	require.Equal(t, second.ownerToken, got.RequestFingerprint)
}

func TestSystemOperationLockDoesNotReviveExpiredLease(t *testing.T) {
	repo := newInMemoryIdempotencyRepo()
	svc := NewSystemOperationLockService(repo, DefaultIdempotencyConfig())
	lock, err := svc.Acquire(context.Background(), "expired")
	require.NoError(t, err)
	defer func() { _ = svc.Release(context.Background(), lock, false, "") }()
	ctx, cancel := lock.Context(context.Background())
	defer cancel()
	repo.mu.Lock()
	for _, rec := range repo.data {
		expired := time.Now().Add(-time.Second)
		rec.LockedUntil = &expired
	}
	repo.mu.Unlock()
	require.ErrorIs(t, lock.EnsureOwned(context.Background()), ErrSystemOperationLockLost)
	require.Eventually(t, func() bool { return ctx.Err() != nil }, time.Second, time.Millisecond)
	require.ErrorIs(t, lock.EnsureOwned(context.Background()), ErrSystemOperationLockLost)
}

func ownedBinaryMutationContext(t *testing.T) (context.Context, *inMemoryIdempotencyRepo) {
	t.Helper()
	repo := newInMemoryIdempotencyRepo()
	svc := NewSystemOperationLockService(repo, DefaultIdempotencyConfig())
	lock, err := svc.Acquire(context.Background(), "test-binary-swap")
	require.NoError(t, err)
	ctx, cancel := lock.Context(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = svc.Release(context.Background(), lock, true, "") })
	return ctx, repo
}

func writeMutationFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	exePath, newPath := filepath.Join(dir, "current"), filepath.Join(dir, "downloaded")
	require.NoError(t, os.WriteFile(exePath, []byte("old-current"), 0755))
	require.NoError(t, os.WriteFile(exePath+".backup", []byte("old-backup"), 0755))
	require.NoError(t, os.WriteFile(newPath, []byte("new-current"), 0755))
	return exePath, newPath
}

func requireMutationFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(data))
}

func TestUpdateBinaryReplacementRequiresLiveOwnership(t *testing.T) {
	for _, mode := range []string{"missing-fence", "lost-owner", "expired-owner"} {
		t.Run(mode, func(t *testing.T) {
			exePath, newPath := writeMutationFixture(t)
			ctx := context.Background()
			if mode != "missing-fence" {
				var repo *inMemoryIdempotencyRepo
				ctx, repo = ownedBinaryMutationContext(t)
				repo.mu.Lock()
				for _, rec := range repo.data {
					if mode == "lost-owner" {
						rec.RequestFingerprint = "new-owner"
					} else {
						expired := time.Now().Add(-time.Second)
						rec.LockedUntil = &expired
					}
				}
				repo.mu.Unlock()
			}
			require.Error(t, replaceUpdateBinary(ctx, exePath, newPath))
			requireMutationFile(t, exePath, "old-current")
			requireMutationFile(t, exePath+".backup", "old-backup")
			requireMutationFile(t, newPath, "new-current")
		})
	}
}

func TestUpdateBinaryReplacementAndRestoreStayInOwnedSection(t *testing.T) {
	t.Run("successful-swap", func(t *testing.T) {
		ctx, _ := ownedBinaryMutationContext(t)
		exePath, newPath := writeMutationFixture(t)
		require.NoError(t, replaceUpdateBinary(ctx, exePath, newPath))
		requireMutationFile(t, exePath, "new-current")
		requireMutationFile(t, exePath+".backup", "old-current")
	})
	t.Run("restore-after-install-fails", func(t *testing.T) {
		ctx, _ := ownedBinaryMutationContext(t)
		exePath, newPath := writeMutationFixture(t)
		require.NoError(t, os.Remove(newPath))
		err := replaceUpdateBinary(ctx, exePath, newPath)
		require.ErrorContains(t, err, "replace failed (restored backup)")
		requireMutationFile(t, exePath, "old-current")
	})
}

func TestLocalRollbackRequiresOwnedCriticalSection(t *testing.T) {
	exePath, _ := writeMutationFixture(t)
	require.Error(t, rollbackUpdateBinary(context.Background(), exePath))
	requireMutationFile(t, exePath, "old-current")
	requireMutationFile(t, exePath+".backup", "old-backup")
	ctx, _ := ownedBinaryMutationContext(t)
	require.NoError(t, rollbackUpdateBinary(ctx, exePath))
	requireMutationFile(t, exePath, "old-backup")
}
