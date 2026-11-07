package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexReviewHeadroomUsesEachWindowObservation(t *testing.T) {
	now := time.Now().UTC()
	extra := map[string]any{
		"codex_usage_updated_at":    now.Format(time.RFC3339Nano),
		"codex_7d_usage_updated_at": now.Add(-9 * time.Hour).Format(time.RFC3339Nano),
		"codex_5h_usage_updated_at": now.Format(time.RFC3339Nano),
		"codex_7d_used_percent":     99.0,
		"codex_5h_used_percent":     99.0,
	}
	require.Equal(t, openAIQuotaHeadroomNeutralFactor, openAIQuotaHeadroomFactor(&Account{Extra: extra}, now),
		"a fresh 5h sample must not refresh stale weekly headroom")
	extra["codex_7d_usage_updated_at"] = now.Format(time.RFC3339Nano)
	extra["codex_7d_used_percent"] = 25.0
	extra["codex_5h_usage_updated_at"] = now.Add(-9 * time.Hour).Format(time.RFC3339Nano)
	require.InDelta(t, 0.75, openAIQuotaHeadroomFactor(&Account{Extra: extra}, now), 1e-9,
		"stale 5h utilization must not halve fresh weekly headroom")
}

func TestCodexReviewUsageProgressAnchorsRelativeResetToWindowObservation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observed := now.Add(-30 * time.Minute)
	progress := buildCodexUsageProgressFromExtra(map[string]any{
		"codex_usage_updated_at":       now.Format(time.RFC3339),
		"codex_5h_usage_updated_at":    observed.Format(time.RFC3339Nano),
		"codex_5h_used_percent":        50.0,
		"codex_5h_reset_after_seconds": 3600,
	}, "5h", now)
	require.NotNil(t, progress)
	require.NotNil(t, progress.ResetsAt)
	require.Equal(t, observed.Add(time.Hour), *progress.ResetsAt)
}

func TestCodexReviewDuplicateAccountClearsObservationTimes(t *testing.T) {
	extra, err := duplicateAccountExtra(map[string]any{
		"codex_usage_updated_at":     "2026-11-07T12:00:00Z",
		"codex_raw_usage_updated_at": "2026-11-07T12:00:00Z",
		"codex_5h_usage_updated_at":  "2026-11-07T12:00:00Z",
		"codex_7d_usage_updated_at":  "2026-11-07T12:00:00Z",
		"auto_pause_5h_threshold":    0.98,
	})
	require.NoError(t, err)
	for _, key := range []string{"codex_usage_updated_at", "codex_raw_usage_updated_at", "codex_5h_usage_updated_at", "codex_7d_usage_updated_at"} {
		require.NotContains(t, extra, key)
	}
	require.Equal(t, 0.98, extra["auto_pause_5h_threshold"])
}
