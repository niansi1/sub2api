//go:build unit

package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// codexQuotaHeaders 构造一组带 5h/7d 用量的 Codex 配额响应头（仅相对 reset，
// 刻意不带绝对的 x-codex-*-reset-at，以贴合只回倒计时的上游）。
func codexQuotaHeaders() http.Header {
	headers := http.Header{}
	headers.Set("x-codex-secondary-used-percent", "97")
	headers.Set("x-codex-secondary-reset-after-seconds", "3600")
	headers.Set("x-codex-secondary-window-minutes", "300")
	headers.Set("x-codex-primary-used-percent", "42")
	headers.Set("x-codex-primary-reset-after-seconds", "86400")
	headers.Set("x-codex-primary-window-minutes", "10080")
	return headers
}

func waitForCodexSnapshotWrite(t *testing.T, ch chan map[string]any) map[string]any {
	t.Helper()
	select {
	case updates := <-ch:
		return updates
	case <-time.After(3 * time.Second):
		t.Fatal("等待 codex 快照落库超时")
	}
	return nil
}

// 回归：WS 池化连接每轮重放同一份握手响应头。若这些响应头被按 time.Now() 落库，
// 一条一小时前建立的连接会让陈旧配额永远显示为「刚刚观测」，既绕过 2h 陈旧熔断，
// 也骗过按观测时间排序的单调写入。修复后快照必须锚定握手时刻。
func TestUpdateCodexUsageSnapshotFromHeadersAt_AnchorsToObservedAt(t *testing.T) {
	t.Parallel()

	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &OpenAIGatewayService{accountRepo: repo}

	handshakeAt := time.Now().Add(-59*time.Minute - 30*time.Second)
	svc.UpdateCodexUsageSnapshotFromHeadersAt(t.Context(), 42, codexQuotaHeaders(), handshakeAt)

	updates := waitForCodexSnapshotWrite(t, repo.updateExtraCh)
	raw, ok := updates["codex_usage_updated_at"]
	require.True(t, ok, "快照必须记录观测时间")

	stored, err := time.Parse(time.RFC3339, raw.(string))
	require.NoError(t, err)
	require.WithinDuration(t, handshakeAt, stored, time.Second,
		"观测时间必须是握手时刻，而不是落库时刻")

	// 单调写入哨兵同样必须锚定握手时刻，否则陈旧写会盖掉更新的观测。
	observedNano, ok := updates[OpenAICodexUsageObservedAtUnixNanoExtraKey]
	require.True(t, ok, "单调守卫哨兵必须写入")
	require.Equal(t, handshakeAt.UnixNano(), observedNano)
}

// 未提供观测时刻时必须回落到「当前」，保持 HTTP 路径的既有语义不变。
func TestUpdateCodexUsageSnapshotFromHeadersAt_ZeroObservedAtMeansNow(t *testing.T) {
	t.Parallel()

	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &OpenAIGatewayService{accountRepo: repo}

	before := time.Now().Add(-time.Second)
	svc.UpdateCodexUsageSnapshotFromHeadersAt(t.Context(), 43, codexQuotaHeaders(), time.Time{})

	updates := waitForCodexSnapshotWrite(t, repo.updateExtraCh)
	stored, err := time.Parse(time.RFC3339, updates["codex_usage_updated_at"].(string))
	require.NoError(t, err)
	require.False(t, stored.Before(before), "零值 observedAt 必须回落到当前时间")
	require.True(t, stored.Before(time.Now().Add(2*time.Second)))
}

// codexQuotaHeadersWithoutReset 模拟上游只回 used-percent、完全不回任何 reset 头的形态。
// 这是 2h 陈旧熔断唯一会生效的场景：没有 reset 头就没有「未来重置待发生」，
// 判定完全依赖 codex_usage_updated_at 的可信度 —— 正是 WS 握手重放污染的字段。
func codexQuotaHeadersWithoutReset() http.Header {
	headers := http.Header{}
	headers.Set("x-codex-secondary-used-percent", "97")
	headers.Set("x-codex-secondary-window-minutes", "300")
	headers.Set("x-codex-primary-used-percent", "42")
	headers.Set("x-codex-primary-window-minutes", "10080")
	return headers
}

// 握手时刻很旧时，2h 陈旧熔断必须生效：不再据陈旧配额暂停账号。
// 修复前握手头被盖上 time.Now()，这条路径永远不会触发。
func TestResolveOpenAIQuotaUtilization_HandshakeStaleSnapshotDoesNotPause(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	snapshot := ParseCodexRateLimitHeaders(codexQuotaHeadersWithoutReset())
	require.NotNil(t, snapshot)
	// 握手发生在 3 小时前：快照已越过 openAICodexAutoPauseStaleAfter(2h)。
	snapshot.UpdatedAt = now.Add(-3 * time.Hour).Format(time.RFC3339Nano)

	updates := buildCodexUsageExtraUpdates(snapshot, now)
	require.Nil(t, updates["codex_5h_reset_at"], "无 reset 头时不应写出绝对 reset")

	_, ok := resolveOpenAIQuotaUtilization(updates, "5h", now)
	require.False(t, ok, "3 小时前观测的快照不得暂停账号")
}

// 同一条连接刚建立时（握手在 2h 内）仍然必须据其配额暂停。
// 这条守门测试确保修复不会把真实的高用量信号一起放掉。
func TestResolveOpenAIQuotaUtilization_FreshHandshakeSnapshotStillPauses(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	snapshot := ParseCodexRateLimitHeaders(codexQuotaHeadersWithoutReset())
	require.NotNil(t, snapshot)
	snapshot.UpdatedAt = now.Add(-10 * time.Minute).Format(time.RFC3339Nano)

	updates := buildCodexUsageExtraUpdates(snapshot, now)

	percent, ok := resolveOpenAIQuotaUtilization(updates, "5h", now)
	require.True(t, ok, "刚建立的连接仍必须据其配额暂停")
	require.InDelta(t, 0.97, percent, 1e-9)
}
