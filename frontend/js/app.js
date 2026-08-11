// Application entry: session gate, role-aware hash routes and app shell.
import { api, clearSession, getSessionEpoch, getToken, getUser, setSession } from './api.js';
import { renderAppShell } from './core/app-shell.js';
import { navigate, resolveRoute } from './core/router.js';
import { button, closeOverlays, field, h, icon, iconButton, inlineAlert, modal, toast } from './ui.js';
import { clearGenerateSessions, renderGenerate } from './views/generate.js';
import { renderHistory } from './views/history.js';
import { renderTemplates } from './views/templates.js';
import { renderInspiration } from './views/inspiration.js';
import { renderApiKeys } from './views/apikeys.js';
import { renderSettings } from './views/settings.js';
import { renderUsers } from './views/users.js';
import { renderInvites } from './views/invites.js';
import { renderAccount } from './views/account.js';

// adminOnly 的条目由 core/router.js 的 visibleRoutes 过滤掉，侧栏和底部导航自动跟着变。
// 注意：这里读的角色来自 localStorage 里的 dk_user，用户自己改一改就能把菜单点亮——
// 但点进去后端照样 403 / 404。**这是显示层的便利，不是安全边界**，别把它当成权限控制。
const ROUTES = [
  { hash: '#/generate', title: '生成工作台', navLabel: '生成', icon: 'sparkles', render: renderGenerate },
  { hash: '#/history', title: '生成记录', navLabel: '记录', icon: 'images', render: renderHistory },
  { hash: '#/templates', title: '提示词模板', navLabel: '模板', icon: 'layout-template', render: renderTemplates },
  { hash: '#/inspiration', title: '灵感库', navLabel: '灵感', icon: 'lightbulb', render: renderInspiration },
  { hash: '#/users', title: '成员账号', navLabel: '成员', icon: 'users', render: renderUsers, adminOnly: true },
  { hash: '#/invites', title: '邀请码', navLabel: '邀请码', icon: 'key-round', render: renderInvites, adminOnly: true },
  { hash: '#/apikeys', title: 'API 对接', navLabel: 'API', icon: 'plug', render: renderApiKeys, adminOnly: true },
  { hash: '#/settings', title: '系统设置', navLabel: '设置', icon: 'settings', render: renderSettings, adminOnly: true },
  { hash: '#/account', title: '我的账户', navLabel: '我的', icon: 'circle-user', render: renderAccount },
];

const root = document.getElementById('app');
let disposeView = null;
// 「历史数据归属回填」失败时给管理员看的红色横幅文案（后端 /api/web/settings 的
// backfill_error）。整个会话只查一次，避免每次切页都多打一个请求。
let backfillError = '';
let backfillChecked = false;
// 正在显示「先改初始密码」那一页时置 true，防止多个并发请求同时撞上 403
// 把改密页反复重绘、弹出一叠对话框。
let passwordGateActive = false;
// 渲染序号：renderApp 中途要等接口返回，用它丢弃「等回来时已经过期」的那次渲染。
let renderSeq = 0;

// 登录页用到的第三方组件与素材**一律走仓库内的本地文件**，不连任何外部域名。
//
// 为什么：以前这几项是从 cdn.jsdelivr.net 现拉的。公共 CDN 一旦被投毒（或被
// 中间人替换），它塞进来的 JS 就是以本站身份执行的——等于全站 XSS，能把所有
// 在线用户那份 7 天有效的登录令牌整包读走。系统面向公网开放注册之后，这个风险
// 不是「概率低所以先放着」，而是必须直接消掉。
// 顺带的好处：内网/离线部署、以及 CDN 被墙时，登录页照样完整可用。
//
// 升级这些文件的办法：重新下载对应版本覆盖，别改成 URL。各文件的出处、版本与
// 许可见 docs/AUTH_VISUAL_ATTRIBUTIONS.md。
const LORDICON_ELEMENT_URL = '/vendor/lordicon/element.js';
const LORDICON_SHOPPING_URL = '/assets/lottie/morph-shopping.json';
const LORDICON_COINS_URL = '/assets/lottie/coins.json';
const TAG_CLOUD_URL = '/vendor/tagcloud/tagcloud.js';
let lordiconElementModule = null;
let tagCloudModule = null;

// 自助注册页的地址。它是**登录之前**就能打开的两个页面之一（另一个是登录页），
// 所以不能走 ROUTES 那套（那套是登录之后才生效的）。没有令牌时，hash 只有
// 「是不是这一条」这一个含义，见下面 renderAuth。
const REGISTER_HASH = '#/register';
// 注册成功、自动登录之后要弹的那一屏（告诉他生图额度还在开通中）。
// 用一个模块级变量传给 paintApp，而不是直接弹——paintApp 开头的 cleanupView()
// 会把所有浮层关掉，先弹的话会被它顺手关了。
let pendingWelcome = null;
// 从注册页退回登录页时带过来的东西：预填的用户名 + 一条要显示的提示。
// 只用一次，renderLogin 读完就清空。
let loginPreset = null;

/** 问一下后端现在开不开放自助注册。
 *
 * 每次进登录页都重新问，不做长期缓存：这个开关由管理员在「系统设置」里随时开关，
 * 缓存的代价是「管理员刚打开注册，来注册的人却还看不到入口」。
 * 查不到（断网、后端是不带这条接口的老版本）一律当成**没开放**——
 * 宁可少一个入口，也不要让人点进去被拒绝。
 */
async function fetchRegisterStatus() {
  try {
    const status = await api.get('/api/web/register');
    return status && typeof status === 'object' ? status : null;
  } catch {
    return null;
  }
}

// 平台标识：能用自由许可（Simple Icons，CC0）的一律放成仓库内 SVG。
//
// 京东 / 拼多多 / Amazon / Walmart 这四家 Simple Icons 没有收录（该项目按商标
// 权利人要求下架了这些条目），原先是从维基百科/维基共享资源热链的图片。热链既是
// 外部请求，收录许可也不都是自由许可（京东那张在英文维基是「合理使用」，本就不
// 该随仓库分发）。所以这四家改成 `text` 文字标——不外链、不分发他人商标图形，
// 展示效果上仍然认得出是哪家平台。
const COMMERCE_CHANNELS = [
  { key: 'taobao', label: '淘宝', market: '中国', src: '/assets/platforms/taobao.svg' },
  { key: 'tmall', label: '天猫', market: '中国', src: '/assets/platforms/tmall.svg', wide: true },
  { key: 'jd', label: '京东', market: '中国', text: 'JD.com', wide: true },
  { key: 'pinduoduo', label: '拼多多', market: '中国', text: '拼多多', wide: true },
  { key: 'douyin', label: '抖音电商', market: '中国', src: '/assets/platforms/tiktok.svg' },
  { key: 'xiaohongshu', label: '小红书', market: '中国', src: '/assets/platforms/xiaohongshu.svg' },
  { key: 'amazon', label: 'Amazon', market: '全球', text: 'Amazon', wide: true },
  { key: 'ebay', label: 'eBay', market: '全球', src: '/assets/platforms/ebay.svg', wide: true },
  { key: 'shopify', label: 'Shopify', market: '全球', src: '/assets/platforms/shopify.svg' },
  { key: 'etsy', label: 'Etsy', market: '欧美', src: '/assets/platforms/etsy.svg' },
  { key: 'aliexpress', label: 'AliExpress', market: '全球', src: '/assets/platforms/aliexpress.svg', wide: true },
  { key: 'shopee', label: 'Shopee', market: '东南亚', src: '/assets/platforms/shopee.svg' },
  { key: 'rakuten', label: 'Rakuten', market: '日本', src: '/assets/platforms/rakuten.svg', wide: true },
  { key: 'walmart', label: 'Walmart', market: '北美', text: 'Walmart', wide: true },
  { key: 'tiktok-shop', label: 'TikTok Shop', market: '全球', src: '/assets/platforms/tiktok.svg' },
];

function ensureLordiconElement() {
  if (!lordiconElementModule) {
    lordiconElementModule = import(LORDICON_ELEMENT_URL).then((module) => {
      module.defineElement();
      return module;
    });
  }
  return lordiconElementModule;
}

function ensureTagCloud() {
  if (!tagCloudModule) {
    tagCloudModule = import(TAG_CLOUD_URL).then((module) => module.default);
  }
  return tagCloudModule;
}

function commerceChannelToken({ key, label, market, src, text, wide = false }) {
  // 文字标没有对应的 CSS 类（frontend/css 不归本次改动管），所以字号字重写成
  // 行内样式；外层 .dk-auth-logo-token 已经给了配色、内边距和不换行。
  const body = src
    ? h('img', {
      class: `dk-auth-logo-token__image${wide ? ' dk-auth-logo-token__image--wide' : ''}`,
      src,
      alt: '',
      width: '92',
      height: '34',
      decoding: 'async',
      draggable: 'false',
      referrerpolicy: 'no-referrer',
    })
    : h('span', {
      class: 'dk-auth-logo-token__text',
      style: 'font-size:14px;font-weight:700;letter-spacing:-0.01em;line-height:1;',
    }, text);
  return h('span', {
    class: `dk-auth-logo-token${wide ? ' dk-auth-logo-token--wide' : ''} dk-auth-logo-token--${key}`,
    title: `${label} · ${market}`,
  }, body);
}

function commerceChannelMarkup(channel) {
  return commerceChannelToken(channel).outerHTML;
}

function setThemeColor(color) {
  const themeMeta = document.querySelector('meta[name="theme-color"]');
  if (themeMeta) themeMeta.content = color;
}

function cleanupView() {
  if (disposeView) {
    try { disposeView(); } catch (error) { console.warn('View cleanup failed', error); }
    disposeView = null;
  }
  closeOverlays();
}

function passwordControl({ id, autocomplete, placeholder = '' }) {
  const input = h('input', { id, class: 'dk-control input', type: 'password', autocomplete, placeholder });
  const toggle = iconButton('eye', '显示密码', {
    className: 'dk-password-toggle',
    onclick: () => {
      const visible = input.type === 'text';
      input.type = visible ? 'password' : 'text';
      toggle.replaceChildren(icon(visible ? 'eye' : 'eye-off'));
      toggle.setAttribute('aria-label', visible ? '显示密码' : '隐藏密码');
      toggle.title = visible ? '显示密码' : '隐藏密码';
      input.focus();
    },
  });
  return { input, wrap: h('div', { class: 'dk-password-control' }, input, toggle) };
}

function mountLoginStage(page) {
  let disposed = false;
  let tagCloudFactory = null;
  let tagCloudInstance = null;
  let resizeObserver = null;
  let resizeTimer = null;
  let lastRadius = 0;
  let workflowReady = false;
  let cloudReady = false;
  let componentFailed = false;
  const prefersReducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const logoCloud = page.querySelector('.dk-auth-logo-cloud');
  const lordiconPlayers = [...page.querySelectorAll('.dk-auth-workflow-player')];

  function renderStaticLogoTokens() {
    if (!logoCloud?.isConnected || disposed) return;
    logoCloud.replaceChildren(...COMMERCE_CHANNELS.map(commerceChannelToken));
  }

  function updateComponentsState() {
    if (prefersReducedMotion) {
      page.dataset.componentsState = 'static';
    } else if (componentFailed) {
      page.dataset.componentsState = 'fallback';
    } else if (workflowReady && cloudReady) {
      page.dataset.componentsState = 'ready';
    } else {
      page.dataset.componentsState = 'loading';
    }
  }

  function cloudRadius() {
    const width = logoCloud?.clientWidth || page.clientWidth || window.innerWidth;
    if (window.matchMedia('(max-width: 639px)').matches) {
      return Math.max(108, Math.min(124, Math.round(width * 0.31)));
    }
    return Math.max(214, Math.min(246, Math.round(width * 0.31)));
  }

  function destroyTagCloud({ restoreStatic = false } = {}) {
    tagCloudInstance?.pause?.();
    tagCloudInstance?.destroy?.();
    tagCloudInstance = null;
    if (restoreStatic) renderStaticLogoTokens();
  }

  function initializeTagCloud() {
    if (disposed || prefersReducedMotion || componentFailed || !tagCloudFactory || !logoCloud?.isConnected) return;
    const radius = cloudRadius();
    destroyTagCloud();
    logoCloud.replaceChildren();
    try {
      tagCloudInstance = tagCloudFactory(
        logoCloud,
        COMMERCE_CHANNELS.map(commerceChannelMarkup),
        {
          radius,
          maxSpeed: 'slow',
          initSpeed: 'slow',
          direction: 135,
          keep: true,
          useHTML: true,
          useContainerInlineStyles: false,
          useItemInlineStyles: true,
        },
      );
      lastRadius = radius;
      cloudReady = true;
      updateComponentsState();
      syncRunningState();
    } catch (error) {
      componentFailed = true;
      cloudReady = false;
      renderStaticLogoTokens();
      updateComponentsState();
      console.warn('Commerce logo cloud failed to initialize', error);
    }
  }

  function scheduleTagCloudLayout(delay = 0) {
    if (resizeTimer) window.clearTimeout(resizeTimer);
    resizeTimer = window.setTimeout(() => {
      resizeTimer = null;
      if (disposed || !tagCloudFactory) return;
      const nextRadius = cloudRadius();
      if (!tagCloudInstance || Math.abs(nextRadius - lastRadius) >= 10) initializeTagCloud();
    }, delay);
  }

  function syncRunningState() {
    const shouldRun = !document.hidden && !prefersReducedMotion;
    page.dataset.stageRunning = shouldRun ? 'running' : 'paused';
    if (tagCloudInstance) {
      if (shouldRun) tagCloudInstance.resume?.();
      else tagCloudInstance.pause?.();
    }
    for (const lordicon of lordiconPlayers) {
      const player = lordicon.playerInstance;
      if (!player) continue;
      if (shouldRun) player.play?.();
      else player.pause?.();
    }
  }

  page.dataset.componentsState = prefersReducedMotion ? 'static' : 'loading';
  if (prefersReducedMotion) {
    for (const lordicon of lordiconPlayers) lordicon.removeAttribute('trigger');
  }
  syncRunningState();
  document.addEventListener('visibilitychange', syncRunningState);

  ensureLordiconElement().then(async () => {
    if (disposed || !logoCloud?.isConnected) return;
    await Promise.all(lordiconPlayers.map((lordicon) => lordicon.readyPromise));
    if (disposed) return;
    workflowReady = true;
    updateComponentsState();
    syncRunningState();
  }).catch((error) => {
    if (disposed) return;
    componentFailed = true;
    destroyTagCloud({ restoreStatic: true });
    updateComponentsState();
    console.warn('Commerce workflow components failed to load', error);
  });

  if (!prefersReducedMotion) {
    ensureTagCloud().then((TagCloud) => {
      if (disposed || !logoCloud?.isConnected) return;
      if (componentFailed) {
        renderStaticLogoTokens();
        return;
      }
      tagCloudFactory = TagCloud;
      scheduleTagCloudLayout();
      if ('ResizeObserver' in window) {
        resizeObserver = new ResizeObserver(() => scheduleTagCloudLayout(100));
        resizeObserver.observe(logoCloud);
      }
    }).catch((error) => {
      if (disposed) return;
      componentFailed = true;
      cloudReady = false;
      renderStaticLogoTokens();
      updateComponentsState();
      console.warn('Commerce logo cloud component failed to load', error);
    });
  } else {
    cloudReady = true;
    renderStaticLogoTokens();
  }

  return () => {
    disposed = true;
    document.removeEventListener('visibilitychange', syncRunningState);
    if (resizeTimer) window.clearTimeout(resizeTimer);
    resizeObserver?.disconnect();
    tagCloudInstance?.pause?.();
    tagCloudInstance?.destroy?.();
    tagCloudInstance = null;
    for (const lordicon of lordiconPlayers) lordicon.playerInstance?.pause?.();
  };
}

/** 登录页与注册页共用的那张「舞台」：左边商品渠道动效，右边一张表单卡片。
 *
 * 两个页面套同一个外壳，是为了让「去注册」看起来是同一个系统的下一屏，
 * 而不是跳去了另一个网站——非技术用户对「界面突然变样」是很敏感的，
 * 会以为自己点到了钓鱼页面而不敢往下填。
 *
 * 返回值里的 formBody 是右侧那一格，调用方把自己的表单塞进去。
 * 舞台上那些动效由 mountLoginStage 接管，两个页面各自渲染时**只换 formBody**
 * 的话动效不会重来一遍；整页重画（renderLogin / renderRegister 各调一次）
 * 则会重新挂载，这也没问题——它们本来就是两次独立的进入。
 */
function authScaffold() {
  function workflowCard(iconComponent, title, copy) {
    return h('article', { class: 'dk-auth-workflow-card' },
      h('span', { class: 'dk-auth-workflow-icon', 'aria-hidden': 'true' }, iconComponent),
      h('span', { class: 'dk-auth-workflow-copy' },
        h('strong', {}, title),
        h('span', {}, copy)));
  }

  function workflowPlayer(src, fallbackIcon) {
    return h('lord-icon', {
      class: 'dk-auth-workflow-player',
      src,
      trigger: 'loop',
      speed: '0.72',
      colors: 'primary:#101828,secondary:#2563eb,tertiary:#7c3aed',
      'aria-hidden': 'true',
    }, icon(fallbackIcon, { size: 22 }));
  }

  const logoCloud = h('div', {
    class: 'dk-auth-logo-cloud',
    'aria-hidden': 'true',
  }, COMMERCE_CHANNELS.map(commerceChannelToken));

  const componentScene = h('div', { class: 'dk-auth-component-scene' },
    logoCloud,
    h('div', { class: 'dk-auth-workflow', 'aria-label': '商品内容处理流程' },
      workflowCard(icon('images', { size: 22 }), '素材管理', '统一导入与归档'),
      workflowCard(workflowPlayer(LORDICON_COINS_URL, 'sliders-horizontal'), '图像处理', '去背、修图与场景优化'),
      workflowCard(icon('layout-template', { size: 22 }), '规格适配', '生成渠道尺寸与版式'),
      workflowCard(workflowPlayer(LORDICON_SHOPPING_URL, 'package'), '渠道交付', '输出平台发布素材')));

  const formBody = h('div', { class: 'dk-auth-form-body' });
  const page = h('main', { class: 'dk-auth-page' },
    h('section', { class: 'dk-auth-stage', 'aria-label': '跨平台商品内容处理' },
      h('header', { class: 'dk-auth-stage-copy' },
        h('p', { class: 'dk-auth-stage-title' }, '一套商品素材，\n适配多端渠道。'),
        h('p', { class: 'dk-auth-stage-subtitle' }, '统一处理商品图片、背景与渠道规格，生成可直接用于各销售平台的发布素材。')),
      componentScene,
      h('p', { class: 'dk-auth-platform-note' }, '平台标识仅用于兼容性说明，相关商标归各自权利人所有。')),
    h('section', { class: 'dk-auth-rail' },
      h('div', { class: 'dk-auth-brand' },
        h('span', { class: 'dk-auth-wordmark' }, 'DesignKit')),
      formBody));
  root.replaceChildren(page);
  disposeView = mountLoginStage(page);
  return { page, formBody };
}

/** 没有登录令牌时该画哪一页。
 *
 * 未登录状态下能打开的页面只有两个：登录页和注册页。ROUTES 那一套是登录之后
 * 才生效的（它要按角色过滤菜单），所以这里不用 resolveRoute，只认一条 hash。
 */
function renderAuth() {
  if (window.location.hash === REGISTER_HASH) renderRegister();
  else renderLogin();
}

function renderLogin() {
  cleanupView();
  passwordGateActive = false;
  // 回到登录页 = 换人了：健康标志要重新查一次，否则下一个登录的管理员
  // 看到的可能是上一个人那次查询的结果。
  backfillChecked = false;
  backfillError = '';
  clearGenerateSessions();
  document.title = '登录 · DesignKit';
  setThemeColor('#f3f5f7');
  // 从注册页退回来时带的东西（预填用户名 + 一条提示），读完就清掉：
  // 留着的话，下一次正常打开登录页会莫名其妙又显示一遍上次那条提示。
  const preset = loginPreset;
  loginPreset = null;
  // 不预填用户名：多人使用之后，这里预填 admin 会让每个成员都要先把它删掉再打自己的名字。
  // 例外是刚注册完那一次——那时预填的是他自己刚起的名字，省一次输入。
  const username = h('input', {
    class: 'dk-control input',
    autocomplete: 'username',
    value: preset?.username || '',
  });
  const password = passwordControl({ autocomplete: 'current-password' });
  const errorRegion = h('div', { id: 'dk-login-error', class: 'dk-form-message', tabindex: '-1', 'aria-live': 'assertive' });
  const submit = button('登录', { size: 'lg', type: 'submit', className: 'dk-auth-submit' });

  function setFieldError(input, invalid) {
    const references = new Set((input.getAttribute('aria-describedby') || '').split(/\s+/).filter(Boolean));
    if (invalid) {
      references.add(errorRegion.id);
      input.setAttribute('aria-invalid', 'true');
    } else {
      references.delete(errorRegion.id);
      input.removeAttribute('aria-invalid');
    }
    if (references.size) input.setAttribute('aria-describedby', [...references].join(' '));
    else input.removeAttribute('aria-describedby');
  }

  username.addEventListener('input', () => setFieldError(username, false));
  password.input.addEventListener('input', () => setFieldError(password.input, false));

  async function login(event) {
    event?.preventDefault();
    errorRegion.replaceChildren();
    setFieldError(username, false);
    setFieldError(password.input, false);
    const name = username.value.trim();
    if (!name || !password.input.value) {
      errorRegion.append(inlineAlert('请输入用户名和密码。', 'error'));
      if (!name) setFieldError(username, true);
      if (!password.input.value) setFieldError(password.input, true);
      (!name ? username : password.input).focus();
      return;
    }
    submit.disabled = true;
    submit.dataset.loading = 'true';
    submit.replaceChildren(icon('loader-circle', { className: 'dk-spin' }), h('span', { class: 'dk-button-label' }, '正在登录…'));
    try {
      const data = await api.post('/api/web/auth/login', { username: name, password: password.input.value });
      setSession(data.token, data.user);
      navigate('#/generate', { replace: true });
      renderApp();
    } catch (error) {
      errorRegion.append(inlineAlert(error.message, 'error'));
      errorRegion.focus();
      submit.disabled = false;
      submit.dataset.loading = 'false';
      submit.replaceChildren(h('span', { class: 'dk-button-label' }, '登录'));
    }
  }

  // 「我有邀请码，去注册」这个入口先留一个空位，等后端回话再决定填不填。
  // **注册没开放时绝不能显示它**：点进去必然被拒，而被拒的那句话按设计
  // 又不解释原因（对公网不该解释），用户只会以为系统坏了。
  const registerEntry = h('div', { class: 'dk-auth-alt' });

  const form = h('form', { class: 'dk-auth-card', novalidate: true, onsubmit: login },
    h('div', { class: 'dk-auth-heading' },
      h('h1', { class: 'dk-auth-title' }, '欢迎回来'),
      h('p', { class: 'dk-auth-subtitle' }, '登录后管理商品素材、生成任务与渠道模板。')),
    errorRegion,
    field('用户名', username, { required: true }),
    field('密码', password.wrap, { required: true }),
    submit,
    registerEntry,
    // 这句话是给「刚把系统装起来、还不知道怎么进去」的人看的。
    // 完全删掉会让首次部署的人找不到入口；但把默认口令印在登录页上，
    // 多人使用之后就不合适了——所以只说管理员那一个账号，成员一律由管理员建号。
    h('p', { class: 'dk-auth-note' },
      icon('shield-check', { size: 15 }),
      h('span', {}, '首次部署：管理员账号 admin / 初始密码 admin123456，登录后立即修改；'
        + '成员账号请由管理员在「成员账号」页创建。')));

  // 刚注册完退回来的那条提示（后端那句「请用刚才设置的用户名和密码登录」）
  if (preset?.message) errorRegion.append(inlineAlert(preset.message, preset.tone || 'success'));

  const { formBody } = authScaffold();
  formBody.append(form);
  // 注册入口的显示与否要等一个网络请求。等回来时页面可能已经被换掉了
  //（用户手快点了别的、或者令牌恢复直接进了工作台），所以先看还在不在 DOM 上。
  fetchRegisterStatus().then((status) => {
    if (!registerEntry.isConnected || !status?.enabled) return;
    registerEntry.append(
      h('span', { class: 'dk-auth-alt__text' }, '还没有账号？'),
      h('a', { class: 'dk-auth-alt__link', href: REGISTER_HASH },
        icon('key-round', { size: 15 }),
        h('span', {}, '我有邀请码，去注册')),
    );
  });
  queueMicrotask(() => {
    if (!username.isConnected) return;
    // 预填了用户名（刚注册完）就直接把光标放到密码框，少一次点击
    if (preset?.username) password.input.focus();
    else username.focus();
  });
}

/** 自助注册页。**不需要登录就能打开**，所以它面对的是公网上的陌生人。
 *
 * 三条写这一页时定下的规矩：
 *
 * 一、**规则文案一律用后端返回的那份**（username_rule / password_rule），
 *    不在前端另抄一份。抄一份的代价是哪天后端把密码下限从 8 位改成 10 位，
 *    页面上还写着 8 位，用户照着填、被报错说不对，两边谁也说不清该信哪个。
 *
 * 二、**不在前端做格式校验**（邀请码要不要 12 位、用户名合不合法）。
 *    邀请码后端会自己归一化（大小写、连字符、把看错打成的 I/O 换回 1/0），
 *    前端多拦一道只会把本来能过的输入挡在门外。这里只检查「有没有填」。
 *
 * 三、**报错原样显示**。后端那几句已经是给非技术用户写的中文，
 *    而且刻意不区分「邀请码不存在」和「已用完」（区分了就成了枚举工具）——
 *    前端更不该自作主张去猜一个更具体的说法。
 */
async function renderRegister() {
  cleanupView();
  passwordGateActive = false;
  backfillChecked = false;
  backfillError = '';
  clearGenerateSessions();
  document.title = '注册 · DesignKit';
  setThemeColor('#f3f5f7');

  const { page, formBody } = authScaffold();
  formBody.append(h('div', { class: 'dk-auth-card' },
    h('div', { class: 'dk-auth-heading' },
      h('h1', { class: 'dk-auth-title' }, '用邀请码注册'),
      h('p', { class: 'dk-auth-subtitle' }, '正在确认是否开放注册…'))));

  // 规则文案要等这个请求，所以先画个「正在确认」，再把真表单换上去。
  // 直接敲地址进来（比如别人把注册页链接发给他）时，这一步就是必须的。
  const status = await fetchRegisterStatus();
  if (!page.isConnected) return;

  if (!status?.enabled) {
    // 关着的时候只说「没开放」，不解释为什么——理由见 backend/routers/register.py。
    // 但要给一条明确的出路：回登录页，以及去找谁。
    formBody.replaceChildren(h('div', { class: 'dk-auth-card' },
      h('div', { class: 'dk-auth-heading' },
        h('h1', { class: 'dk-auth-title' }, '暂时不能自助注册'),
        h('p', { class: 'dk-auth-subtitle' },
          status?.message || '现在没有开放自助注册，请联系管理员帮你开通账号。')),
      button('返回登录', {
        size: 'lg',
        className: 'dk-auth-submit',
        onclick: () => backToLogin(),
      })));
    return;
  }

  const invite = h('input', {
    class: 'dk-control input',
    autocomplete: 'off',
    autocapitalize: 'characters',
    spellcheck: 'false',
    placeholder: '例如 ABCD-EFGH-JKMN',
  });
  const username = h('input', { class: 'dk-control input', autocomplete: 'username' });
  const displayName = h('input', { class: 'dk-control input', autocomplete: 'name', placeholder: '例如：张三（不填就用用户名）' });
  const password = passwordControl({ autocomplete: 'new-password' });
  const errorRegion = h('div', { class: 'dk-form-message', tabindex: '-1', 'aria-live': 'assertive' });
  const submit = button('注册并进入', { size: 'lg', type: 'submit', className: 'dk-auth-submit' });

  function setSubmitState(loading, label) {
    submit.disabled = loading;
    submit.dataset.loading = loading ? 'true' : 'false';
    // filter(Boolean) 不能省：replaceChildren 会把 null 按 DOM 规范转成字符串，
    // 按钮上会多出一个「null」（h() 会过滤 null，replaceChildren 不会）。
    submit.replaceChildren(...[
      loading ? icon('loader-circle', { className: 'dk-spin' }) : null,
      h('span', { class: 'dk-button-label' }, label),
    ].filter(Boolean));
  }

  async function submitRegister(event) {
    event?.preventDefault();
    errorRegion.replaceChildren();
    const code = invite.value.trim();
    const name = username.value.trim();
    const secret = password.input.value;
    // 只拦「没填」。格式一律交给后端判断（见函数头第二条）。
    if (!code || !name || !secret) {
      errorRegion.append(inlineAlert('邀请码、用户名和密码都要填。', 'error'));
      (!code ? invite : (!name ? username : password.input)).focus();
      return;
    }
    setSubmitState(true, '正在注册…');
    let created = null;
    try {
      created = await api.post('/api/web/register', {
        invite_code: code,
        username: name,
        password: secret,
        display_name: displayName.value.trim(),
      });
    } catch (error) {
      if (!page.isConnected) return;
      // 后端的报错已经是能直接显示的中文，包括限速那条（正文里带了「请 X 分钟后再试」）。
      errorRegion.append(inlineAlert(error.message, 'error'));
      // 403 = 注册在他填表的这几分钟里被管理员关掉了。留在这一页反复点没有意义，
      // 给一个回登录页的按钮。
      if (error.status === 403) {
        errorRegion.append(button('返回登录', { variant: 'secondary', onclick: () => backToLogin() }));
      }
      errorRegion.focus();
      setSubmitState(false, '注册并进入');
      return;
    }

    if (!page.isConnected) return;
    // 注册接口**故意不发登录令牌**（理由见 backend/app/routers/register.py）。
    // 但对一个刚填完四个字段的人来说，再把他推回登录页重打一遍用户名密码
    // 是白白多出来的一道坎，所以这里拿他刚填的密码替他登一次。
    // 密码只活在这个函数的局部变量里，不写 localStorage、不进 URL。
    setSubmitState(true, '正在进入…');
    try {
      const data = await api.post('/api/web/auth/login', {
        username: created.username,
        password: secret,
      });
      if (!page.isConnected) return;
      setSession(data.token, data.user);
      // 「生图额度还在开通中」这句话必须让他看见，所以不用 toast（几秒就没了），
      // 而是进工作台之后弹一屏，由 paintApp 在画完页面之后打开。
      pendingWelcome = {
        name: created.display_name || created.username,
        username: created.username,
      };
      navigate('#/generate', { replace: true });
      renderApp();
    } catch (error) {
      if (!page.isConnected) return;
      // 自动登录失败（登录接口正好被限速、网络断了……）**不代表注册失败**——
      // 账号是真的建好了。这里必须说清楚，否则他会以为要重来一遍，
      // 于是换个名字再注册一次，白白多废一张邀请码。
      loginPreset = {
        username: created.username,
        tone: 'success',
        message: `${created.message || '注册成功。'}（这次没能自动帮你登录：${error.message}）`,
      };
      backToLogin();
    }
  }

  const form = h('form', { class: 'dk-auth-card', novalidate: true, onsubmit: submitRegister },
    h('div', { class: 'dk-auth-heading' },
      h('h1', { class: 'dk-auth-title' }, '用邀请码注册'),
      h('p', { class: 'dk-auth-subtitle' }, '填好下面四项就能开一个自己的账号，生成记录和商品图各归各的。')),
    errorRegion,
    field('邀请码', invite, {
      required: true,
      help: '管理员发给你的那一串字母数字。带不带连字符、大写小写都行，粘贴过来就可以。',
    }),
    field('用户名', username, { required: true, help: status.username_rule || '' }),
    field('显示名（选填）', displayName, { help: '同事在系统里看到的名字，不填就显示用户名。' }),
    field('密码', password.wrap, { required: true, help: status.password_rule || '' }),
    submit,
    h('div', { class: 'dk-auth-alt' },
      h('span', { class: 'dk-auth-alt__text' }, '已经有账号了？'),
      h('a', {
        class: 'dk-auth-alt__link',
        href: '#',
        onclick: (event) => { event.preventDefault(); backToLogin(); },
      }, icon('arrow-left', { size: 15 }), h('span', {}, '返回登录'))),
    h('p', { class: 'dk-auth-note' },
      icon('shield-check', { size: 15 }),
      h('span', {}, '注册成功后会直接进入工作台。生图额度由系统在后台自动开通，'
        + '通常一两分钟，期间其他功能都能正常用。')));

  formBody.replaceChildren(form);
  queueMicrotask(() => { if (invite.isConnected) invite.focus(); });
}

/** 从注册页回到登录页。
 * 顺手把地址里的 #/register 抹掉：留着的话，用户在登录页按一下浏览器刷新，
 * 又会被送回注册页，而他刚刚才决定不注册。
 */
function backToLogin() {
  history.replaceState(null, '', window.location.pathname);
  renderLogin();
}

/** 注册成功、已经自动登录之后的那一屏。
 *
 * 这一屏存在的唯一理由是**说清楚生图额度还没好**：自动开通是异步的
 *（注册接口一步外部请求都不发，真正去网关建号是后台慢慢做的），
 * 不说的话，新用户进来第一件事就是点「生成」，然后看到一句「还不能生图」，
 * 得出的结论是「这系统坏了」。
 *
 * 用弹窗而不是 toast：toast 2.6 秒就消失，而这段话他必须读到。
 */
function showWelcomeAfterRegister(info) {
  const dialog = modal({
    title: `欢迎，${info.name}`,
    body: h('div', { class: 'dk-stack' },
      inlineAlert('账号已经建好，也帮你登录进来了。', 'success'),
      h('p', {},
        h('strong', {}, '生图额度正在后台开通中，通常一两分钟完成。'),
        ' 这段时间里模板、灵感库、历史记录都能正常用；'
        + '如果点「生成」时提示还不能生图，等一两分钟刷新一下页面再试就行。'),
      h('p', {}, '要是过了十分钟还是不行，找管理员看一眼即可——你这边不需要做任何事。'),
      h('p', {}, '你的用户名是 ', h('code', {}, info.username), '，下次登录用它和刚才设置的密码。')),
    footer: [button('开始使用', { onclick: () => dialog.close('ok') })],
  });
}

/** 向服务端确认一次「我是谁」，并把结果写回本地。
 *
 * 这一步**不能因为「本地已经有 user 缓存」就跳过**，它同时承担两件事：
 * 1. 后端会顺手补发取图凭证（dk_files Cookie）。升级当天，所有已经登录着的人
 *    手上只有 localStorage 里的令牌、并没有这个 Cookie，跳过这一步的直接后果
 *    是他们打开页面看到满屏裂图，而且刷新多少次都一样。
 * 2. 顺便刷新可能已经变了的角色、以及「要不要强制改密码」。
 * 令牌过期（401）由 api.js 统一处理：清会话 + 派发 dk-unauthorized 回登录页，
 * 这里不用重复写。断网之类的其他错误则继续用本地缓存渲染，不把人挡在门外。
 */
async function refreshSession() {
  const token = getToken();
  try {
    const me = await api.get('/api/web/auth/me');
    if (getToken() !== token) return;
    // 只有内容真的变了才写回：setSession 会让会话计数 +1，
    // 而各个页面用它判断「请求发出后用户是不是换人了」，无谓地 +1 会让在途请求白白作废。
    if (JSON.stringify(getUser()) !== JSON.stringify(me)) setSession(token, me);
  } catch (error) {
    if (error?.status !== 401) console.warn('刷新登录状态失败，暂时沿用本地缓存', error);
  }
}

/** 管理员专用：历史数据归属回填失败时，顶部要挂一条红色横幅。
 * 标志来自 GET /api/web/settings 的 backfill_ok / backfill_error（后端 P1-3 写入）。
 * 只查一次：这个状态只在服务启动时写一次，不会在使用过程中自己变好。
 */
async function ensureBackfillFlag(user) {
  if (backfillChecked || user.role !== 'admin') return;
  backfillChecked = true;
  try {
    const settings = await api.get('/api/web/settings');
    if (settings && settings.backfill_ok === false) {
      backfillError = String(settings.backfill_error
        || '历史数据的归属回填没有全部成功，请查看服务器日志后重启一次服务。');
    }
  } catch { /* 查不到就不显示横幅，不影响任何功能 */ }
}

function backfillBanner(user) {
  if (!backfillError || user.role !== 'admin') return null;
  return h('div', { class: 'dk-shell-banner', role: 'alert' },
    icon('circle-alert', { size: 18 }),
    h('div', {},
      h('strong', { class: 'dk-shell-banner__title' }, '历史数据归属回填没有完成'),
      // backfill_error 后端写的就是给非技术用户看的中文，自带处置建议，原样显示
      h('span', {}, backfillError)));
}

async function renderApp() {
  // 这个函数中途要等服务端回话，而用户完全可能在等待期间又点了一次导航。
  // 不记序号的话，两次渲染会先后各画一遍：先画的那个页面已经被覆盖掉了，
  // 它的清理函数却被后画的那个顶掉，于是它的轮询定时器永远没人停——
  // 表现是切几次页面之后请求越来越多。这里只让最后一次渲染算数。
  const seq = ++renderSeq;
  if (!getToken() || !getUser()) {
    // 没登录时能打开的是登录页或注册页，由 hash 决定
    renderAuth();
    return;
  }
  // 先把会话刷新掉再动界面：中途如果发现令牌已经失效，
  // 用户看到的是直接回到登录页，而不是先闪一下空白的工作台。
  await refreshSession();
  const user = getUser();
  if (!getToken() || !user) return; // 刷新过程中被登出，dk-unauthorized 已经接手
  await ensureBackfillFlag(user);
  if (!getToken() || seq !== renderSeq) return;
  paintApp(user);
}

function paintApp(user) {
  cleanupView();
  setThemeColor('#f3f5f7');
  if (user.must_change_password) {
    renderPasswordGate(user);
    return;
  }
  passwordGateActive = false;
  const route = resolveRoute(ROUTES, user);
  if (window.location.hash !== route.hash) navigate(route.hash, { replace: true });
  document.title = `${route.title} · DesignKit`;
  const { main } = renderAppShell({
    root, user, route, routes: ROUTES, onLogout: logout, banner: backfillBanner(user),
  });
  const cleanup = route.render(main, user);
  disposeView = typeof cleanup === 'function' ? cleanup : null;
  // 刚注册完自动登录进来的那一次：等页面画完再弹「额度正在开通中」。
  // 不能在注册页那边就弹——paintApp 开头的 cleanupView() 会把所有浮层关掉。
  if (pendingWelcome) {
    const info = pendingWelcome;
    pendingWelcome = null;
    showWelcomeAfterRegister(info);
  }
}

function renderPasswordGate(user) {
  passwordGateActive = true;
  const route = ROUTES[0];
  if (window.location.hash !== route.hash) navigate(route.hash, { replace: true });
  document.title = '设置安全密码 · DesignKit';
  const { main } = renderAppShell({
    root, user, route, routes: ROUTES, onLogout: logout, banner: backfillBanner(user),
  });
  main.replaceChildren(h('section', { class: 'dk-session-gate', 'aria-label': '首次登录安全设置' },
    h('div', { class: 'dk-session-gate__content' },
      h('span', { class: 'dk-session-gate__icon', 'aria-hidden': 'true' }, icon('shield-check', { size: 28 })),
      h('h1', {}, '先完成安全设置'),
      h('p', {}, '默认密码仅用于首次进入。设置新密码后即可使用生成工作台。'))));
  forceChangePassword(user);
}

function forceChangePassword(user) {
  let dialog;
  const oldPassword = passwordControl({ autocomplete: 'current-password', placeholder: '当前使用的初始密码' });
  const newPassword = passwordControl({ autocomplete: 'new-password', placeholder: '至少 8 位' });
  const confirmPassword = passwordControl({ autocomplete: 'new-password', placeholder: '再次输入新密码' });
  const feedback = h('div', { class: 'dk-form-message', tabindex: '-1', 'aria-live': 'assertive' });
  const requirement = h('div', { class: 'dk-password-requirements', 'aria-live': 'polite' }, '至少 8 位；两次输入需要一致。');
  const save = button('设置新密码并进入', { size: 'lg', onclick: submit });

  function validate() {
    const enough = newPassword.input.value.length >= 8;
    const matches = Boolean(newPassword.input.value) && newPassword.input.value === confirmPassword.input.value;
    requirement.textContent = `${enough ? '长度符合要求' : '至少需要 8 位'}；${matches ? '两次输入一致' : '两次输入尚未一致'}。`;
    requirement.dataset.valid = enough && matches ? 'true' : 'false';
    return enough && matches;
  }

  async function submit() {
    feedback.replaceChildren();
    if (!validate()) {
      feedback.append(inlineAlert('请检查新密码要求。', 'error'));
      feedback.focus();
      return;
    }
    const sessionAtSubmit = getSessionEpoch();
    save.disabled = true;
    cancel.disabled = true;
    try {
      const response = await api.post('/api/web/auth/change_password', {
        old_password: oldPassword.input.value,
        new_password: newPassword.input.value,
      });
      if (sessionAtSubmit !== getSessionEpoch()) return;
      if (response.token) setSession(response.token, { ...user, must_change_password: false });
      dialog.close('password-changed');
      toast('密码已设置', 'success');
      navigate('#/generate', { replace: true });
      renderApp();
    } catch (error) {
      if (sessionAtSubmit !== getSessionEpoch()) return;
      feedback.append(inlineAlert(error.message, 'error'));
      feedback.focus();
      save.disabled = false;
      cancel.disabled = false;
    }
  }

  newPassword.input.addEventListener('input', validate);
  confirmPassword.input.addEventListener('input', validate);
  const cancel = button('退出登录', { variant: 'quiet', onclick: logout });
  dialog = modal({
    title: '首次登录：设置安全密码',
    closeable: false,
    body: h('div', { class: 'dk-password-change' },
      inlineAlert('默认密码仅用于首次进入。完成修改后，旧登录令牌会立即失效。', 'warning'),
      feedback,
      field('当前密码', oldPassword.wrap, { required: true }),
      field('新密码', newPassword.wrap, { required: true, help: '建议使用与其他系统不同的密码。' }),
      field('确认新密码', confirmPassword.wrap, { required: true }),
      requirement),
    footer: [cancel, save],
  });
}

function logout() {
  // 必须先招呼一声服务端：POST /auth/logout 会把取图凭证（dk_files Cookie）删掉。
  // 只清 localStorage 的话，用户在公用电脑上点完「退出登录」，
  // 那张 Cookie 最长还能再用 7 天——下一个人直接敲图片地址，
  // 就能把他的商品图和出图全看走，而界面上明明写着「已退出」。
  // 不 await：清本地和回登录页要立刻发生，网络慢不该让人干等；
  // 请求失败也不挡住退出流程（Cookie 删不掉是次要问题，本地不清才是大问题）。
  api.post('/api/web/auth/logout').catch(() => {});
  cleanupView();
  clearSession();
  passwordGateActive = false;
  history.replaceState(null, '', window.location.pathname);
  renderLogin();
}

// 未登录时也要响应 hash：登录页 ↔ 注册页就是靠 #/register 这一条来回切的。
// 少了 else 那半截，点「我有邀请码，去注册」会只改地址栏、页面一动不动。
window.addEventListener('hashchange', () => {
  if (getToken()) renderApp();
  else renderAuth();
});
window.addEventListener('dk-unauthorized', () => {
  passwordGateActive = false;
  pendingWelcome = null;
  renderLogin();
});
// 后端的「改密闸门」：没改初始密码时，所有业务接口都会返回 403 +
// backend/app/deps.MUST_CHANGE_PASSWORD_DETAIL 那句话。api.js 识别出来后派发这个事件，
// 这里把人领到改密页——而不是弹一个「请先修改初始密码」却没地方可改的报错。
window.addEventListener('dk-must-change-password', () => {
  if (passwordGateActive) return; // 并发请求会一起撞上 403，只处理第一条
  const user = getUser();
  if (!getToken() || !user) return;
  if (!user.must_change_password) setSession(getToken(), { ...user, must_change_password: true });
  paintApp(getUser());
});

renderApp();
