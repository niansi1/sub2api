package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 功能精简门控：把「只在前端隐藏入口」升级为「API 层直接拒绝」。
//
// 三个门控都遵循同一套约定：
//   - settingService 为 nil 时放行（保底兼容既有只读路由测试与部分装配场景）；
//   - 开关缺省值按开启处理（opt-out），只有管理员显式关闭才会命中拒绝分支；
//   - 关闭后统一返回 403 + 明确的 reason code，前端据此提示，网关链路不受影响。
//
// 重新打开方式：PUT /api/v1/admin/settings 把对应开关置回 true，无需改代码或重启。

// redeemFeatureGuard 兑换码（卡密）总开关关闭时拒绝访问。
func redeemFeatureGuard(settingService *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settingService == nil || settingService.IsRedeemEnabled(c.Request.Context()) {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrRedeemDisabled)
		c.Abort()
	}
}

// promoCodeFeatureGuard 优惠码开关关闭时拒绝访问。
func promoCodeFeatureGuard(settingService *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settingService == nil || settingService.IsPromoCodeEnabled(c.Request.Context()) {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrPromoCodeFeatureDisabled)
		c.Abort()
	}
}

// affiliateFeatureGuard 邀请返利总开关关闭时拒绝访问。
func affiliateFeatureGuard(settingService *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settingService == nil || settingService.IsAffiliateEnabled(c.Request.Context()) {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrAffiliateDisabled)
		c.Abort()
	}
}

// subscriptionFeatureGuard 用户侧订阅功能开关关闭时拒绝访问。
func subscriptionFeatureGuard(settingService *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settingService == nil || settingService.IsSubscriptionEnabled(c.Request.Context()) {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrSubscriptionDisabled)
		c.Abort()
	}
}
