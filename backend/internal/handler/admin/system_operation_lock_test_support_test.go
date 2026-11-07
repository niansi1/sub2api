package admin

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Lock-specific methods mirror the production owner/status/lease CAS. Do not
// change the ordinary request-idempotency mock's existing methods to implement
// lock behavior: those APIs deliberately have different completion semantics.
func (r *memoryIdempotencyRepoStub) TryReclaimSystemOperationLock(ctx context.Context, id int64, fromStatus, previousOwner, newOwner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
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
		rec.Status = service.IdempotencyStatusProcessing
		rec.RequestFingerprint = newOwner
		rec.LockedUntil = &newLockedUntil
		rec.ExpiresAt = newExpiresAt
		rec.ResponseStatus, rec.ResponseBody, rec.ErrorReason = nil, nil, nil
		rec.UpdatedAt = now
		return true, nil
	}
	return false, nil
}

func (r *memoryIdempotencyRepoStub) RenewSystemOperationLock(ctx context.Context, id int64, owner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
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
		if rec.Status != service.IdempotencyStatusProcessing || rec.RequestFingerprint != owner ||
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

func (r *memoryIdempotencyRepoStub) ReleaseSystemOperationLock(ctx context.Context, id int64, owner string, succeeded bool, responseBody, failureReason string, expiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != service.IdempotencyStatusProcessing || rec.RequestFingerprint != owner {
			return false, nil
		}
		rec.Status = service.IdempotencyStatusFailedRetryable
		rec.ResponseStatus, rec.ResponseBody = nil, nil
		rec.ErrorReason = &failureReason
		if succeeded {
			rec.Status = service.IdempotencyStatusSucceeded
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

func (r *memoryIdempotencyRepoStub) WithSystemOperationLockOwnership(ctx context.Context, id int64, owner string, mutation func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.data {
		if rec.ID != id {
			continue
		}
		if rec.Status != service.IdempotencyStatusProcessing || rec.RequestFingerprint != owner ||
			rec.LockedUntil == nil || !rec.LockedUntil.After(time.Now()) {
			return service.ErrSystemOperationLockLost
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return mutation(context.WithoutCancel(ctx))
	}
	return service.ErrSystemOperationLockLost
}
