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

const ROUTES = [
  { hash: '#/generate', title: '生成工作台', navLabel: '生成', icon: 'sparkles', render: renderGenerate },
  { hash: '#/history', title: '生成记录', navLabel: '记录', icon: 'images', render: renderHistory },
  { hash: '#/templates', title: '提示词模板', navLabel: '模板', icon: 'layout-template', render: renderTemplates },
  { hash: '#/inspiration', title: '灵感库', navLabel: '灵感', icon: 'lightbulb', render: renderInspiration },
  { hash: '#/apikeys', title: 'API 对接', navLabel: 'API', icon: 'plug', render: renderApiKeys, adminOnly: true },
  { hash: '#/settings', title: '系统设置', navLabel: '设置', icon: 'settings', render: renderSettings, adminOnly: true },
];

const root = document.getElementById('app');
let disposeView = null;

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
  clearGenerateSessions();
  document.title = '登录 · DesignKit';
  setThemeColor('#f3f5f7');
  const username = h('input', { class: 'dk-control input', autocomplete: 'username', value: 'admin' });
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
    h('p', { class: 'dk-auth-note' },
      icon('shield-check', { size: 15 }),
      h('span', {}, '首次使用：账号 admin，初始密码 admin123456；登录后会要求你立即设置新密码。')));

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

function renderApp() {
  cleanupView();
  const user = getUser();
  if (!getToken() || !user) {
    renderLogin();
    return;
  }
  setThemeColor('#f3f5f7');
  if (user.must_change_password) {
    renderPasswordGate(user);
    return;
  }
  const route = resolveRoute(ROUTES, user);
  if (window.location.hash !== route.hash) navigate(route.hash, { replace: true });
  document.title = `${route.title} · DesignKit`;
  const { main } = renderAppShell({ root, user, route, routes: ROUTES, onLogout: logout });
  const cleanup = route.render(main, user);
  disposeView = typeof cleanup === 'function' ? cleanup : null;
}

function renderPasswordGate(user) {
  const route = ROUTES[0];
  if (window.location.hash !== route.hash) navigate(route.hash, { replace: true });
  document.title = '设置安全密码 · DesignKit';
  const { main } = renderAppShell({ root, user, route, routes: ROUTES, onLogout: logout });
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
  cleanupView();
  clearSession();
  history.replaceState(null, '', window.location.pathname);
  renderLogin();
}

window.addEventListener('hashchange', () => { if (getToken()) renderApp(); });
window.addEventListener('dk-unauthorized', renderLogin);

renderApp();
