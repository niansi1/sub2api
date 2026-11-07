package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerMetadataReviewKeepsIndependentCodexObservationTimes(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-3 * time.Hour).Format(time.RFC3339Nano)
	fresh := now.Format(time.RFC3339Nano)
	filtered := filterSchedulerExtra(map[string]any{
		"codex_usage_updated_at":    fresh,
		"codex_5h_usage_updated_at": old,
		"codex_7d_usage_updated_at": fresh,
		"codex_5h_used_percent":     99.0,
		"codex_7d_used_percent":     9.0,
	})
	require.Equal(t, old, filtered["codex_5h_usage_updated_at"])
	require.Equal(t, fresh, filtered["codex_7d_usage_updated_at"])
	account := &service.Account{Platform: service.PlatformOpenAI, Extra: filtered}
	require.False(t, service.EvaluateAccountSchedulingThreshold(account,
		map[string]int{service.PlatformOpenAI: 90}, now).ShouldPause,
		"candidate selection must see the same stale-window decision as a hydrated account")
}
