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

const SIMPLE_ICON_ROOT = 'https://cdn.jsdelivr.net/npm/simple-icons@16.21.0/icons';
const LORDICON_ELEMENT_URL = 'https://cdn.jsdelivr.net/npm/@lordicon/element@2.3.1/+esm';
const LORDICON_SHOPPING_URL = 'https://cdn.jsdelivr.net/gh/lordicondev/player-element@1d8ec10f991ba8c0bde0c9618e55092e22ee3fe0/examples/icons/morph-shopping.json';
const LORDICON_COINS_URL = 'https://cdn.jsdelivr.net/gh/lordicondev/player-element@1d8ec10f991ba8c0bde0c9618e55092e22ee3fe0/examples/icons/coins.json';
const TAG_CLOUD_URL = 'https://cdn.jsdelivr.net/npm/TagCloud@2.5.0/+esm';
let lordiconElementModule = null;
let tagCloudModule = null;

const COMMERCE_CHANNELS = [
  { key: 'taobao', label: '淘宝', market: '中国', src: '/assets/platforms/taobao.svg' },
  { key: 'tmall', label: '天猫', market: '中国', src: '/assets/platforms/tmall.svg', wide: true },
  { key: 'jd', label: '京东', market: '中国', src: 'https://en.wikipedia.org/wiki/Special:Redirect/file/JD.com_logo.png', wide: true },
  { key: 'pinduoduo', label: '拼多多', market: '中国', src: 'https://commons.wikimedia.org/wiki/Special:Redirect/file/Pinduoduologo.png', wide: true },
  { key: 'douyin', label: '抖音电商', market: '中国', src: '/assets/platforms/tiktok.svg' },
  { key: 'xiaohongshu', label: '小红书', market: '中国', src: '/assets/platforms/xiaohongshu.svg' },
  { key: 'amazon', label: 'Amazon', market: '全球', src: 'https://commons.wikimedia.org/wiki/Special:Redirect/file/Amazon_2024.svg', wide: true },
  { key: 'ebay', label: 'eBay', market: '全球', src: `${SIMPLE_ICON_ROOT}/ebay.svg`, wide: true },
  { key: 'shopify', label: 'Shopify', market: '全球', src: `${SIMPLE_ICON_ROOT}/shopify.svg` },
  { key: 'etsy', label: 'Etsy', market: '欧美', src: `${SIMPLE_ICON_ROOT}/etsy.svg` },
  { key: 'aliexpress', label: 'AliExpress', market: '全球', src: `${SIMPLE_ICON_ROOT}/aliexpress.svg`, wide: true },
  { key: 'shopee', label: 'Shopee', market: '东南亚', src: `${SIMPLE_ICON_ROOT}/shopee.svg` },
  { key: 'rakuten', label: 'Rakuten', market: '日本', src: `${SIMPLE_ICON_ROOT}/rakuten.svg`, wide: true },
  { key: 'walmart', label: 'Walmart', market: '北美', src: 'https://commons.wikimedia.org/wiki/Special:Redirect/file/Walmart_logo_(2025).svg', wide: true },
  { key: 'tiktok-shop', label: 'TikTok Shop', market: '全球', src: `${SIMPLE_ICON_ROOT}/tiktok.svg` },
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

function commerceChannelToken({ key, label, market, src, wide = false }) {
  return h('span', {
    class: `dk-auth-logo-token${wide ? ' dk-auth-logo-token--wide' : ''} dk-auth-logo-token--${key}`,
    title: `${label} · ${market}`,
  },
  h('img', {
    class: `dk-auth-logo-token__image${wide ? ' dk-auth-logo-token__image--wide' : ''}`,
    src,
    alt: '',
    width: '92',
    height: '34',
    decoding: 'async',
    draggable: 'false',
    referrerpolicy: 'no-referrer',
  }));
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
  // 不预填用户名：多人使用之后，这里预填 admin 会让每个成员都要先把它删掉再打自己的名字。
  const username = h('input', { class: 'dk-control input', autocomplete: 'username' });
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

  const form = h('form', { class: 'dk-auth-card', novalidate: true, onsubmit: login },
    h('div', { class: 'dk-auth-heading' },
      h('h1', { class: 'dk-auth-title' }, '欢迎回来'),
      h('p', { class: 'dk-auth-subtitle' }, '登录后管理商品素材、生成任务与渠道模板。')),
    errorRegion,
    field('用户名', username, { required: true }),
    field('密码', password.wrap, { required: true }),
    submit,
    // 这句话是给「刚把系统装起来、还不知道怎么进去」的人看的。
    // 完全删掉会让首次部署的人找不到入口；但把默认口令印在登录页上，
    // 多人使用之后就不合适了——所以只说管理员那一个账号，成员一律由管理员建号。
    h('p', { class: 'dk-auth-note' },
      icon('shield-check', { size: 15 }),
      h('span', {}, '首次部署：管理员账号 admin / 初始密码 admin123456，登录后立即修改；'
        + '成员账号请由管理员在「成员账号」页创建。')));

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
      h('div', { class: 'dk-auth-form-body' }, form)));
  root.replaceChildren(page);
  disposeView = mountLoginStage(page);
  queueMicrotask(() => { if (username.isConnected) username.focus(); });
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
    renderLogin();
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

window.addEventListener('hashchange', () => { if (getToken()) renderApp(); });
window.addEventListener('dk-unauthorized', () => {
  passwordGateActive = false;
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
