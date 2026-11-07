//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newSystemOperationIntegrationRecord(t *testing.T, lease time.Duration) (*idempotencyRepository, *service.IdempotencyRecord) {
	t.Helper()
	repo := &idempotencyRepository{sql: integrationDB}
	now := time.Now().UTC()
	until := now.Add(lease)
	record := &service.IdempotencyRecord{Scope: uniqueTestValue(t, "system-operation-owner"),
		IdempotencyKeyHash: hashedTestValue(t, "system-operation-key"), RequestFingerprint: hashedTestValue(t, "system-operation-owner"),
		Status: service.IdempotencyStatusProcessing, LockedUntil: &until, ExpiresAt: now.Add(time.Hour)}
	created, err := repo.CreateProcessing(context.Background(), record)
	require.NoError(t, err)
	require.True(t, created)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM idempotency_records WHERE id = $1`, record.ID)
	})
	return repo, record
}

func TestSystemOperationLockRepoReclaimRotatesOwnerAndFencesStaleTokens(t *testing.T) {
	repo, record := newSystemOperationIntegrationRecord(t, time.Minute)
	ctx := context.Background()
	oldOwner := record.RequestFingerprint
	released, err := repo.ReleaseSystemOperationLock(ctx, record.ID, oldOwner, true, `{}`, "", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, released)
	newOwner := hashedTestValue(t, "new-owner")
	reclaimed, err := repo.TryReclaimSystemOperationLock(ctx, record.ID, service.IdempotencyStatusSucceeded,
		oldOwner, newOwner, time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, reclaimed)
	got, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.Equal(t, newOwner, got.RequestFingerprint)
	require.Equal(t, service.IdempotencyStatusProcessing, got.Status)
	ok, err := repo.RenewSystemOperationLock(ctx, record.ID, newOwner, time.Now().Add(2*time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok, "the second acquisition must renew with its own new owner")
	ok, err = repo.RenewSystemOperationLock(ctx, record.ID, oldOwner, time.Now().Add(3*time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, ok)
	for _, succeeded := range []bool{true, false} {
		ok, err = repo.ReleaseSystemOperationLock(ctx, record.ID, oldOwner, succeeded, `{}`, "old-owner-failure", time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.False(t, ok, "a stale owner may neither succeed nor fail the new operation")
	}
	ok, err = repo.TryReclaimSystemOperationLock(ctx, record.ID, service.IdempotencyStatusProcessing,
		newOwner, hashedTestValue(t, "third-owner"), time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, ok, "the third acquisition must remain busy during the live lease")
	ok, err = repo.TryReclaimSystemOperationLock(ctx, record.ID, service.IdempotencyStatusProcessing,
		oldOwner, hashedTestValue(t, "wrong-old-owner"), time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSystemOperationLockRepoExpiredOwnerCannotReviveOrMutate(t *testing.T) {
	repo, record := newSystemOperationIntegrationRecord(t, -time.Second)
	ctx := context.Background()
	ok, err := repo.RenewSystemOperationLock(ctx, record.ID, record.RequestFingerprint,
		time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, ok)
	called := false
	err = repo.WithSystemOperationLockOwnership(ctx, record.ID, record.RequestFingerprint, func(context.Context) error { called = true; return nil })
	require.ErrorIs(t, err, service.ErrSystemOperationLockLost)
	require.False(t, called)
}

func TestSystemOperationLockServiceIntegrationSameOperationIDGetsNewOwner(t *testing.T) {
	ctx := context.Background()
	repo := &idempotencyRepository{sql: integrationDB}
	const scope = "admin.system.operations.global_lock"
	keyHash := service.HashIdempotencyKey("global-system-operation-lock")
	_, err := integrationDB.ExecContext(ctx, `DELETE FROM idempotency_records WHERE scope = $1 AND idempotency_key_hash = $2`, scope, keyHash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM idempotency_records WHERE scope = $1 AND idempotency_key_hash = $2`, scope, keyHash)
	})
	svc := service.NewSystemOperationLockService(repo, service.DefaultIdempotencyConfig())
	first, err := svc.Acquire(ctx, "same-operation")
	require.NoError(t, err)
	firstRow, err := repo.GetByScopeAndKeyHash(ctx, scope, keyHash)
	require.NoError(t, err)
	require.NoError(t, svc.Release(ctx, first, true, ""))
	second, err := svc.Acquire(ctx, "same-operation")
	require.NoError(t, err)
	defer func() { _ = svc.Release(ctx, second, true, "") }()
	secondRow, err := repo.GetByScopeAndKeyHash(ctx, scope, keyHash)
	require.NoError(t, err)
	require.Equal(t, first.OperationID(), second.OperationID())
	require.NotEqual(t, firstRow.RequestFingerprint, secondRow.RequestFingerprint)
	require.Len(t, secondRow.RequestFingerprint, 64)
	require.NoError(t, second.EnsureOwned(ctx))
	ok, err := repo.ReleaseSystemOperationLock(ctx, firstRow.ID, firstRow.RequestFingerprint, true, `{}`, "", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, ok)
	_, err = svc.Acquire(ctx, "third-operation")
	require.Equal(t, infraerrors.Reason(service.ErrSystemOperationBusy), infraerrors.Reason(err))
}

func TestSystemOperationLockRepoCriticalSectionBlocksReclaimPastLeaseAndCancellation(t *testing.T) {
	repo, record := newSystemOperationIntegrationRecord(t, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	finishMutation := func() { finishOnce.Do(func() { close(finish) }) }
	defer finishMutation()
	criticalDone := make(chan error, 1)
	go func() {
		criticalDone <- repo.WithSystemOperationLockOwnership(ctx, record.ID, record.RequestFingerprint, func(criticalCtx context.Context) error {
			close(entered)
			<-finish
			if err := criticalCtx.Err(); err != nil {
				return fmt.Errorf("critical mutation was canceled before completion: %w", err)
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-criticalDone:
		t.Fatalf("critical section failed before entry: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("critical section did not start")
	}
	// The old HTTP/download context can expire while the local filesystem call
	// is paused. The database row must remain locked until that call finishes.
	cancel()
	if remaining := time.Until(record.LockedUntil.Add(50 * time.Millisecond)); remaining > 0 {
		<-time.After(remaining)
	}
	conn, err := integrationDB.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var pid int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid))
	reclaimCtx, reclaimCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reclaimCancel()
	type reclaimResult struct {
		ok  bool
		err error
	}
	reclaimDone := make(chan reclaimResult, 1)
	newOwner := hashedTestValue(t, "owner-after-critical")
	go func() {
		waitingRepo := &idempotencyRepository{sql: conn}
		ok, err := waitingRepo.TryReclaimSystemOperationLock(reclaimCtx, record.ID, service.IdempotencyStatusProcessing,
			record.RequestFingerprint, newOwner, time.Now().Add(time.Minute), time.Now().Add(time.Hour))
		reclaimDone <- reclaimResult{ok: ok, err: err}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "reclaim must actually be waiting on PostgreSQL's row lock")
	select {
	case result := <-reclaimDone:
		t.Fatalf("reclaim completed during the filesystem callback: %+v", result)
	default:
	}
	finishMutation()
	require.NoError(t, <-criticalDone)
	result := <-reclaimDone
	require.NoError(t, result.err)
	require.True(t, result.ok, "reclaim may proceed only after the complete filesystem callback finishes")
	got, err := repo.GetByScopeAndKeyHash(context.Background(), record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.Equal(t, newOwner, got.RequestFingerprint)
}

func TestSystemOperationLockRepoChecksLeaseAfterWaitingForRowLock(t *testing.T) {
	_, record := newSystemOperationIntegrationRecord(t, 2*time.Second)
	blocker, err := integrationDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	var lockedID int64
	require.NoError(t, blocker.QueryRowContext(context.Background(), `SELECT id FROM idempotency_records WHERE id = $1 FOR UPDATE`, record.ID).Scan(&lockedID))
	conn, err := integrationDB.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var pid int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	called := false
	go func() {
		waitingRepo := &idempotencyRepository{sql: conn}
		done <- waitingRepo.WithSystemOperationLockOwnership(ctx, record.ID, record.RequestFingerprint, func(context.Context) error {
			called = true
			return nil
		})
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond)
	if remaining := time.Until(record.LockedUntil.Add(50 * time.Millisecond)); remaining > 0 {
		<-time.After(remaining)
	}
	require.NoError(t, blocker.Commit())
	require.ErrorIs(t, <-done, service.ErrSystemOperationLockLost)
	require.False(t, called, "a transaction started before lease expiry cannot use stale NOW() after the row lock wait")
}
