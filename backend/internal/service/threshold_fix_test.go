//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 回归：上游只回 used-percent、不回任何 reset 头时，全局调度阈值此前会因
// candidate.until == nil 直接判否，导致「设了阈值也不生效」。修复后应以
// codex_usage_updated_at + 窗口长度作为保守上界兜底。
func TestEvaluateAccountSchedulingThreshold_OpenAIPausesWithoutResetHeaders(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-time.Minute)
	account := &Account{
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_usage_updated_at": updatedAt.Format(time.RFC3339),
			"codex_5h_used_percent":  92.0,
			// 故意不写 codex_5h_reset_at / codex_5h_reset_after_seconds
		},
	}

	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 80}, now)

	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window)
	require.Equal(t, 92.0, decision.UsedPercent)
	require.NotNil(t, decision.Until)
	// 上界必须严格晚于「快照采样时间 + 5h」，不能提前放行。
	require.False(t, decision.Until.Before(updatedAt.Add(5*time.Hour)))
	require.True(t, decision.Until.After(now))
}

func TestOpenAIThresholdFallbackUntil_SevenDayWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-2 * time.Hour)

	until := openAIThresholdFallbackUntil(map[string]any{
		"codex_usage_updated_at": updatedAt.Format(time.RFC3339),
	}, "7d", now)

	require.NotNil(t, until)
	require.True(t, until.Equal(updatedAt.Add(7*24*time.Hour)))
}

func TestOpenAIThresholdFallbackUntil_ReturnsNilWhenWindowElapsed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	// 快照采样于 6 小时前 → 5h 窗口上界已过期，不应再据此暂停。
	until := openAIThresholdFallbackUntil(map[string]any{
		"codex_usage_updated_at": now.Add(-6 * time.Hour).Format(time.RFC3339),
	}, "5h", now)

	require.Nil(t, until)
}

func TestOpenAIThresholdFallbackUntil_ReturnsNilWithoutTimestamp(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	require.Nil(t, openAIThresholdFallbackUntil(map[string]any{}, "5h", now))
	require.Nil(t, openAIThresholdFallbackUntil(map[string]any{
		"codex_usage_updated_at": "not-a-timestamp",
	}, "5h", now))
}

// 显式 reset 头存在时必须优先使用真实重置时间，不被兜底逻辑覆盖。
func TestEvaluateAccountSchedulingThreshold_OpenAIPrefersExplicitResetAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(90 * time.Minute)
	account := &Account{
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Add(-time.Minute).Format(time.RFC3339),
			"codex_5h_used_percent":  95.0,
			"codex_5h_reset_at":      resetAt.Format(time.RFC3339),
		},
	}

	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 80}, now)

	require.True(t, decision.ShouldPause)
	require.NotNil(t, decision.Until)
	require.True(t, resetAt.Equal(*decision.Until))
}

func TestCodexSnapshotNearLimit_UsesProvidedBypassPercent(t *testing.T) {
	t.Parallel()

	updates := map[string]any{"codex_5h_used_percent": 85.0}

	// 默认 90% 不触发；把旁路点降到 80% 后触发。
	require.False(t, codexSnapshotNearLimit(updates, 90))
	require.True(t, codexSnapshotNearLimit(updates, 80))
}

func TestCodexSnapshotNearLimit_TolerantOfNumericShapes(t *testing.T) {
	t.Parallel()

	require.True(t, codexSnapshotNearLimit(map[string]any{"codex_7d_used_percent": 95.0}, 90))
	require.True(t, codexSnapshotNearLimit(map[string]any{"codex_7d_used_percent": int(95)}, 90))
	require.True(t, codexSnapshotNearLimit(map[string]any{"codex_7d_used_percent": int64(95)}, 90))
	require.True(t, codexSnapshotNearLimit(map[string]any{"codex_7d_used_percent": "95"}, 90))
	require.False(t, codexSnapshotNearLimit(map[string]any{"codex_7d_used_percent": 42.0}, 90))
	require.False(t, codexSnapshotNearLimit(map[string]any{}, 90))
}

func TestCodexSnapshotNearLimit_FallsBackToDefaultPercent(t *testing.T) {
	t.Parallel()

	updates := map[string]any{"codex_5h_used_percent": 88.0}
	require.False(t, codexSnapshotNearLimit(updates, 0))

	updates["codex_5h_used_percent"] = 91.0
	require.True(t, codexSnapshotNearLimit(updates, 0))
}
