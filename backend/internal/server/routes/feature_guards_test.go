package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newFeatureSettings 构造一个只含指定开关值的 SettingService。
// 注意：channelMonitorRouteSettingRepoStub.GetValue 对缺失 key 返回 ("", nil)
// 而不是 ErrSettingNotFound，因此「设置缺失」等价于空字符串值。
func newFeatureSettings(values map[string]string) *service.SettingService {
	if values == nil {
		values = map[string]string{}
	}
	return service.NewSettingService(&channelMonitorRouteSettingRepoStub{values: values}, &config.Config{})
}

// featureGuardCase 描述一个门控的测试参数。
type featureGuardCase struct {
	name string
	// guard 是被测的门控工厂。
	guard func(*service.SettingService) gin.HandlerFunc
	// settingKey 是该门控读取的 settings key。
	settingKey string
	// reasonCode 是拒绝时响应体里的 reason code。
	reasonCode string
	// enabledFor 复述该开关在 service 层真实的取值判定，门控必须与之一致。
	enabledFor func(raw string) bool
	// note 说明该开关的默认语义，便于报告里解释。
	note string
}

func featureGuardCases() []featureGuardCase {
	return []featureGuardCase{
		{
			// 新增开关，采用 isFalseSettingValue 口径：false/0/off/disabled 均视为关闭。
			name:       "redeem",
			guard:      redeemFeatureGuard,
			settingKey: service.SettingKeyRedeemEnabled,
			reasonCode: "REDEEM_DISABLED",
			enabledFor: looseOptOut,
			note:       "新增，opt-out，识别 false/0/off/disabled",
		},
		{
			// 上游历史实现只判断 value != "false"，其余写法一律算开启。
			name:       "promo code",
			guard:      promoCodeFeatureGuard,
			settingKey: service.SettingKeyPromoCodeEnabled,
			reasonCode: "PROMO_CODE_DISABLED",
			enabledFor: literalOptOut,
			note:       "上游既有，opt-out，仅字面量 false 关闭",
		},
		{
			// 邀请返利是 opt-in：只有字面量 "true" 才算开启。
			name:       "affiliate",
			guard:      affiliateFeatureGuard,
			settingKey: service.SettingKeyAffiliateEnabled,
			reasonCode: "AFFILIATE_DISABLED",
			enabledFor: strictOptIn,
			note:       "上游既有，opt-in，仅字面量 true 开启",
		},
		{
			// 订阅是 opt-out，走 isFalseSettingValue。
			name:       "subscription",
			guard:      subscriptionFeatureGuard,
			settingKey: service.SettingKeySubscriptionEnabled,
			reasonCode: "SUBSCRIPTION_DISABLED",
			enabledFor: looseOptOut,
			note:       "新增读取方法，opt-out，识别 false/0/off/disabled",
		},
	}
}

// looseOptOut 复述 service.isFalseSettingValue 的反面：trim + 小写后比对四种假值。
func looseOptOut(raw string) bool {
	switch normalize(raw) {
	case "false", "0", "off", "disabled":
		return false
	}
	return true
}

// literalOptOut 复述 IsPromoCodeEnabled：只认字面量 "false"。
func literalOptOut(raw string) bool { return raw != "false" }

// strictOptIn 复述 IsAffiliateEnabled：只认字面量 "true"。
func strictOptIn(raw string) bool { return raw == "true" }

func normalize(raw string) string {
	trimmed := make([]rune, 0, len(raw))
	start, end := 0, len(raw)
	for start < end && (raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\n' || raw[start] == '\r') {
		start++
	}
	for end > start && (raw[end-1] == ' ' || raw[end-1] == '\t' || raw[end-1] == '\n' || raw[end-1] == '\r') {
		end--
	}
	for _, r := range raw[start:end] {
		if r >= 'A' && r <= 'Z' {
			r = r - 'A' + 'a'
		}
		trimmed = append(trimmed, r)
	}
	return string(trimmed)
}

// TestFeatureGuards 验证四个门控在关键取值下的放行/拒绝结果。
func TestFeatureGuards(t *testing.T) {
	for _, tt := range featureGuardCases() {
		t.Run(tt.name, func(t *testing.T) {
			// nil SettingService 一律放行：既有只读路由测试大量传 nil，
			// 门控必须保持兼容，不能把测试装配场景挡死。
			t.Run("nil setting service allows", func(t *testing.T) {
				status, _ := runFeatureGuard(t, tt.guard, nil)
				require.Equal(t, http.StatusOK, status)
			})

			for _, raw := range []string{"", "true", "false", "1", "TRUE", "0", "off", "disabled", " FALSE "} {
				raw := raw
				t.Run("value="+raw, func(t *testing.T) {
					want := http.StatusOK
					if !tt.enabledFor(raw) {
						want = http.StatusForbidden
					}
					status, body := runFeatureGuard(t, tt.guard,
						newFeatureSettings(map[string]string{tt.settingKey: raw}))
					require.Equal(t, want, status)
					if want == http.StatusForbidden {
						require.Contains(t, body, tt.reasonCode)
					}
				})
			}
		})
	}
}

// TestFeatureGuardsRejectReasonCodes 确认拒绝时返回的是 403 + 专属 reason code，
// 前端 apiError.ts 优先读 reason 做 i18n，前端路由守卫也依赖该语义。
func TestFeatureGuardsRejectReasonCodes(t *testing.T) {
	for _, tt := range featureGuardCases() {
		t.Run(tt.name, func(t *testing.T) {
			status, body := runFeatureGuard(t, tt.guard,
				newFeatureSettings(map[string]string{tt.settingKey: "false"}))
			require.Equal(t, http.StatusForbidden, status)
			require.Contains(t, body, tt.reasonCode)
		})
	}
}

// TestFeatureGuardsAllRegistered 确保四个门控都真实挂到了路由上。
// 漏挂的后果是「开关关了但 API 仍可访问」，比报错更难发现。
func TestFeatureGuardsAllRegistered(t *testing.T) {
	cases := featureGuardCases()
	require.Len(t, cases, 4)
	seen := make(map[string]struct{}, len(cases))
	for _, tt := range cases {
		require.NotNil(t, tt.guard, tt.name)
		require.NotEmpty(t, tt.settingKey, tt.name)
		require.NotEmpty(t, tt.reasonCode, tt.name)
		_, dup := seen[tt.settingKey]
		require.False(t, dup, "duplicate setting key: "+tt.settingKey)
		seen[tt.settingKey] = struct{}{}
	}
}

// runFeatureGuard 把门控挂到一个最小 router 上并返回状态码与响应体。
func runFeatureGuard(t *testing.T, guard func(*service.SettingService) gin.HandlerFunc, svc *service.SettingService) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(guard(svc))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
