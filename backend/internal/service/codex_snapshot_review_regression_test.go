//go:build unit

package service

import (
	"context"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexSnapshotReviewAccountOverrideBypassesThrottleAtCrossing(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 4)}
	svc := &OpenAIGatewayService{accountRepo: repo, codexSnapshotThrottle: newAccountWriteThrottle(time.Hour)}
	account := &Account{ID: 801, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"account_scheduling_threshold": 60}}
	require.Equal(t, 60.0, svc.codexSnapshotBypassPercent(context.Background(), account))
	require.Equal(t, 90.0, svc.codexSnapshotBypassPercent(context.Background()))
	for _, used := range []string{"59", "65"} {
		headers := http.Header{"X-Codex-Primary-Used-Percent": {used}, "X-Codex-Primary-Window-Minutes": {"300"}}
		svc.UpdateCodexUsageSnapshotFromHeaders(context.Background(), account.ID, headers, account)
		select {
		case updates := <-repo.updateExtraCh:
			account.Extra = updates
		case <-time.After(2 * time.Second):
			t.Fatal("account override crossing was lost to the normal snapshot write throttle")
		}
	}
	require.Equal(t, 65.0, account.Extra["codex_5h_used_percent"])
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 100}, time.Now())
	require.True(t, decision.ShouldPause, "the account override must win over a disabled global threshold")
	require.Equal(t, 60, decision.ThresholdPercent)
}

func TestCodexSnapshotReviewWSKeepsObservationTimeWithAccountOverride(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &OpenAIGatewayService{accountRepo: repo, codexSnapshotThrottle: newAccountWriteThrottle(time.Hour)}
	account := &Account{ID: 802, Credentials: map[string]any{"account_scheduling_threshold": 60}}
	now := time.Now().UTC()
	require.True(t, svc.codexSnapshotThrottle.Allow(account.ID, now))
	observedAt := now.Add(-3 * time.Hour)
	headers := http.Header{"X-Codex-Primary-Used-Percent": {"65"}, "X-Codex-Primary-Window-Minutes": {"300"}}
	svc.UpdateCodexUsageSnapshotFromHeadersAt(context.Background(), account.ID, headers, observedAt, account)
	select {
	case updates := <-repo.updateExtraCh:
		require.Equal(t, observedAt.Format(time.RFC3339Nano), updates["codex_5h_usage_updated_at"])
		require.True(t, openAICodexWindowStaleForPause(updates, "5h", now), "a reused WS handshake cannot refresh an old sample")
	case <-time.After(2 * time.Second):
		t.Fatal("the account threshold should bypass the pre-filled throttle")
	}
}

func TestCodexSnapshotReviewImageDoesNotReviveStaleCanonicalUsage(t *testing.T) {
	now := time.Now().UTC()
	oldTime := now.Add(-3 * time.Hour).Format(time.RFC3339)
	extra := map[string]any{"codex_5h_used_percent": 99.0, "codex_usage_updated_at": oldTime}
	image := buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{
		PrimaryUsedPercent: float64Ptr(1), PrimaryWindowMinutes: intPtr(1440),
	}, now)
	require.NotContains(t, image, "codex_usage_updated_at")
	require.NotContains(t, image, OpenAICodexUsageObservedAtUnixNanoExtraKey)
	maps.Copy(extra, image)
	require.Equal(t, oldTime, extra["codex_usage_updated_at"])
	require.Equal(t, 1.0, extra["codex_primary_used_percent"])
	account := &Account{Platform: PlatformOpenAI, Extra: extra}
	require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 90}, now).ShouldPause)
	_, usable := resolveOpenAIQuotaUtilization(extra, "5h", now)
	require.False(t, usable)
}

func TestCodexSnapshotReviewWeeklyObservationDoesNotReviveFiveHourWindow(t *testing.T) {
	now := time.Now().UTC()
	extra := buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{
		PrimaryUsedPercent: float64Ptr(99), PrimaryWindowMinutes: intPtr(300),
	}, now.Add(-3*time.Hour))
	maps.Copy(extra, buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{
		PrimaryUsedPercent: float64Ptr(9), PrimaryWindowMinutes: intPtr(10080),
	}, now))
	require.True(t, openAICodexWindowStaleForPause(extra, "5h", now))
	require.False(t, openAICodexWindowStaleForPause(extra, "7d", now))
	require.False(t, EvaluateAccountSchedulingThreshold(&Account{Platform: PlatformOpenAI, Extra: extra},
		map[string]int{PlatformOpenAI: 90}, now).ShouldPause)
}

func TestCodexSnapshotReviewUsedOnlyClearsPreviousResetCycle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		window  string
		minutes int
		length  time.Duration
	}{{"5h", 300, 5 * time.Hour}, {"7d", 10080, 7 * 24 * time.Hour}} {
		t.Run(tc.window, func(t *testing.T) {
			prefix := "codex_" + tc.window + "_"
			extra := map[string]any{prefix + "used_percent": 99.0,
				prefix + "reset_at":            now.Add(-time.Hour).Format(time.RFC3339),
				prefix + "reset_after_seconds": 60,
				"codex_usage_updated_at":       now.Add(-2 * time.Hour).Format(time.RFC3339)}
			maps.Copy(extra, buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{
				PrimaryUsedPercent: float64Ptr(99), PrimaryWindowMinutes: intPtr(tc.minutes),
			}, now))
			require.Nil(t, extra[prefix+"reset_at"])
			require.Nil(t, extra[prefix+"reset_after_seconds"])
			decision := EvaluateAccountSchedulingThreshold(&Account{Platform: PlatformOpenAI, Extra: extra},
				map[string]int{PlatformOpenAI: 90}, now)
			require.True(t, decision.ShouldPause, "old elapsed reset metadata must not hide fresh above-threshold usage")
			require.NotNil(t, decision.Until)
			require.WithinDuration(t, now.Add(tc.length), *decision.Until, time.Second)
		})
	}
}

func TestCodexSnapshotReviewRelativeResetUsesItsOwnWindowObservation(t *testing.T) {
	now := time.Now().UTC()
	observed := now.Add(-time.Hour)
	extra := map[string]any{
		"codex_usage_updated_at":       now.Format(time.RFC3339Nano),
		"codex_5h_usage_updated_at":    observed.Format(time.RFC3339Nano),
		"codex_5h_reset_after_seconds": 7200,
	}
	reset, ok := openAICodexWindowResetAt(extra, "5h")
	require.True(t, ok)
	require.True(t, reset.Equal(observed.Add(2*time.Hour)), "weekly observations cannot slide the 5h reset forward")
	extra["codex_5h_usage_updated_at"] = "invalid"
	_, ok = openAICodexWindowResetAt(extra, "5h")
	require.False(t, ok, "an invalid specific timestamp must not fall back to a newer unrelated window")
}
