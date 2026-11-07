/**
 * Type definitions for Vue Router meta fields
 * Extends the RouteMeta interface with custom properties
 */

import 'vue-router'

declare module 'vue-router' {
  interface RouteMeta {
    /**
     * Whether this route requires authentication
     * @default true
     */
    requiresAuth?: boolean

    /**
     * Whether this route requires admin role
     * @default false
     */
    requiresAdmin?: boolean

    /**
     * Page title for this route
     */
    title?: string

    /**
     * Optional breadcrumb items for navigation
     */
    breadcrumbs?: Array<{
      label: string
      to?: string
    }>

    /**
     * Icon name for this route (for sidebar navigation)
     */
    icon?: string

    /**
     * Whether to hide this route from navigation menu
     * @default false
     */
    hideInMenu?: boolean

    /**
     * Whether this route requires internal payment system to be enabled
     * @default false
     */
    requiresPayment?: boolean

    /**
     * 是否要求风控中心功能开关已启用
     * @default false
     */
    requiresRiskControl?: boolean

    /**
     * 是否要求订阅功能开关（subscription_enabled，opt-out）未被显式关闭
     * @default false
     */
    requiresSubscription?: boolean

    /**
     * 是否要求兑换码功能开关（redeem_enabled，opt-out）未被显式关闭
     * @default false
     */
    requiresRedeem?: boolean

    /**
     * 是否要求优惠码功能开关（promo_code_enabled，opt-out）未被显式关闭
     * @default false
     */
    requiresPromoCode?: boolean

    /**
     * 是否要求邀请返利功能开关（affiliate_enabled，opt-in）已被显式开启
     * @default false
     */
    requiresAffiliate?: boolean

    /**
     * 是否要求可用渠道功能开关（available_channels_enabled，opt-in）已被显式开启
     * @default false
     */
    requiresAvailableChannels?: boolean

    /**
     * 是否要求渠道监控功能开关（channel_monitor_enabled，opt-out）未被显式关闭
     * @default false
     */
    requiresChannelMonitor?: boolean

    /**
     * i18n key for the page title
     */
    titleKey?: string

    /**
     * i18n key for the page description
     */
    descriptionKey?: string
  }
}
