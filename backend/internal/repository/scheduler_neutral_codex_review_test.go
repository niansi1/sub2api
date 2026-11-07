package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCodexReviewObservationRemainsSchedulerNeutral(t *testing.T) {
	t.Parallel()
	for name, updates := range map[string]map[string]any{
		"raw only": {"codex_raw_usage_updated_at": "2026-11-07T12:00:00Z"},
		"canonical": {
			"codex_raw_usage_updated_at":                       "2026-11-07T12:00:00Z",
			"codex_usage_updated_at":                           "2026-11-07T12:00:00Z",
			"codex_5h_usage_updated_at":                        "2026-11-07T12:00:00Z",
			"codex_5h_used_percent":                            99.0,
			"codex_5h_reset_at":                                nil,
			service.OpenAICodexUsageObservedAtUnixNanoExtraKey: int64(1794052800000000000),
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, shouldEnqueueSchedulerOutboxForExtraUpdates(updates))
		})
	}
	require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{"auto_pause_5h_disabled": true}),
		"actual scheduling policy changes must still emit outbox events")
}
