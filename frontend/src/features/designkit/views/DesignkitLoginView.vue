<!--
  designkit 登录页。

  ⚠ 这份文件是**上游 `views/auth/LoginView.vue` 的换皮版**：
  版式换成老 designkit 那套（左边商品渠道说明、右边一张表单卡片，
  照 legacy-python 分支的 `frontend/css/views/auth.css`），
  **登录逻辑一行没删**——验证码（Turnstile / 腾讯 / 阿里云）、登录协议弹窗、
  Passkey、各家 OAuth、两步验证（TOTP）、登录后跳转、错误提示，
  全部与上游保持逐条一致。

  为什么不直接改上游那一份：CLAUDE.md 第一节规定上游文件只有几个能碰，
  `views/auth/LoginView.vue` 不在其中。做法是自己写一份，
  然后只在白名单内的 `router/index.ts` 里把 `/login` 指过来。

  跟版提醒：上游 `views/auth/LoginView.vue` 变了登录逻辑时，
  这一份要跟着改（版式不用动）。差异只在 <template> 的排版和 <style>，
  <script setup> 应当与上游保持一致，方便 diff 对照。
-->
<template>
  <main class="dk-auth-page">
    <!-- ── 左：商品图这件事是怎么跑起来的 ─────────────────── -->
    <section class="dk-auth-stage" aria-label="跨平台商品图处理">
      <header class="dk-auth-stage-copy">
        <!-- 换行必须走绑定：模板里直接敲回车会被 Vue 编译器压成一个空格，
             配上 white-space: pre-line 也断不了行。 -->
        <p class="dk-auth-stage-title">{{ stageTitle }}</p>
        <p class="dk-auth-stage-subtitle">
          换背景、配场景、按平台比例出图，主图、详情图、海报一次做完。
        </p>
      </header>

      <div class="dk-auth-component-scene">
        <!--
          15 个电商平台标识，绕成一个缓慢自转的球，衬在下面那四张卡片后面。
          这是老 designkit 登录页就有的东西（monica 2026-08-14：「我记得原来设计的是
          动态的，多个电商平台logo的」），2026-08-13 移植登录页时漏掉了，现在补回来。

          ⚠ 三件事不能省：
          1. **系统开了「减少动态效果」时不转**，退回静态排列。这是无障碍要求，
             对前庭功能敏感的人来说，一个一直转的东西会引起不适。
          2. **库加载失败要有退路**：退回静态排列，而不是留一块空白。
             登录页是运营见到的第一屏，空一块会让人以为系统坏了。
          3. **aria-hidden**：这是装饰，屏幕阅读器念 15 个平台名毫无意义，
             下面那句「适配主流电商平台」才是要念的。
        -->
        <div ref="logoCloudEl" class="dk-auth-logo-cloud" aria-hidden="true"></div>

        <!--
          四张流程卡片 2026-08-14 按 monica 要求删除。
          左边这一屏现在只有标题 + 平台标识球，球独占整个场景区。
        -->
      </div>

      <p class="dk-auth-platform-note">
        适配淘宝、天猫、抖音等主流电商平台。
      </p>
    </section>

    <!-- ── 右：登录表单 ───────────────────────────────────── -->
    <section class="dk-auth-rail">
      <div class="dk-auth-brand">
        <span class="dk-auth-wordmark">DesignKit</span>
      </div>

      <div class="dk-auth-form-body">
        <form class="dk-auth-card" novalidate @submit.prevent="handleLogin">
          <div class="dk-auth-heading">
            <!-- 不用上游的 auth.welcomeBack（「欢迎回来」）：话术口径是产品话，
                 标题就写这是哪儿、来干什么（决策 35）。 -->
            <h1 class="dk-auth-title">登录 DesignKit</h1>
            <p class="dk-auth-subtitle">电商商品图生成平台</p>
          </div>

          <!-- 错误提示。上游只弹 toast，这里额外把它留在页面上：
               toast 几秒就没了，用户回头想再看一眼「到底哪里不对」是看不到的。 -->
          <div class="dk-form-message" aria-live="assertive">
            <div v-if="errorMessage" class="dk-alert dk-alert--error">
              <span>{{ errorMessage }}</span>
            </div>
          </div>

          <!-- 邮箱 -->
          <div class="dk-field">
            <label class="dk-field-label" for="dk-login-email">
              {{ t('auth.emailLabel') }}
            </label>
            <input
              id="dk-login-email"
              v-model="formData.email"
              class="dk-input"
              type="email"
              required
              autofocus
              autocomplete="email"
              :disabled="authActionDisabled"
              :aria-invalid="errors.email ? 'true' : undefined"
              :placeholder="t('auth.emailPlaceholder')"
            />
          </div>

          <!-- 密码 -->
          <div class="dk-field">
            <label class="dk-field-label" for="dk-login-password">
              {{ t('auth.passwordLabel') }}
            </label>
            <div class="dk-password-control">
              <input
                id="dk-login-password"
                v-model="formData.password"
                class="dk-input"
                :type="showPassword ? 'text' : 'password'"
                required
                autocomplete="current-password"
                :disabled="authActionDisabled"
                :aria-invalid="errors.password ? 'true' : undefined"
                :placeholder="t('auth.passwordPlaceholder')"
              />
              <button
                type="button"
                class="dk-password-toggle"
                :disabled="authActionDisabled"
                :aria-label="showPassword ? '隐藏密码' : '显示密码'"
                @click="showPassword = !showPassword"
              >
                <svg
                  v-if="showPassword"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.75"
                  aria-hidden="true"
                >
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    d="M3.98 8.223A10.477 10.477 0 0 0 1.934 12C3.226 16.338 7.244 19.5 12 19.5c.993 0 1.953-.138 2.863-.395M6.228 6.228A10.451 10.451 0 0 1 12 4.5c4.756 0 8.773 3.162 10.065 7.498a10.522 10.522 0 0 1-4.293 5.774M6.228 6.228 3 3m3.228 3.228 3.65 3.65m7.894 7.894L21 21m-3.228-3.228-3.65-3.65m0 0a3 3 0 1 0-4.243-4.243"
                  />
                </svg>
                <svg
                  v-else
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.75"
                  aria-hidden="true"
                >
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    d="M2.036 12.322a1.012 1.012 0 0 1 0-.639C3.423 7.51 7.36 4.5 12 4.5c4.638 0 8.573 3.007 9.963 7.178.07.207.07.431 0 .639C20.577 16.49 16.64 19.5 12 19.5c-4.638 0-8.573-3.007-9.964-7.178Z"
                  />
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    d="M15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z"
                  />
                </svg>
              </button>
            </div>

            <div v-if="passwordResetEnabled && !backendModeEnabled" class="dk-field-aside">
              <RouterLink class="dk-auth-alt__link" to="/forgot-password">
                {{ t('auth.forgotPassword') }}
              </RouterLink>
            </div>
          </div>

          <!-- 人机验证。启用哪一种由后台设置决定，这里原样透传上游的全部参数。 -->
          <div v-if="captchaEnabled" class="dk-auth-captcha">
            <TurnstileWidget
              ref="turnstileRef"
              :turnstile-enabled="turnstileEnabled"
              :turnstile-site-key="turnstileSiteKey"
              :tencent-enabled="tencentCaptchaEnabled"
              :tencent-app-id="tencentCaptchaAppId"
              :tencent-region="tencentCaptchaRegion"
              :aliyun-enabled="aliyunCaptchaEnabled"
              :aliyun-scene-id="aliyunCaptchaSceneId"
              :aliyun-prefix="aliyunCaptchaPrefix"
              :aliyun-region="aliyunCaptchaRegion"
              @verify="onTurnstileVerify"
              @expire="onTurnstileExpire"
              @error="onTurnstileError"
            />
          </div>

          <button
            type="submit"
            class="dk-auth-submit"
            :disabled="authActionDisabled || (turnstileEnabled && !turnstileToken)"
          >
            {{ isLoading ? t('auth.signingIn') : t('auth.signIn') }}
          </button>

          <LoginAgreementPrompt
            v-if="loginAgreementEnabled"
            :accepted="agreementAccepted"
            :documents="loginAgreementDocuments"
            :mode="loginAgreementMode"
            :updated-at="loginAgreementUpdatedAt"
            :visible="showAgreementModal"
            @accept="acceptLoginAgreement"
            @reject="rejectLoginAgreement"
            @open="showAgreementModal = true"
          />

          <!-- Passkey / 第三方登录。后台没开就整块不出现。 -->
          <div v-if="showPasskeyLogin || showOAuthLogin" class="dk-auth-oauth">
            <div class="dk-auth-divider">
              <span>{{ t('auth.oauthOrContinue') }}</span>
            </div>

            <button
              v-if="showPasskeyLogin"
              type="button"
              class="dk-auth-secondary"
              :disabled="authActionDisabled"
              @click="handlePasskeyLogin"
            >
              {{ passkeyLoading ? t('auth.passkeySigningIn') : t('auth.passkeySignIn') }}
            </button>

            <EmailOAuthButtons
              :disabled="authActionDisabled"
              :github-enabled="githubOAuthEnabled"
              :google-enabled="googleOAuthEnabled"
              :show-divider="false"
              @start="handleOAuthStart"
            />

            <LinuxDoOAuthSection
              v-if="linuxdoOAuthEnabled"
              :disabled="authActionDisabled"
              :show-divider="false"
              @start="handleOAuthStart"
            />
            <DingTalkOAuthSection
              v-if="dingtalkOAuthEnabled"
              :disabled="authActionDisabled"
              :show-divider="false"
              @start="handleOAuthStart"
            />
            <WechatOAuthSection
              v-if="wechatOAuthEnabled"
              :disabled="authActionDisabled"
              :show-divider="false"
              @start="handleOAuthStart"
            />
            <OidcOAuthSection
              v-if="oidcOAuthEnabled"
              :disabled="authActionDisabled"
              :provider-name="oidcOAuthProviderName"
              :show-divider="false"
              @start="handleOAuthStart"
            />
          </div>

          <p v-if="!backendModeEnabled" class="dk-auth-alt">
            <span>{{ t('auth.dontHaveAccount') }}</span>
            <RouterLink class="dk-auth-alt__link" to="/register">
              {{ t('auth.signUp') }}
            </RouterLink>
          </p>

          <p class="dk-auth-note">
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.75"
              aria-hidden="true"
            >
              <path
                stroke-linecap="round"
                stroke-linejoin="round"
                d="M9 12.75 11.25 15 15 9.75m-3-7.036A11.959 11.959 0 0 1 3.598 6 11.99 11.99 0 0 0 3 9.749c0 5.592 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.57-.598-3.75h-.152c-3.196 0-6.1-1.248-8.25-3.285Z"
              />
            </svg>
            <span>账号和出图额度由管理员开通，问题直接联系管理员。</span>
          </p>
        </form>
      </div>
    </section>
  </main>

  <!-- 两步验证弹窗 -->
  <TotpLoginModal
    v-if="show2FAModal"
    ref="totpModalRef"
    :temp-token="totpTempToken"
    :user-email-masked="totpUserEmailMasked"
    @verify="handle2FAVerify"
    @cancel="handle2FACancel"
  />
</template>

<script setup lang="ts">
import { computed, ref, reactive, onBeforeUnmount, onMounted, watch } from 'vue'
import { loadTagCloud } from '../vendor/tagcloud'
import type { TagCloudInstance } from '../vendor/tagcloud'
import { RouterLink, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import LinuxDoOAuthSection from '@/components/auth/LinuxDoOAuthSection.vue'
import DingTalkOAuthSection from '@/components/auth/DingTalkOAuthSection.vue'
import OidcOAuthSection from '@/components/auth/OidcOAuthSection.vue'
import WechatOAuthSection from '@/components/auth/WechatOAuthSection.vue'
import EmailOAuthButtons from '@/components/auth/EmailOAuthButtons.vue'
import LoginAgreementPrompt from '@/components/auth/LoginAgreementPrompt.vue'
import TotpLoginModal from '@/components/auth/TotpLoginModal.vue'
import TurnstileWidget from '@/components/CaptchaChallenge.vue'
import { useAuthStore, useAppStore } from '@/stores'
import { DESIGNKIT_WORKBENCH_PATH } from '@/features/designkit/nav'
import {
  buildOAuthLoginStartURL,
  getPublicSettings,
  isTotp2FARequired,
  isWeChatWebOAuthEnabled,
  startOAuthLogin,
  type OAuthLoginStart
} from '@/api/auth'
import type {
  ActionCaptchaRequestProof,
  LoginAgreementDocument,
  TotpLoginResponse
} from '@/types'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { clearAllAffiliateReferralCodes } from '@/utils/oauthAffiliate'
// 设计变量（--dk-*，全站深色单主题）。全局样式，Vite 会自动去重。
import '@/features/designkit/theme/tokens.css'

const { t } = useI18n()
const LOGIN_AGREEMENT_STORAGE_KEY = 'sub2api_login_agreement_consent'

// ==================== Router & Stores ====================

const router = useRouter()
const authStore = useAuthStore()

/**
 * 登录成功后默认去哪（`?redirect=` 优先，这里只是兜底）。
 *
 * 上游原来一律去 `/dashboard`（Sub2API 的用户仪表盘）。对我们不合适：
 * 按决策 10，运营只该看到我们那几个菜单，落在上游那个页面上既没用、
 * 也没有入口回到工作台。
 *
 * 规则跟 router/index.ts 里「已登录访问 /login」的守卫保持一致，
 * 免得同一个人从两条路进来落到两个不同的页面：
 *   管理员 → 管理后台（monica 的主要工作是配分组、配账号）
 *   其他人 → 生成工作台
 */
function defaultLandingPath(): string {
  return authStore.isAdmin ? '/admin/dashboard' : DESIGNKIT_WORKBENCH_PATH
}
const appStore = useAppStore()

// ==================== 左侧说明（纯静态，不请求任何外部资源） ====================
//
// 图标一律内联 SVG：部署环境要求页面不请求任何外部域名（离线可用 + CSP），
// 老 designkit 那边的 lottie 动画和标签云也一并去掉了——它们要额外的
// 第三方 JS，收益只是好看一点，代价是又一堆要跟着维护的 vendor 文件。

/** 左侧大标题。`\n` 在这里是真的换行（配 CSS 的 white-space: pre-line）。 */
const stageTitle = '一张商品原图\n出齐全平台的图'


// ==================== State ====================

const isLoading = ref<boolean>(false)
const passkeyLoading = ref<boolean>(false)
const errorMessage = ref<string>('')
const showPassword = ref<boolean>(false)
const publicSettingsLoaded = ref<boolean>(false)

// Public settings
const turnstileEnabled = ref<boolean>(false)
const turnstileSiteKey = ref<string>('')
const tencentCaptchaEnabled = ref<boolean>(false)
const tencentCaptchaAppId = ref<string>('')
const tencentCaptchaRegion = ref<string>('cn')
const aliyunCaptchaEnabled = ref<boolean>(false)
const aliyunCaptchaSceneId = ref<string>('')
const aliyunCaptchaPrefix = ref<string>('')
const aliyunCaptchaRegion = ref<string>('cn')
const linuxdoOAuthEnabled = ref<boolean>(false)
const dingtalkOAuthEnabled = ref<boolean>(false)
const wechatOAuthEnabled = ref<boolean>(false)
const backendModeEnabled = ref<boolean>(false)
const oidcOAuthEnabled = ref<boolean>(false)
const oidcOAuthProviderName = ref<string>('OIDC')
const githubOAuthEnabled = ref<boolean>(false)
const googleOAuthEnabled = ref<boolean>(false)
const passwordResetEnabled = ref<boolean>(false)
const passkeyEnabled = ref<boolean>(false)
const loginAgreementEnabled = ref<boolean>(false)
const loginAgreementMode = ref<'modal' | 'checkbox' | string>('modal')
const loginAgreementUpdatedAt = ref<string>('')
const loginAgreementRevision = ref<string>('')
const loginAgreementDocuments = ref<LoginAgreementDocument[]>([])
const agreementAccepted = ref<boolean>(false)
const showAgreementModal = ref<boolean>(false)

// Turnstile
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileToken = ref<string>('')
const tencentCaptchaRandstr = ref<string>('')
const aliyunCaptchaReady = computed(
  () =>
    aliyunCaptchaEnabled.value &&
    Boolean(aliyunCaptchaSceneId.value) &&
    Boolean(aliyunCaptchaPrefix.value)
)
// 动作触发式验证码（腾讯/阿里云）：提交、OAuth 启动、passkey 时弹窗验证
const actionCaptchaEnabled = computed(
  () =>
    (tencentCaptchaEnabled.value && Boolean(tencentCaptchaAppId.value)) ||
    aliyunCaptchaReady.value
)
const captchaEnabled = computed(
  () =>
    (turnstileEnabled.value && Boolean(turnstileSiteKey.value)) || actionCaptchaEnabled.value
)

// 2FA state
const show2FAModal = ref<boolean>(false)
const totpTempToken = ref<string>('')
const totpUserEmailMasked = ref<string>('')
const totpModalRef = ref<InstanceType<typeof TotpLoginModal> | null>(null)

const formData = reactive({
  email: '',
  password: ''
})

const errors = reactive({
  email: '',
  password: '',
  turnstile: ''
})

const validationToastMessage = computed(
  () => errors.email || errors.password || errors.turnstile || ''
)

const agreementGateActive = computed(
  () => loginAgreementEnabled.value && !agreementAccepted.value
)

const authActionDisabled = computed(
  () => isLoading.value || passkeyLoading.value || !publicSettingsLoaded.value || agreementGateActive.value
)

const showPasskeyLogin = computed(
  () => passkeyEnabled.value && typeof window.PublicKeyCredential !== 'undefined'
)

const showOAuthLogin = computed(
  () =>
    !backendModeEnabled.value &&
    (linuxdoOAuthEnabled.value ||
      dingtalkOAuthEnabled.value ||
      wechatOAuthEnabled.value ||
      oidcOAuthEnabled.value ||
      githubOAuthEnabled.value ||
      googleOAuthEnabled.value)
)

watch(validationToastMessage, (value, previousValue) => {
  if (value && value !== previousValue) {
    appStore.showError(value)
  }
})

// ==================== Lifecycle ====================

onMounted(async () => {
  // 平台标识球。**不 await**：它跟登录表单没有任何关系，
  // 让它慢慢加载，别把「能不能输密码」压在一个装饰性动画后面。
  void mountLogoCloud()
  window.addEventListener('resize', onCloudResize)

  const expiredFlag = sessionStorage.getItem('auth_expired')
  if (expiredFlag) {
    sessionStorage.removeItem('auth_expired')
    const message = t('auth.reloginRequired')
    errorMessage.value = message
    appStore.showWarning(message)
  }

  try {
    const settings = await getPublicSettings()
    turnstileEnabled.value = settings.turnstile_enabled
    turnstileSiteKey.value = settings.turnstile_site_key || ''
    tencentCaptchaEnabled.value = settings.tencent_captcha_enabled === true
    tencentCaptchaAppId.value = settings.tencent_captcha_app_id || ''
    tencentCaptchaRegion.value = settings.tencent_captcha_region || 'cn'
    aliyunCaptchaEnabled.value = settings.aliyun_captcha_enabled === true
    aliyunCaptchaSceneId.value = settings.aliyun_captcha_scene_id || ''
    aliyunCaptchaPrefix.value = settings.aliyun_captcha_prefix || ''
    aliyunCaptchaRegion.value = settings.aliyun_captcha_region || 'cn'
    linuxdoOAuthEnabled.value = settings.linuxdo_oauth_enabled
    dingtalkOAuthEnabled.value = settings.dingtalk_oauth_enabled ?? false
    wechatOAuthEnabled.value = isWeChatWebOAuthEnabled(settings)
    backendModeEnabled.value = settings.backend_mode_enabled
    oidcOAuthEnabled.value = settings.oidc_oauth_enabled
    oidcOAuthProviderName.value = settings.oidc_oauth_provider_name || 'OIDC'
    githubOAuthEnabled.value = settings.github_oauth_enabled
    googleOAuthEnabled.value = settings.google_oauth_enabled
    backendModeEnabled.value = settings.backend_mode_enabled
    passwordResetEnabled.value = settings.password_reset_enabled
    passkeyEnabled.value = settings.passkey_enabled === true
    applyLoginAgreementSettings(settings)
  } catch (error) {
    console.error('Failed to load public settings:', error)
    loginAgreementEnabled.value = false
    agreementAccepted.value = true
  } finally {
    publicSettingsLoaded.value = true
  }
})

// ==================== Login Agreement ====================

function applyLoginAgreementSettings(settings: {
  login_agreement_enabled?: boolean
  login_agreement_mode?: string
  login_agreement_updated_at?: string
  login_agreement_revision?: string
  login_agreement_documents?: LoginAgreementDocument[]
}): void {
  const documents = Array.isArray(settings.login_agreement_documents)
    ? settings.login_agreement_documents.filter((doc) => doc.title?.trim())
    : []
  loginAgreementDocuments.value = documents
  loginAgreementEnabled.value = settings.login_agreement_enabled === true && documents.length > 0
  loginAgreementMode.value = settings.login_agreement_mode === 'checkbox' ? 'checkbox' : 'modal'
  loginAgreementUpdatedAt.value = settings.login_agreement_updated_at || ''
  loginAgreementRevision.value =
    settings.login_agreement_revision ||
    `${loginAgreementUpdatedAt.value}:${documents.map((doc) => `${doc.id}:${doc.title}`).join('|')}`

  agreementAccepted.value = !loginAgreementEnabled.value || hasAcceptedLoginAgreement(loginAgreementRevision.value)
  showAgreementModal.value =
    loginAgreementEnabled.value && !agreementAccepted.value && loginAgreementMode.value !== 'checkbox'
}

function hasAcceptedLoginAgreement(revision: string): boolean {
  if (!revision) {
    return false
  }
  try {
    const raw = localStorage.getItem(LOGIN_AGREEMENT_STORAGE_KEY)
    if (!raw) {
      return false
    }
    const parsed = JSON.parse(raw) as { revision?: string }
    return parsed.revision === revision
  } catch {
    return false
  }
}

function acceptLoginAgreement(): void {
  if (loginAgreementRevision.value) {
    localStorage.setItem(
      LOGIN_AGREEMENT_STORAGE_KEY,
      JSON.stringify({
        revision: loginAgreementRevision.value,
        accepted_at: new Date().toISOString()
      })
    )
  }
  agreementAccepted.value = true
  showAgreementModal.value = false
}

function rejectLoginAgreement(): void {
  localStorage.removeItem(LOGIN_AGREEMENT_STORAGE_KEY)
  agreementAccepted.value = false
  showAgreementModal.value = false
  appStore.showWarning(t('legal.loginAgreementPrompt.loginRejectedWarning'))
}

// ==================== Turnstile Handlers ====================

function onTurnstileVerify(token: string, randstr = ''): void {
  turnstileToken.value = token
  tencentCaptchaRandstr.value = randstr
  errors.turnstile = ''
}

function onTurnstileExpire(): void {
  turnstileToken.value = ''
  tencentCaptchaRandstr.value = ''
  errors.turnstile = t('auth.turnstileExpired')
}

function onTurnstileError(): void {
  turnstileToken.value = ''
  tencentCaptchaRandstr.value = ''
  errors.turnstile = t('auth.turnstileFailed')
}

function resetCaptchaProof(): void {
  turnstileRef.value?.reset()
  turnstileToken.value = ''
  tencentCaptchaRandstr.value = ''
  errors.turnstile = ''
}

async function acquireActionProof(): Promise<boolean> {
  if (!actionCaptchaEnabled.value) return true

  const proof = await turnstileRef.value?.verifyAction()
  if (!proof) return false

  turnstileToken.value = proof.token
  tencentCaptchaRandstr.value = proof.randstr
  return true
}

// ==================== Validation ====================

function validateForm(): boolean {
  // Reset errors
  errors.email = ''
  errors.password = ''
  errors.turnstile = ''

  let isValid = true

  if (agreementGateActive.value) {
    appStore.showWarning(t('legal.loginAgreementPrompt.loginRequiredWarning'))
    if (loginAgreementMode.value !== 'checkbox') {
      showAgreementModal.value = true
    }
    return false
  }

  // Email validation
  if (!formData.email.trim()) {
    errors.email = t('auth.emailRequired')
    isValid = false
  } else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(formData.email)) {
    errors.email = t('auth.invalidEmail')
    isValid = false
  }

  // Password validation
  if (!formData.password) {
    errors.password = t('auth.passwordRequired')
    isValid = false
  } else if (formData.password.length < 6) {
    errors.password = t('auth.passwordMinLength')
    isValid = false
  }

  // Turnstile validation
  if (turnstileEnabled.value && !turnstileToken.value) {
    errors.turnstile = t('auth.completeVerification')
    isValid = false
  }

  return isValid
}

// ==================== Form Handlers ====================

async function handleLogin(): Promise<void> {
  // Clear previous error
  errorMessage.value = ''

  // Validate form
  if (!validateForm()) {
    return
  }

  if (!(await acquireActionProof())) {
    return
  }

  isLoading.value = true

  try {
    // Call auth store login（阿里云 captchaVerifyParam 复用 turnstile_token 字段）
    const response = await authStore.login({
      email: formData.email,
      password: formData.password,
      turnstile_token:
        turnstileEnabled.value || aliyunCaptchaEnabled.value ? turnstileToken.value : undefined,
      tencent_captcha_ticket: tencentCaptchaEnabled.value ? turnstileToken.value : undefined,
      tencent_captcha_randstr: tencentCaptchaEnabled.value
        ? tencentCaptchaRandstr.value
        : undefined
    })

    // Check if 2FA is required
    if (isTotp2FARequired(response)) {
      const totpResponse = response as TotpLoginResponse
      totpTempToken.value = totpResponse.temp_token || ''
      totpUserEmailMasked.value = totpResponse.user_email_masked || ''
      show2FAModal.value = true
      isLoading.value = false
      return
    }

    // Show success toast
    clearAllAffiliateReferralCodes()
    appStore.showSuccess(t('auth.loginSuccess'))

    // Redirect to dashboard or intended route
    const redirectTo = (router.currentRoute.value.query.redirect as string) || defaultLandingPath()
    await router.push(redirectTo)
  } catch (error: unknown) {
    errorMessage.value = extractI18nErrorMessage(error, t, 'auth.errors', t('auth.loginFailed'))

    // Also show error toast
    appStore.showError(errorMessage.value)
  } finally {
    if (captchaEnabled.value) {
      resetCaptchaProof()
    }
    isLoading.value = false
  }
}

async function handlePasskeyLogin(): Promise<void> {
  if (agreementGateActive.value) {
    appStore.showWarning(t('legal.loginAgreementPrompt.loginRequiredWarning'))
    if (loginAgreementMode.value !== 'checkbox') {
      showAgreementModal.value = true
    }
    return
  }

  passkeyLoading.value = true
  try {
    let proof: ActionCaptchaRequestProof | undefined
    if (actionCaptchaEnabled.value) {
      const result = await turnstileRef.value?.verifyAction()
      if (!result) return
      proof = tencentCaptchaEnabled.value
        ? {
            tencent_captcha_ticket: result.token,
            tencent_captcha_randstr: result.randstr
          }
        : { turnstile_token: result.token }
    }

    await authStore.loginWithPasskey(proof)
    clearAllAffiliateReferralCodes()
    appStore.showSuccess(t('auth.loginSuccess'))
    const redirectTo = (router.currentRoute.value.query.redirect as string) || defaultLandingPath()
    await router.push(redirectTo)
  } catch (error: unknown) {
    const fallback = error instanceof DOMException && error.name === 'NotAllowedError'
      ? t('auth.passkeyCancelled')
      : t('auth.passkeyFailed')
    errorMessage.value = extractI18nErrorMessage(error, t, 'auth.errors', fallback)
    appStore.showError(errorMessage.value)
  } finally {
    if (actionCaptchaEnabled.value) {
      resetCaptchaProof()
    }
    passkeyLoading.value = false
  }
}

async function handleOAuthStart(request: OAuthLoginStart): Promise<void> {
  if (authActionDisabled.value) return

  if (!actionCaptchaEnabled.value) {
    window.location.href = buildOAuthLoginStartURL(request)
    return
  }

  isLoading.value = true
  try {
    const proof = await turnstileRef.value?.verifyAction()
    if (!proof) return

    const result = await startOAuthLogin(
      request,
      tencentCaptchaEnabled.value
        ? {
            tencent_captcha_ticket: proof.token,
            tencent_captcha_randstr: proof.randstr
          }
        : { turnstile_token: proof.token }
    )
    window.location.href = result.authorize_url
  } catch (error: unknown) {
    errorMessage.value = extractI18nErrorMessage(
      error,
      t,
      'auth.errors',
      t('auth.turnstileFailed')
    )
    appStore.showError(errorMessage.value)
  } finally {
    resetCaptchaProof()
    isLoading.value = false
  }
}

// ==================== 2FA Handlers ====================

async function handle2FAVerify(code: string): Promise<void> {
  if (totpModalRef.value) {
    totpModalRef.value.setVerifying(true)
  }

  try {
    await authStore.login2FA(totpTempToken.value, code)

    // Close modal and show success
    show2FAModal.value = false
    clearAllAffiliateReferralCodes()
    appStore.showSuccess(t('auth.loginSuccess'))

    // Redirect to dashboard or intended route
    const redirectTo = (router.currentRoute.value.query.redirect as string) || defaultLandingPath()
    await router.push(redirectTo)
  } catch (error: unknown) {
    const err = error as { message?: string; response?: { data?: { message?: string } } }
    const message = err.response?.data?.message || err.message || t('profile.totp.loginFailed')

    if (totpModalRef.value) {
      totpModalRef.value.setError(message)
      totpModalRef.value.setVerifying(false)
    }
  }
}

function handle2FACancel(): void {
  show2FAModal.value = false
  totpTempToken.value = ''
  totpUserEmailMasked.value = ''
}

// ---------------------------------------------------------------------------
// 平台标识旋转球
// ---------------------------------------------------------------------------

/**
 * 15 个电商平台。原样搬自老 designkit（frontend/js/app.js 的 COMMERCE_CHANNELS）。
 *
 * ⚠ 京东 / 拼多多 / Amazon / Walmart 这四家**刻意用文字**，不用图形标：
 * Simple Icons（CC0）按商标权利人要求下架了这几条，热链维基百科的图既是外部请求、
 * 收录许可也不都是自由许可（京东那张在英文维基是「合理使用」，本就不该随仓库分发）。
 * 文字标既不外链也不分发他人商标图形，展示上照样认得出是哪家。
 */
interface CommerceChannel {
  key: string
  label: string
  /** 有 src 用图，没有就用 text 文字标。 */
  src?: string
  text?: string
  /** 横向的标（文字或宽图），给更大的最小宽度，免得挤成一团。 */
  wide?: boolean
}

const PLATFORM_ASSET_BASE = '/designkit/platforms'

const COMMERCE_CHANNELS: CommerceChannel[] = [
  { key: 'taobao', label: '淘宝', src: `${PLATFORM_ASSET_BASE}/taobao.svg` },
  { key: 'tmall', label: '天猫', src: `${PLATFORM_ASSET_BASE}/tmall.svg`, wide: true },
  { key: 'jd', label: '京东', text: 'JD.com', wide: true },
  { key: 'pinduoduo', label: '拼多多', text: '拼多多', wide: true },
  { key: 'douyin', label: '抖音电商', src: `${PLATFORM_ASSET_BASE}/tiktok.svg` },
  { key: 'xiaohongshu', label: '小红书', src: `${PLATFORM_ASSET_BASE}/xiaohongshu.svg` },
  { key: 'amazon', label: 'Amazon', text: 'Amazon', wide: true },
  { key: 'ebay', label: 'eBay', src: `${PLATFORM_ASSET_BASE}/ebay.svg`, wide: true },
  { key: 'shopify', label: 'Shopify', src: `${PLATFORM_ASSET_BASE}/shopify.svg` },
  { key: 'etsy', label: 'Etsy', src: `${PLATFORM_ASSET_BASE}/etsy.svg` },
  { key: 'aliexpress', label: 'AliExpress', src: `${PLATFORM_ASSET_BASE}/aliexpress.svg`, wide: true },
  { key: 'shopee', label: 'Shopee', src: `${PLATFORM_ASSET_BASE}/shopee.svg` },
  { key: 'rakuten', label: 'Rakuten', src: `${PLATFORM_ASSET_BASE}/rakuten.svg`, wide: true },
  { key: 'walmart', label: 'Walmart', text: 'Walmart', wide: true },
  { key: 'tiktok-shop', label: 'TikTok Shop', src: `${PLATFORM_ASSET_BASE}/tiktok.svg` },
]

const logoCloudEl = ref<HTMLElement | null>(null)
let tagCloudInstance: TagCloudInstance | null = null
let cloudResizeTimer: ReturnType<typeof setTimeout> | null = null

/** 一个平台标的 HTML。TagCloud 吃的是字符串数组，所以这里拼好再交给它。 */
function channelToken(channel: CommerceChannel): string {
  const wide = channel.wide ? ' dk-auth-logo-token--wide' : ''
  const body = channel.src
    ? `<img class="dk-auth-logo-token__image${channel.wide ? ' dk-auth-logo-token__image--wide' : ''}" src="${channel.src}" alt="" loading="lazy" />`
    : `<span class="dk-auth-logo-token__text">${channel.text ?? channel.label}</span>`
  return `<span class="dk-auth-logo-token${wide}">${body}</span>`
}

/**
 * 球半径。跟着容器宽度走，并卡死上下限。
 *
 * 2026-08-14 调大：原来的值（214~246）是**给中间那四张流程卡片让位**算出来的，
 * 卡片按 monica 要求删掉之后，球独占整个场景区，还用旧值中间会空一大块。
 * 现在按容器宽度的 0.40 走，上下限相应放大。
 *
 * 上下限不能去掉：太小挤成一团认不出是哪家，太大会转出可视区、
 * 一半的标常年看不见。
 */
function cloudRadius(): number {
  const width = logoCloudEl.value?.clientWidth || window.innerWidth
  if (window.matchMedia('(max-width: 639px)').matches) {
    return Math.max(130, Math.min(160, Math.round(width * 0.40)))
  }
  return Math.max(260, Math.min(330, Math.round(width * 0.40)))
}

/** 库加载不出来 / 不该动的时候：静态排一排，**不要留空白**。 */
function renderStaticTokens(): void {
  if (!logoCloudEl.value) {
    return
  }
  logoCloudEl.value.classList.add('is-static')
  logoCloudEl.value.innerHTML = COMMERCE_CHANNELS.map(channelToken).join('')
}

async function mountLogoCloud(): Promise<void> {
  const host = logoCloudEl.value
  if (!host) {
    return
  }
  // 无障碍：系统开了「减少动态效果」就不转。一直转的东西对前庭功能敏感的人
  // 会引起不适，这不是可选项。
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    renderStaticTokens()
    return
  }
  // 库加载失败返回 null（不抛），见 vendor/tagcloud/index.ts。
  const TagCloud = await loadTagCloud()
  if (!TagCloud) {
    // 退回静态排列。登录页是运营见到的第一屏，空一块会让人以为系统坏了。
    renderStaticTokens()
    return
  }
  try {
    host.innerHTML = ''
    host.classList.remove('is-static')
    tagCloudInstance = TagCloud(host, COMMERCE_CHANNELS.map(channelToken), {
      radius: cloudRadius(),
      // 慢一点。这是背景衬托，不是主角；转快了会把视线从登录表单上拽走。
      maxSpeed: 'slow',
      initSpeed: 'slow',
      direction: 135,
      keep: true,
      // ⚠ 下面两个**必须是 true**，2026-08-14 各踩了一次：
      //
      // useHTML: 库默认用 innerText 塞内容。我们给的是拼好的
      //   `<span class="dk-auth-logo-token">…</span>`，默认值下会被**当成纯文本
      //   原样显示在页面上** —— 运营看到的是一大串 <span class=... 的源码。
      //
      // useItemInlineStyles: 定位（transform / position）是库写成行内样式的。
      //   给 false 等于告诉它「样式我自己管」，而我们并没有它那套 CSS，
      //   结果是 15 个标一个挨一个平铺在正文里，完全不转。
      //
      // useContainerInlineStyles 保持 false 是**对的**：容器的 position/尺寸
      //   由我们自己的 .dk-auth-logo-cloud 管（absolute + inset:0），
      //   让库去写会把它变成 relative，就压不到卡片后面了。
      useHTML: true,
      useItemInlineStyles: true,
      useContainerInlineStyles: false,
    })
  } catch {
    renderStaticTokens()
  }
}

function destroyLogoCloud(): void {
  tagCloudInstance?.pause?.()
  tagCloudInstance?.destroy?.()
  tagCloudInstance = null
}

/** 窗口大小变了要按新宽度重算半径。防抖，不然拖动窗口时会疯狂重建。 */
function onCloudResize(): void {
  if (cloudResizeTimer) {
    clearTimeout(cloudResizeTimer)
  }
  cloudResizeTimer = setTimeout(() => {
    destroyLogoCloud()
    void mountLogoCloud()
  }, 250)
}


onBeforeUnmount(() => {
  // 收拾平台标识球：不移除监听的话，登录成功跳走之后那个 resize 回调还挂着，
  // 里面引用的 DOM 已经没了。
  window.removeEventListener('resize', onCloudResize)
  if (cloudResizeTimer) {
    clearTimeout(cloudResizeTimer)
  }
  destroyLogoCloud()
})
</script>

<style scoped>
/* ── 整页：左边说明，右边表单 ─────────────────────────────── */
/* color-scheme 原在 auth-light.css（已随浅色主题删除）。深色单主题下浏览器
   自绘件（滚动条/表单控件/自动填充底色）也要走深色，否则深底卡片上冒白件。 */
.dk-auth-page {
  color-scheme: dark;

  position: relative;
  display: grid;
  width: 100%;
  height: 100dvh;
  min-width: 0;
  min-height: 100dvh;
  grid-template-columns: minmax(0, 1fr) clamp(400px, 31vw, 456px);
  overflow: hidden;
  background: var(--dk-canvas);
  color: var(--dk-text);
  font-size: var(--dk-text-md);
  line-height: var(--dk-leading);
}

/* ── 左侧舞台 ─────────────────────────────────────────────── */
.dk-auth-stage {
  position: relative;
  isolation: isolate;
  display: grid;
  min-width: 0;
  min-height: 0;
  grid-template-rows: auto minmax(0, 1fr) auto;
  overflow: hidden;
  background:
    radial-gradient(circle at 52% 56%, var(--dk-glow), transparent 34%),
    linear-gradient(#101625, #0b0f1a);
  padding: clamp(36px, 4.5vw, 68px);
  color: var(--dk-text);
}

.dk-auth-stage::before {
  position: absolute;
  inset: 0;
  z-index: -1;
  background-image:
    linear-gradient(rgba(255, 255, 255, 0.035) 1px, transparent 1px),
    linear-gradient(90deg, rgba(255, 255, 255, 0.035) 1px, transparent 1px);
  background-position: center;
  background-size: 42px 42px;
  mask-image: radial-gradient(circle at center, #000 0 34%, transparent 72%);
  content: "";
  pointer-events: none;
}

.dk-auth-stage-copy {
  position: relative;
  z-index: 5;
  width: min(100%, 660px);
}

.dk-auth-stage-title {
  max-width: 14ch;
  margin: 0;
  color: var(--dk-text);
  font-size: clamp(44px, 4.4vw, 64px);
  font-weight: 700;
  letter-spacing: -0.048em;
  line-height: 1.06;
  text-wrap: balance;
  white-space: pre-line;
}

.dk-auth-stage-subtitle {
  max-width: 54ch;
  margin: 14px 0 0;
  color: var(--dk-text-secondary);
  font-size: clamp(14px, 1.05vw, 15px);
  line-height: 1.7;
}

/* ── 平台标识旋转球 ─────────────────────────────────────────
   15 个电商平台绕成一个缓慢自转的球，**衬在那四张卡片后面**（z-index 1 vs 3）。
   样式搬自老 designkit 的 frontend/css/views/auth.css。 */
.dk-auth-logo-cloud {
  position: absolute;
  inset: 0;
  z-index: 1;
  width: 100%;
  height: 100%;
  /* 球会转出容器边界，不能裁掉。 */
  overflow: visible;
}

/* 转不起来时（库没加载出来 / 系统开了「减少动态效果」）的退路：
   排成一行行居中的标，而不是留一块空白。 */
.dk-auth-logo-cloud.is-static {
  display: flex;
  flex-wrap: wrap;
  align-content: center;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 0 6%;
  /* 静态时压暗一点：它是背景，不该跟前面的卡片抢注意力。 */
  opacity: 0.55;
}

/*
  ⚠ 下面这些**必须走 :deep()**。
  这是 <style scoped>，Vue 靠给模板里的元素加一个属性标记来限定作用域；
  而这些标是 JS 用 innerHTML / 由 TagCloud 塞进去的，**拿不到那个标记**，
  普通选择器一条都不会生效。2026-08-14 踩过：现象是标没有白底卡片、
  图片塌成 0 宽高（看起来"只有文字在转"）。
*/
.dk-auth-logo-cloud :deep(.tagcloud--item) {
  cursor: default;
  outline: none;
}

.dk-auth-logo-cloud :deep(.dk-auth-logo-token) {
  display: flex;
  min-width: 54px;
  min-height: 50px;
  align-items: center;
  justify-content: center;
  padding: 9px 11px;
  border: 1px solid var(--dk-border-strong);
  border-radius: 13px;
  background: var(--dk-surface-raised);
  box-shadow: var(--dk-shadow-2);
  color: var(--dk-text-secondary);
  white-space: nowrap;
}

/* 横向的标（文字标、宽图标）要更宽，否则挤成一团认不出。 */
.dk-auth-logo-cloud :deep(.dk-auth-logo-token--wide) {
  min-width: 92px;
}

.dk-auth-logo-cloud :deep(.dk-auth-logo-token__text) {
  font-size: 14px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

/* ⚠ 尺寸必须写死：SVG 自带 viewBox 但没有固定宽高，不给就会塌成 0。 */
.dk-auth-logo-cloud :deep(.dk-auth-logo-token__image) {
  display: block;
  width: 30px;
  height: 30px;
  flex: 0 0 30px;
  object-fit: contain;
}

.dk-auth-logo-cloud :deep(.dk-auth-logo-token__image--wide) {
  width: 66px;
  height: 26px;
  flex-basis: auto;
}

/* 系统层面要求减少动态效果时，连 CSS 动画一起停掉。
   JS 那边已经不启动球了，这一条是兜底（比如库自己带的过渡）。 */
@media (prefers-reduced-motion: reduce) {
  .dk-auth-logo-cloud,
  .dk-auth-logo-cloud * {
    animation: none !important;
    transition: none !important;
  }
}

.dk-auth-component-scene {
  position: relative;
  z-index: 2;
  display: grid;
  width: min(100%, 860px);
  min-width: 0;
  min-height: clamp(410px, 54vh, 560px);
  align-self: center;
  justify-self: center;
  place-items: center;
}








.dk-auth-platform-note {
  position: relative;
  z-index: 5;
  align-self: end;
  margin: 0;
  color: var(--dk-text-tertiary);
  font-size: 11px;
  line-height: 1.5;
}

/* ── 右侧表单栏 ───────────────────────────────────────────── */
.dk-auth-rail {
  position: relative;
  z-index: 2;
  display: grid;
  min-width: 0;
  min-height: 100dvh;
  grid-template-rows: auto minmax(0, 1fr);
  overflow-y: auto;
  border-left: 1px solid var(--dk-border);
  background: var(--dk-surface);
  box-shadow: -20px 0 56px rgba(28, 29, 31, 0.08);
  padding: clamp(32px, 3.2vw, 48px);
}

.dk-auth-brand {
  display: inline-flex;
  align-items: center;
  align-self: start;
  justify-self: start;
  color: var(--dk-text);
}

.dk-auth-wordmark {
  font-size: 17px;
  font-weight: 760;
  letter-spacing: -0.025em;
  line-height: 1;
}

/*
 * 「有空间就垂直居中，没空间就从顶部开始」。
 * 不能直接写 align-self: center——开了验证码 + 一堆第三方登录按钮时，
 * 表单会比这一格高；居中的元素超出格子是**上下同时溢出**的，
 * 上半截会盖住左上角的 DesignKit 字标，而且在滚动条起点之上，怎么滚都露不出来。
 * align-self: start + margin-block: auto 是标准解法：还有余量时 auto 把它推到
 * 中间，不够时 auto 归零、老实从顶部排、正常滚动。
 */
.dk-auth-form-body {
  display: grid;
  width: min(100%, 352px);
  min-width: 0;
  align-self: start;
  justify-self: center;
  margin-block: auto;
  padding-block: var(--dk-space-8);
}

.dk-auth-card {
  display: flex;
  width: 100%;
  min-width: 0;
  flex-direction: column;
}

.dk-auth-heading {
  margin-bottom: var(--dk-space-8);
}

.dk-auth-title {
  color: var(--dk-text);
  font-size: clamp(28px, 2.3vw, 34px);
  font-weight: 700;
  letter-spacing: -0.04em;
  line-height: 1.22;
}

.dk-auth-subtitle {
  max-width: 32ch;
  margin-top: var(--dk-space-2);
  color: var(--dk-text-secondary);
  font-size: var(--dk-text-md);
  line-height: 1.65;
}

.dk-form-message:empty {
  display: none;
}

.dk-alert {
  display: flex;
  min-width: 0;
  align-items: flex-start;
  gap: var(--dk-space-2);
  margin-bottom: var(--dk-space-5);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-md);
  background: var(--dk-info-soft);
  color: var(--dk-text-secondary);
  padding: 11px 12px;
  font-size: var(--dk-text-sm);
  line-height: 1.5;
}

.dk-alert--error {
  border-color: rgba(248, 113, 113, 0.5);
  background: var(--dk-danger-soft);
  color: var(--dk-danger-strong);
}

/* ── 表单控件 ─────────────────────────────────────────────── */
.dk-field {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: var(--dk-space-2);
  margin: 0 0 var(--dk-space-5);
}

.dk-field-label {
  display: flex;
  align-items: baseline;
  gap: var(--dk-space-1);
  color: var(--dk-text);
  font-size: var(--dk-text-sm);
  font-weight: 650;
  line-height: 1.4;
}

.dk-field-aside {
  display: flex;
  justify-content: flex-end;
}

.dk-input {
  width: 100%;
  min-width: 0;
  min-height: 50px;
  border: 1px solid var(--dk-border-strong);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface-subtle);
  color: var(--dk-text);
  padding: 10px 13px;
  font-size: 15px;
  line-height: 1.45;
  transition:
    border-color var(--dk-motion-fast) var(--dk-ease-out),
    box-shadow var(--dk-motion-fast) var(--dk-ease-out),
    background-color var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-input:hover:not(:disabled) {
  border-color: var(--dk-border-strong);
  background: var(--dk-surface);
}

.dk-input:focus {
  border-color: var(--dk-accent);
  background: var(--dk-surface);
  box-shadow: 0 0 0 3px var(--dk-focus);
  outline: none;
}

.dk-input::placeholder {
  color: var(--dk-text-tertiary);
  opacity: 1;
}

.dk-input:disabled {
  border-color: var(--dk-border);
  background: var(--dk-surface-subtle);
  color: var(--dk-text-disabled);
  opacity: 1;
}

.dk-input[aria-invalid="true"] {
  border-color: var(--dk-danger);
}

/* 浏览器自动填充的黄底在这套白灰配色里非常刺眼，压回我们的底色。 */
.dk-input:-webkit-autofill,
.dk-input:-webkit-autofill:hover,
.dk-input:-webkit-autofill:focus {
  -webkit-text-fill-color: var(--dk-text);
  box-shadow: 0 0 0 1000px var(--dk-surface-subtle) inset;
  caret-color: var(--dk-text);
}

.dk-password-control {
  position: relative;
  min-width: 0;
}

.dk-password-control .dk-input {
  padding-right: 52px;
}

.dk-password-toggle {
  position: absolute;
  top: 5px;
  right: 5px;
  display: inline-flex;
  width: 40px;
  height: 40px;
  align-items: center;
  justify-content: center;
  border: 0;
  border-radius: var(--dk-radius-md);
  background: transparent;
  color: var(--dk-text-tertiary);
  padding: 0;
  cursor: pointer;
  transition:
    background-color var(--dk-motion-fast) var(--dk-ease-out),
    color var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-password-toggle:hover:not(:disabled) {
  background: var(--dk-surface-hover);
  color: var(--dk-text);
}

.dk-password-toggle:focus-visible {
  outline: 2px solid var(--dk-accent);
  outline-offset: 2px;
}

.dk-password-toggle svg {
  width: 18px;
  height: 18px;
}

.dk-auth-captcha {
  margin-bottom: var(--dk-space-5);
}

/* ── 按钮 ─────────────────────────────────────────────────── */
.dk-auth-submit {
  display: inline-flex;
  width: 100%;
  min-height: 50px;
  align-items: center;
  justify-content: center;
  gap: var(--dk-space-2);
  margin-top: var(--dk-space-1);
  border: 1px solid var(--dk-action);
  border-radius: var(--dk-radius-md);
  background: var(--dk-action);
  color: var(--dk-text-inverse);
  padding: 10px 18px;
  font-size: 15px;
  font-weight: 680;
  line-height: 1.2;
  white-space: nowrap;
  cursor: pointer;
  transition:
    background-color var(--dk-motion-fast) var(--dk-ease-out),
    border-color var(--dk-motion-fast) var(--dk-ease-out),
    transform var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-auth-submit:hover:not(:disabled) {
  border-color: var(--dk-action-hover);
  background: var(--dk-action-hover);
  transform: translateY(-1px);
}

.dk-auth-submit:active:not(:disabled) {
  background: var(--dk-action-pressed);
  transform: translateY(1px) scale(0.99);
}

.dk-auth-submit:focus-visible {
  outline: 2px solid var(--dk-accent);
  outline-offset: 2px;
}

.dk-auth-submit:disabled {
  border-color: var(--dk-border-strong);
  background: var(--dk-text-tertiary);
  color: var(--dk-text-inverse);
  opacity: 1;
  cursor: not-allowed;
}

.dk-auth-secondary {
  display: inline-flex;
  width: 100%;
  min-height: var(--dk-primary-h);
  align-items: center;
  justify-content: center;
  gap: var(--dk-space-2);
  border: 1px solid var(--dk-border-strong);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface);
  color: var(--dk-text);
  padding: 10px 18px;
  font-size: var(--dk-text-md);
  font-weight: 650;
  cursor: pointer;
  transition:
    background-color var(--dk-motion-fast) var(--dk-ease-out),
    border-color var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-auth-secondary:hover:not(:disabled) {
  border-color: var(--dk-border-strong);
  background: var(--dk-surface-subtle);
}

.dk-auth-secondary:disabled {
  color: var(--dk-text-disabled);
  cursor: not-allowed;
}

/* ── 第三方登录 ───────────────────────────────────────────── */
.dk-auth-oauth {
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-3);
  margin-top: var(--dk-space-5);
}

.dk-auth-divider {
  display: flex;
  align-items: center;
  gap: var(--dk-space-3);
  color: var(--dk-text-tertiary);
  font-size: var(--dk-text-xs);
}

.dk-auth-divider::before,
.dk-auth-divider::after {
  height: 1px;
  flex: 1;
  background: var(--dk-border);
  content: "";
}

/* ── 底部两条说明 ─────────────────────────────────────────── */
.dk-auth-alt {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: center;
  gap: var(--dk-space-2);
  margin-top: var(--dk-space-4);
  color: var(--dk-text-secondary);
  font-size: var(--dk-text-sm);
}

.dk-auth-alt__link {
  display: inline-flex;
  align-items: center;
  gap: var(--dk-space-1);
  border-radius: var(--dk-radius-sm);
  color: var(--dk-accent-strong);
  font-size: var(--dk-text-sm);
  font-weight: 650;
  text-decoration: none;
}

.dk-auth-alt__link:hover {
  text-decoration: underline;
}

.dk-auth-alt__link:focus-visible {
  outline: 2px solid var(--dk-accent);
  outline-offset: 2px;
}

.dk-auth-note {
  display: flex;
  align-items: flex-start;
  gap: var(--dk-space-2);
  margin-top: var(--dk-space-6);
  border-top: 1px solid var(--dk-border);
  color: var(--dk-text-secondary);
  padding-top: var(--dk-space-5);
  font-size: var(--dk-text-xs);
  line-height: 1.55;
}

.dk-auth-note svg {
  width: 15px;
  height: 15px;
  flex: 0 0 15px;
  margin-top: 1px;
  color: var(--dk-accent-strong);
}

/* ── 响应式 ───────────────────────────────────────────────── */
@media (max-width: 1100px) and (min-width: 900px) {
  .dk-auth-page {
    grid-template-columns: minmax(0, 1fr) 400px;
  }

  .dk-auth-stage {
    padding: 36px;
  }

  .dk-auth-stage-title {
    font-size: clamp(42px, 5vw, 56px);
  }

  .dk-auth-component-scene {
    min-height: 370px;
  }

  .dk-auth-rail {
    padding: 36px;
  }
}

@media (max-width: 899px) {
  .dk-auth-page {
    height: auto;
    min-height: 100dvh;
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: clamp(400px, 46dvh, 470px) minmax(0, 1fr);
    overflow-x: hidden;
    overflow-y: auto;
  }

  .dk-auth-stage {
    min-height: 400px;
    grid-template-rows: auto minmax(250px, 1fr) auto;
    padding: 28px max(28px, var(--dk-safe-right)) 22px max(28px, var(--dk-safe-left));
  }

  .dk-auth-stage-title {
    max-width: 16ch;
    font-size: clamp(34px, 6.2vw, 46px);
  }

  .dk-auth-stage-subtitle {
    max-width: 54ch;
    margin-top: 8px;
    font-size: 13px;
  }

  .dk-auth-component-scene {
    width: min(100%, 700px);
    min-height: 250px;
  }

  .dk-auth-rail {
    min-height: 0;
    border-top: 1px solid var(--dk-border);
    border-left: 0;
    box-shadow: 0 -16px 40px rgba(28, 29, 31, 0.07);
    padding: 28px max(28px, var(--dk-safe-right)) max(28px, var(--dk-safe-bottom))
      max(28px, var(--dk-safe-left));
  }

  .dk-auth-form-body {
    width: min(100%, 420px);
    padding-block: 28px 8px;
  }
}

@media (max-width: 639px) {
  .dk-auth-page {
    grid-template-rows: clamp(350px, 44dvh, 390px) minmax(0, 1fr);
  }

  .dk-auth-stage {
    min-height: 350px;
    grid-template-rows: auto minmax(190px, 1fr);
    padding: 18px max(18px, var(--dk-safe-right)) 14px max(18px, var(--dk-safe-left));
  }

  .dk-auth-stage-title {
    max-width: 15ch;
    font-size: clamp(28px, 8vw, 34px);
  }

  .dk-auth-stage-subtitle {
    max-width: 38ch;
    margin-top: 6px;
    font-size: 11.5px;
    line-height: 1.55;
  }

  .dk-auth-component-scene {
    min-height: 190px;
  }







  .dk-auth-platform-note {
    display: none;
  }

  .dk-auth-rail {
    padding: 22px max(20px, var(--dk-safe-right)) max(22px, var(--dk-safe-bottom))
      max(20px, var(--dk-safe-left));
  }

  .dk-auth-form-body {
    width: 100%;
    padding-block: 24px 0;
  }

  .dk-auth-heading {
    margin-bottom: var(--dk-space-6);
  }

  .dk-auth-title {
    font-size: 26px;
  }

  .dk-field {
    margin-bottom: var(--dk-space-4);
  }

  .dk-auth-note {
    margin-top: var(--dk-space-5);
    padding-top: var(--dk-space-4);
  }
}

@media (max-height: 700px) and (min-width: 900px) {
  .dk-auth-stage {
    padding-block: 26px 22px;
  }

  .dk-auth-stage-title {
    font-size: clamp(38px, 4.3vw, 52px);
  }

  .dk-auth-component-scene {
    min-height: 300px;
  }

  .dk-auth-platform-note {
    display: none;
  }

  .dk-auth-rail {
    padding-block: 24px;
  }

  .dk-auth-form-body {
    padding-block: 24px 0;
  }

  .dk-auth-heading {
    margin-bottom: var(--dk-space-5);
  }
}
</style>
