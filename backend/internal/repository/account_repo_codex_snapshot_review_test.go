//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *AccountRepoSuite) TestUpdateExtra_ReviewPartialCodexSnapshotPreservesLegacyWindowAge() {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-3 * time.Hour).Format(time.RFC3339)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "review-partial-codex-observation", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"codex_usage_updated_at": old, "codex_5h_used_percent": 99.0},
	})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_7d_used_percent":                            9.0,
		"codex_usage_updated_at":                           now.Format(time.RFC3339),
		"codex_7d_usage_updated_at":                        now.Format(time.RFC3339Nano),
		service.OpenAICodexUsageObservedAtUnixNanoExtraKey: now.UnixNano(),
	}))
	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(old, got.Extra["codex_5h_usage_updated_at"])
	s.Require().Equal(now.Format(time.RFC3339Nano), got.Extra["codex_7d_usage_updated_at"])
	s.Require().False(service.EvaluateAccountSchedulingThreshold(got, map[string]int{service.PlatformOpenAI: 90}, now).ShouldPause,
		"a weekly-only update must not re-pause a legacy stale 5h sample")
	// Older, out-of-order header snapshots must still be rejected atomically,
	// including the new per-window timestamps and explicit reset clears.
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_5h_used_percent":                            1.0,
		"codex_usage_updated_at":                           now.Add(-time.Minute).Format(time.RFC3339),
		"codex_5h_usage_updated_at":                        now.Add(-time.Minute).Format(time.RFC3339Nano),
		service.OpenAICodexUsageObservedAtUnixNanoExtraKey: now.Add(-time.Minute).UnixNano(),
	}))
	got, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(99.0, got.Extra["codex_5h_used_percent"])
	s.Require().Equal(old, got.Extra["codex_5h_usage_updated_at"])
}

func (s *AccountRepoSuite) TestUpdateExtra_ReviewUsedOnlyNullsResetWithoutTouchingOtherWindow() {
	now := time.Now().UTC().Truncate(time.Second)
	weeklyReset := now.Add(6 * 24 * time.Hour).Format(time.RFC3339)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "review-used-only-reset-cycle", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{
			"codex_usage_updated_at":       now.Add(-3 * time.Hour).Format(time.RFC3339),
			"codex_5h_used_percent":        1.0,
			"codex_5h_reset_at":            now.Add(-time.Hour).Format(time.RFC3339),
			"codex_5h_reset_after_seconds": 60,
			"codex_7d_used_percent":        10.0,
			"codex_7d_reset_at":            weeklyReset,
		},
	})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_5h_used_percent":                            99.0,
		"codex_5h_reset_at":                                nil,
		"codex_5h_reset_after_seconds":                     nil,
		"codex_usage_updated_at":                           now.Format(time.RFC3339),
		"codex_5h_usage_updated_at":                        now.Format(time.RFC3339Nano),
		service.OpenAICodexUsageObservedAtUnixNanoExtraKey: now.UnixNano(),
	}))
	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Contains(got.Extra, "codex_5h_reset_at")
	s.Require().Nil(got.Extra["codex_5h_reset_at"])
	s.Require().Nil(got.Extra["codex_5h_reset_after_seconds"])
	s.Require().Equal(weeklyReset, got.Extra["codex_7d_reset_at"])
	decision := service.EvaluateAccountSchedulingThreshold(got, map[string]int{service.PlatformOpenAI: 90}, now)
	s.Require().True(decision.ShouldPause)
	s.Require().Equal("5h", decision.Window)
	s.Require().NotNil(decision.Until)
	s.Require().WithinDuration(now.Add(5*time.Hour), *decision.Until, time.Second)
}
