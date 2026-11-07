package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.SystemOperationLockRepository = (*idempotencyRepository)(nil)

// These methods are only for the global system-operation lock. Ordinary request
// idempotency retains its existing reclaim, renewal, and completion semantics.
func (r *idempotencyRepository) TryReclaimSystemOperationLock(ctx context.Context, id int64, fromStatus, previousOwner, newOwner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
	return systemOperationLockChanged(r.sql.ExecContext(ctx, `
		UPDATE idempotency_records
		SET status = $2, request_fingerprint = $3, locked_until = $4,
			expires_at = $5, response_status = NULL, response_body = NULL,
			error_reason = NULL, updated_at = NOW()
		WHERE id = $1 AND status = $6 AND request_fingerprint = $7
			AND (locked_until IS NULL OR locked_until <= clock_timestamp())
			AND $4 > clock_timestamp()
	`, id, service.IdempotencyStatusProcessing, newOwner, newLockedUntil, newExpiresAt, fromStatus, previousOwner))
}

func (r *idempotencyRepository) RenewSystemOperationLock(ctx context.Context, id int64, owner string, newLockedUntil, newExpiresAt time.Time) (bool, error) {
	return systemOperationLockChanged(r.sql.ExecContext(ctx, `
		UPDATE idempotency_records
		SET locked_until = $2, expires_at = $3, updated_at = NOW()
		WHERE id = $1 AND status = $4 AND request_fingerprint = $5
			AND locked_until > clock_timestamp() AND $2 > clock_timestamp()
	`, id, newLockedUntil, newExpiresAt, service.IdempotencyStatusProcessing, owner))
}

func (r *idempotencyRepository) ReleaseSystemOperationLock(ctx context.Context, id int64, owner string, succeeded bool, responseBody, failureReason string, expiresAt time.Time) (bool, error) {
	status := service.IdempotencyStatusFailedRetryable
	var responseStatus any
	var body any
	var reason any = failureReason
	if succeeded {
		status = service.IdempotencyStatusSucceeded
		responseStatus = 200
		body = responseBody
		reason = nil
	}
	return systemOperationLockChanged(r.sql.ExecContext(ctx, `
		UPDATE idempotency_records
		SET status = $2, response_status = $3, response_body = $4,
			error_reason = $5, locked_until = NULL, expires_at = $6, updated_at = NOW()
		WHERE id = $1 AND status = $7 AND request_fingerprint = $8
	`, id, status, responseStatus, body, reason, expiresAt, service.IdempotencyStatusProcessing, owner))
}

// WithSystemOperationLockOwnership holds a row lock across the complete binary
// swap, including restore on failure. Reclaim/renew/release target the same row
// and must wait even if the old lease elapses while the process is paused.
func (r *idempotencyRepository) WithSystemOperationLockOwnership(ctx context.Context, id int64, owner string, mutation func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mutation == nil {
		return errors.New("system operation mutation is required")
	}
	db, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return service.ErrIdempotencyStoreUnavail.WithCause(errors.New("system operation lock requires transaction support"))
	}
	// Cancellation while waiting is handled by the queries below. Once the
	// callback starts, cancellation must not roll back this transaction and
	// release its row lock while filesystem operations are still running.
	criticalCtx := context.WithoutCancel(ctx)
	tx, err := db.BeginTx(criticalCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status, storedOwner string
	var lockedUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT status, request_fingerprint, locked_until
		FROM idempotency_records WHERE id = $1 FOR UPDATE
	`, id).Scan(&status, &storedOwner, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrSystemOperationLockLost
	}
	if err != nil {
		return err
	}
	// Read database wall time AFTER acquiring the row lock. NOW() is fixed at
	// transaction start and can be stale after waiting for another operation.
	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return err
	}
	if status != service.IdempotencyStatusProcessing || storedOwner != owner || !lockedUntil.Valid || !lockedUntil.Time.After(databaseNow) {
		return service.ErrSystemOperationLockLost
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mutation(criticalCtx); err != nil {
		return err
	}
	return tx.Commit()
}

func systemOperationLockChanged(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
