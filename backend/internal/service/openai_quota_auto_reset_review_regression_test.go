package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOpenAIQuotaAutoResetReviewSuccessWithCreditSurvivesLowUsageScan(t *testing.T) {
	svc, repo, quota := newResetThresholdTestFixture(t, nil, 2)
	repo.setExtraForTest(OpenAIAutoResetCredit5hThresholdExtraKey, 1.0)
	repo.setExtraForTest(OpenAIAutoResetCredit7dThresholdExtraKey, 1.0)
	repo.setExtraForTest("auto_pause_5h_disabled", true)
	repo.setExtraForTest("auto_pause_7d_threshold", 0.98)
	repo.setExtraForTest("codex_7d_used_percent", 100.0)
	quota.usage.RateLimit.SecondaryWindow.UsedPercent = 100
	firstExpiry := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	secondExpiry := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	quota.usage.RateLimitResetCredits.Credits = []OpenAIRateLimitResetCreditDetail{{ExpiresAt: firstExpiry}, {ExpiresAt: secondExpiry}}
	quota.usage.autoResetCandidates = []openAIAutoResetCreditCandidate{{ID: "completed-card", ExpiresAt: firstExpiry}, {ID: "remaining-card", ExpiresAt: secondExpiry}}
	quota.queryHook = func() {
		if quota.resetCalls.Load() > 0 {
			quota.usage.RateLimit.SecondaryWindow.UsedPercent = 0
			quota.usage.RateLimitResetCredits = &OpenAIRateLimitResetCredits{
				AvailableCount: 1,
				Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: secondExpiry}},
			}
			quota.usage.autoResetCandidates = []openAIAutoResetCreditCandidate{{ID: "remaining-card", ExpiresAt: secondExpiry}}
		}
	}

	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, int32(1), quota.resetCalls.Load())
	completed := repo.stateForTest()
	require.Equal(t, OpenAIAutoResetStatusSuccess, completed.Status)
	require.Equal(t, 1, completed.AvailableCount)
	require.NotEmpty(t, completed.LastResultAt)
	require.NotEmpty(t, completed.AttemptCreditHash)

	// CachePostResetSnapshot is a no-op in this test double; publish the actual
	// post-reset low usage before exercising the next ordinary scanner pass.
	repo.setExtraForTest("codex_7d_used_percent", 0.0)
	repo.setExtraForTest("codex_usage_updated_at", time.Now().UTC().Format(time.RFC3339))
	queries := quota.queryCalls.Load()
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	afterScan := repo.stateForTest()
	require.Equal(t, OpenAIAutoResetStatusSuccess, afterScan.Status)
	require.Empty(t, afterScan.TriggerWindow)
	require.Equal(t, completed.LastResultAt, afterScan.LastResultAt)
	require.Equal(t, completed.AttemptCreditHash, afterScan.AttemptCreditHash)
	require.Equal(t, completed.AttemptCycleHash, afterScan.AttemptCycleHash)
	require.Equal(t, queries, quota.queryCalls.Load())

	// The next request must bridge 98% to 100% without erasing success history
	// or waiting for the ten-minute credit snapshot TTL.
	repo.setExtraForTest("codex_7d_used_percent", 99.0)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, queries, quota.queryCalls.Load())
	account, err := repo.GetByID(context.Background(), 501)
	require.NoError(t, err)
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
	require.False(t, paused, "a fresh successful redemption with a remaining credit must keep scheduling before the reset threshold")

	for _, mode := range []string{"no credit", "stale credit", "unresolved", "reset threshold"} {
		t.Run(mode, func(t *testing.T) {
			copy := *account
			copy.Extra = cloneOpenAIAutoResetExtra(account.Extra)
			state := *afterScan
			switch mode {
			case "no credit":
				state.AvailableCount = 0
			case "stale credit":
				state.CheckedAt = time.Now().Add(-openAIAutoResetSnapshotTTL - time.Minute).UTC().Format(time.RFC3339)
			case "unresolved":
				state.Status = OpenAIAutoResetStatusFailed
			case "reset threshold":
				copy.Extra["codex_7d_used_percent"] = 100.0
			}
			copy.Extra[OpenAIAutoResetCreditStateExtraKey] = &state
			paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), &copy)
			require.True(t, paused)
		})
	}
}

type reviewAutoResetAccountRepo struct {
	*autoResetTestAccountRepo
	getCalls  int
	beforeGet func(int) error
}

func (r *reviewAutoResetAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.getCalls++
	if r.beforeGet != nil {
		if err := r.beforeGet(r.getCalls); err != nil {
			return nil, err
		}
	}
	return r.autoResetTestAccountRepo.GetByID(ctx, id)
}

func reviewAutoResetPrepareReplacement(repo *autoResetTestAccountRepo, quota *autoResetTestQuota) {
	repo.mu.Lock()
	repo.account.Status = StatusActive
	repo.account.Schedulable = true
	repo.mu.Unlock()
	repo.setExtraForTest(OpenAIAutoResetCreditEnabledExtraKey, true)
	repo.setExtraForTest(OpenAIAutoResetCreditExpiryEnabledExtraKey, true)
	repo.setExtraForTest(OpenAIAutoResetCreditExpiryMinutesExtraKey, 60)
	expiresAt := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	quota.usage.RateLimitResetCredits = &OpenAIRateLimitResetCredits{
		AvailableCount: 1,
		Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: expiresAt}},
	}
	quota.usage.autoResetCandidates = []openAIAutoResetCreditCandidate{{ID: "replacement-card", ExpiresAt: expiresAt}}
	quota.usage.RateLimit.SecondaryWindow.ResetAt += 24 * 60 * 60
	repo.setExtraForTest(openaiQuotaResetCreditsKey, quota.usage.RateLimitResetCredits)
}

func TestOpenAIQuotaAutoResetReviewPreDispatchCancellationDoesNotPinNewCredit(t *testing.T) {
	for _, mode := range []string{"read error", "disabled", "inactive", "unschedulable", "expiry policy disabled", "lead shortened", "expired"} {
		t.Run(mode, func(t *testing.T) {
			svc, repo, quota := newResetExpiryTestFixture(t)
			readErr := errors.New("final account read failed")
			var expiresAt time.Time
			if mode == "expired" {
				// Block only the last repository read until a previously valid
				// candidate expires, without changing clocks or sleeping blindly.
				expiresAt = time.Now().Add(2 * time.Second)
				expiry := expiresAt.UTC().Format(time.RFC3339Nano)
				quota.usage.RateLimitResetCredits.Credits[0].ExpiresAt = expiry
				quota.usage.autoResetCandidates[0].ExpiresAt = expiry
				repo.setExtraForTest(OpenAIAutoResetCredit7dDisabledExtraKey, false)
				repo.setExtraForTest("codex_7d_used_percent", 100.0)
				quota.usage.RateLimit.SecondaryWindow.UsedPercent = 100
			}
			wrapped := &reviewAutoResetAccountRepo{autoResetTestAccountRepo: repo}
			wrapped.beforeGet = func(call int) error {
				if call != 3 {
					return nil
				}
				switch mode {
				case "read error":
					return readErr
				case "disabled":
					repo.setExtraForTest(OpenAIAutoResetCreditEnabledExtraKey, false)
				case "inactive":
					repo.mu.Lock()
					repo.account.Status = "disabled"
					repo.mu.Unlock()
				case "unschedulable":
					repo.mu.Lock()
					repo.account.Schedulable = false
					repo.mu.Unlock()
				case "expiry policy disabled":
					repo.setExtraForTest(OpenAIAutoResetCreditExpiryEnabledExtraKey, false)
				case "lead shortened":
					repo.setExtraForTest(OpenAIAutoResetCreditExpiryMinutesExtraKey, 1)
				case "expired":
					timer := time.NewTimer(time.Until(expiresAt))
					defer timer.Stop()
					<-timer.C
				}
				return nil
			}
			svc.accountRepo = wrapped

			err := svc.evaluateAccount(context.Background(), 501)
			if mode == "read error" {
				require.ErrorIs(t, err, readErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 3, wrapped.getCalls, "the last pre-dispatch account check must be reached")
			require.Zero(t, quota.resetCalls.Load())
			state := repo.stateForTest()
			require.NotNil(t, state)
			require.False(t, openAIAutoResetAttemptPending(state), "a cancelled operation that never dispatched must not acquire a credit pin")
			require.Empty(t, state.AttemptCreditHash)
			require.Empty(t, state.AttemptCycleHash)

			// The original card is absent in the next quota cycle. No database
			// state cleanup is needed before a newly available card can be used.
			wrapped.beforeGet = nil
			reviewAutoResetPrepareReplacement(repo, quota)
			require.NoError(t, svc.evaluateAccount(context.Background(), 501))
			require.Equal(t, int32(1), quota.resetCalls.Load())
			require.Equal(t, "replacement-card", quota.resetArgs[0][0])
			require.Equal(t, OpenAIAutoResetStatusSuccess, repo.stateForTest().Status)
		})
	}
}

func TestOpenAIQuotaAutoResetReviewFinalCancellationPreservesUncertainAttempt(t *testing.T) {
	for _, status := range []string{OpenAIAutoResetStatusChecking, OpenAIAutoResetStatusResetting, OpenAIAutoResetStatusFailed} {
		t.Run(status, func(t *testing.T) {
			svc, repo, quota := newResetExpiryTestFixture(t)
			pending := &OpenAIAutoResetCreditState{
				Status:            status,
				TriggerWindow:     "expiry",
				AvailableCount:    1,
				CheckedAt:         time.Now().UTC().Format(time.RFC3339),
				AttemptCycleHash:  "original-uncertain-cycle",
				AttemptCreditHash: shortOpenAIAutoResetHash("expiry-card"),
			}
			repo.setExtraForTest(OpenAIAutoResetCreditStateExtraKey, pending)
			wrapped := &reviewAutoResetAccountRepo{autoResetTestAccountRepo: repo}
			wrapped.beforeGet = func(call int) error {
				if call == 3 {
					repo.setExtraForTest(OpenAIAutoResetCreditEnabledExtraKey, false)
				}
				return nil
			}
			svc.accountRepo = wrapped

			require.NoError(t, svc.evaluateAccount(context.Background(), 501))
			require.Equal(t, 3, wrapped.getCalls)
			state := repo.stateForTest()
			require.True(t, openAIAutoResetAttemptPending(state))
			require.Equal(t, pending.AttemptCreditHash, state.AttemptCreditHash)
			require.Equal(t, pending.AttemptCycleHash, state.AttemptCycleHash)
			require.Zero(t, quota.resetCalls.Load())

			wrapped.beforeGet = nil
			reviewAutoResetPrepareReplacement(repo, quota)
			err := svc.evaluateAccount(context.Background(), 501)
			require.Error(t, err)
			require.Equal(t, "OPENAI_AUTO_RESET_ORIGINAL_CREDIT_UNAVAILABLE", infraerrors.Reason(err))
			require.Zero(t, quota.resetCalls.Load(), "an unresolved dispatched operation must never fall through to a different card")
			require.Equal(t, pending.AttemptCreditHash, repo.stateForTest().AttemptCreditHash)
			require.Equal(t, pending.AttemptCycleHash, repo.stateForTest().AttemptCycleHash)
		})
	}
}

type reviewAutoResetQuota struct {
	*autoResetTestQuota
	beforeReset func()
}

func (q *reviewAutoResetQuota) ResetCreditTargeted(ctx context.Context, id int64, creditID, redeemRequestID string) (*OpenAIQuotaResetResult, error) {
	q.beforeReset()
	return q.autoResetTestQuota.ResetCreditTargeted(ctx, id, creditID, redeemRequestID)
}

func TestOpenAIQuotaAutoResetReviewDispatchPersistsAttemptBeforeUpstream(t *testing.T) {
	svc, repo, quota := newResetExpiryTestFixture(t)
	observed := false
	svc.quota = &reviewAutoResetQuota{autoResetTestQuota: quota, beforeReset: func() {
		state := repo.stateForTest()
		require.NotNil(t, state)
		require.Equal(t, OpenAIAutoResetStatusResetting, state.Status)
		require.True(t, openAIAutoResetAttemptPending(state))
		require.Equal(t, shortOpenAIAutoResetHash("expiry-card"), state.AttemptCreditHash)
		require.NotEmpty(t, state.AttemptCycleHash)
		observed = true
	}}

	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.True(t, observed)
	require.Equal(t, int32(1), quota.resetCalls.Load())
	require.Equal(t, OpenAIAutoResetStatusSuccess, repo.stateForTest().Status)
}
