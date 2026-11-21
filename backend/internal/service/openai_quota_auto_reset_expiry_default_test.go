package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// prepareExpiryOnlyAccount builds an OpenAI OAuth account whose usage switch is off and
// whose expiry lead time falls back to the built-in default (nil) unless overridden.
func prepareExpiryOnlyAccount(t *testing.T, expiresIn time.Duration, leadMinutes any) (*OpenAIQuotaAutoResetService, *autoResetTestAccountRepo, *autoResetTestQuota) {
	t.Helper()
	svc, repo, quota := newResetThresholdTestFixture(t, nil, 1)
	repo.setExtraForTest(OpenAIAutoResetCreditEnabledExtraKey, false)
	repo.setExtraForTest(OpenAIAutoResetCredit5hDisabledExtraKey, true)
	repo.setExtraForTest(OpenAIAutoResetCredit7dDisabledExtraKey, true)
	repo.setExtraForTest(OpenAIAutoResetCreditExpiryEnabledExtraKey, nil)
	repo.setExtraForTest(OpenAIAutoResetCreditExpiryMinutesExtraKey, leadMinutes)
	quota.usage.RateLimit.SecondaryWindow.UsedPercent = 0
	expiry := time.Now().Add(expiresIn).UTC().Format(time.RFC3339)
	quota.usage.RateLimitResetCredits.Credits = []OpenAIRateLimitResetCreditDetail{{ExpiresAt: expiry}}
	quota.usage.autoResetCandidates = []openAIAutoResetCreditCandidate{{ID: "expiry-card", ExpiresAt: expiry}}
	repo.setExtraForTest(openaiQuotaResetCreditsKey, quota.usage.RateLimitResetCredits)
	return svc, repo, quota
}

func TestResolveOpenAIAutoResetExpiryDefaultsToTenMinutes(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	config := ResolveOpenAIAutoResetCreditConfig(account)
	require.False(t, config.Enabled, "the usage thresholds must stay opt-in")
	require.True(t, config.ExpiryEnabled, "the expiry trigger must default to on")
	require.Equal(t, 10, config.ExpiryMinutes)

	account.Extra[OpenAIAutoResetCreditExpiryEnabledExtraKey] = false
	require.False(t, ResolveOpenAIAutoResetCreditConfig(account).ExpiryEnabled)

	account.Extra[OpenAIAutoResetCreditExpiryEnabledExtraKey] = true
	account.Extra[OpenAIAutoResetCreditExpiryMinutesExtraKey] = 45
	config = ResolveOpenAIAutoResetCreditConfig(account)
	require.True(t, config.ExpiryEnabled)
	require.Equal(t, 45, config.ExpiryMinutes)

	account.Extra[OpenAIAutoResetCreditExpiryMinutesExtraKey] = 0
	require.Equal(t, 10, ResolveOpenAIAutoResetCreditConfig(account).ExpiryMinutes)
	require.False(t, ResolveOpenAIAutoResetCreditConfig(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{}}).ExpiryEnabled)
}

func TestNormalizeOpenAIAutoResetExpiryFillsTenMinutes(t *testing.T) {
	normalized, err := normalizeOpenAIAutoResetCreditExtra(PlatformOpenAI, AccountTypeOAuth, false, map[string]any{
		OpenAIAutoResetCreditExpiryEnabledExtraKey: true,
	})
	require.NoError(t, err)
	require.Equal(t, openAIAutoResetExpiryDefaultLeadTime, normalized[OpenAIAutoResetCreditExpiryMinutesExtraKey])
}

func TestOpenAIQuotaAutoResetUsageThresholdsNeedTheUsageSwitch(t *testing.T) {
	svc := &OpenAIQuotaAutoResetService{}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	config := ResolveOpenAIAutoResetCreditConfig(account)
	require.False(t, svc.buildAssessment(account, config, 1, 1).resetReached, "usage thresholds must not fire while the usage switch is off")

	account.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
	config = ResolveOpenAIAutoResetCreditConfig(account)
	require.True(t, svc.buildAssessment(account, config, 1, 1).resetReached)
}

func TestOpenAIQuotaAutoResetExpiryFiresWithUsageSwitchOff(t *testing.T) {
	svc, repo, quota := prepareExpiryOnlyAccount(t, 5*time.Minute, nil)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, int32(1), quota.resetCalls.Load())
	require.Equal(t, "expiry", repo.stateForTest().TriggerWindow)
}

func TestOpenAIQuotaAutoResetExpiryDefaultsToTenMinuteWindow(t *testing.T) {
	svc, _, quota := prepareExpiryOnlyAccount(t, 15*time.Minute, nil)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, int32(0), quota.resetCalls.Load(), "a card 15 minutes out is outside the default 10-minute window")

	svc, _, quota = prepareExpiryOnlyAccount(t, 15*time.Minute, 30)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, int32(1), quota.resetCalls.Load(), "an explicit lead time must still widen the window")
}

func TestOpenAIQuotaAutoResetExpiryHonorsExplicitDisable(t *testing.T) {
	svc, repo, quota := prepareExpiryOnlyAccount(t, 5*time.Minute, nil)
	repo.setExtraForTest(OpenAIAutoResetCreditExpiryEnabledExtraKey, false)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, int32(0), quota.resetCalls.Load())
	require.Nil(t, repo.stateForTest())
}
func TestOpenAIQuotaAutoResetExpiryOnlyScanDoesNotPollWithoutAnExpiringCard(t *testing.T) {
	svc, repo, quota := prepareExpiryOnlyAccount(t, 6*time.Hour, nil)
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Zero(t, quota.resetCalls.Load())
	queries := quota.queryCalls.Load()

	// The usage snapshot ages past its TTL, but no card is about to expire: an
	// account that only opted into the expiry trigger must not poll upstream.
	repo.setExtraForTest("codex_usage_updated_at", time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, svc.evaluateAccount(context.Background(), 501))
	require.Equal(t, queries, quota.queryCalls.Load())
	require.Zero(t, quota.resetCalls.Load())
}
