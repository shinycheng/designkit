// 应用外壳：登录门卫 + 侧边导航 + 哈希路由
import { api, clearSession, getToken, getUser, setSession } from './api.js';
import { field, h, modal, toast } from './ui.js';
import { renderGenerate } from './views/generate.js';
import { renderHistory } from './views/history.js';
import { renderTemplates } from './views/templates.js';
import { renderApiKeys } from './views/apikeys.js';
import { renderSettings } from './views/settings.js';

const ROUTES = [
  { hash: '#/generate', title: '生成工作台', icon: '✨', render: renderGenerate },
  { hash: '#/history', title: '生成记录', icon: '🖼', render: renderHistory },
  { hash: '#/templates', title: '提示词模板', icon: '📚', render: renderTemplates },
  { hash: '#/apikeys', title: 'API 对接', icon: '🔌', render: renderApiKeys, adminOnly: true },
  { hash: '#/settings', title: '系统设置', icon: '⚙️', render: renderSettings, adminOnly: true },
];

const root = document.getElementById('app');
let viewCleanup = null; // 当前视图的清理函数（停掉轮询定时器等）

function runCleanup() {
  if (viewCleanup) { try { viewCleanup(); } catch { /* 忽略 */ } viewCleanup = null; }
}

function currentRoute() {
  const user = getUser();
  const visible = ROUTES.filter(r => !r.adminOnly || (user && user.role === 'admin'));
  return visible.find(r => r.hash === location.hash) || visible[0];
}

function renderLogin() {
  runCleanup();
  root.innerHTML = '';
  const userInput = h('input', { class: 'input', placeholder: '用户名', value: 'admin' });
  const passInput = h('input', { class: 'input', type: 'password', placeholder: '密码' });
  const btn = h('button', { class: 'btn lg', onclick: doLogin }, '登 录');
  async function doLogin() {
    if (!userInput.value.trim() || !passInput.value) return toast('请输入用户名和密码', 'error');
    btn.disabled = true;
    try {
      const data = await api.post('/api/web/auth/login', { username: userInput.value.trim(), password: passInput.value });
      setSession(data.token, data.user);
      if (data.user.must_change_password) {
        forceChangePassword(data.user);
      } else {
        location.hash = '#/generate';
        renderApp();
      }
    } catch (e) {
      toast(e.message, 'error');
    } finally {
      btn.disabled = false;
    }
  }
  passInput.addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });

  root.append(
    h('div', { class: 'login-wrap' },
      h('div', { class: 'login-card' },
        h('div', { class: 'login-logo' },
          h('div', { class: 'brand-logo' }, '🎨'),
          h('div', { class: 'login-title' }, 'DesignKit')),
        h('div', { class: 'login-sub' }, 'AI 商品图生成平台 · 内部版'),
        h('div', { class: 'field' }, userInput),
        h('div', { class: 'field' }, passInput),
        btn,
        h('div', { class: 'login-tip' }, '首次使用：账号 admin，初始密码 admin123456，登录后请尽快到「系统设置」修改密码。'))));
}

function renderApp() {
  runCleanup();
  const user = getUser();
  if (!getToken() || !user) return renderLogin();
  const route = currentRoute();
  if (location.hash !== route.hash) { history.replaceState(null, '', route.hash); }

  root.innerHTML = '';
  const content = h('div', { class: 'content' });
  const visibleRoutes = ROUTES.filter(r => !r.adminOnly || user.role === 'admin');

  root.append(
    h('div', { class: 'layout' },
      h('aside', { class: 'sidebar' },
        h('div', { class: 'brand' },
          h('div', { class: 'brand-logo' }, '🎨'),
          h('div', {},
            h('div', { class: 'brand-name' }, 'DesignKit'),
            h('div', { class: 'brand-sub' }, 'AI 商品图平台'))),
        h('nav', { class: 'nav' },
          visibleRoutes.map(r =>
            h('button', {
              class: 'nav-item' + (r.hash === route.hash ? ' active' : ''),
              onclick: () => { location.hash = r.hash; },
            }, h('span', { class: 'nav-icon' }, r.icon), r.title))),
        h('div', { class: 'sidebar-foot' }, 'DesignKit v0.1 · 内部版')),
      h('div', { class: 'main' },
        h('header', { class: 'topbar' },
          h('div', { class: 'topbar-title' }, route.title),
          h('div', { class: 'topbar-user' },
            h('div', { class: 'avatar' }, (user.display_name || user.username || '?').slice(0, 1).toUpperCase()),
            h('span', {}, user.display_name || user.username),
            h('button', { class: 'btn ghost sm', onclick: logout }, '退出'))),
        content)));

  const cleanup = route.render(content, user);
  if (typeof cleanup === 'function') viewCleanup = cleanup;
}

function forceChangePassword(user) {
  const oldInput = h('input', { class: 'input', type: 'password', placeholder: '原密码（初始密码）' });
  const newInput = h('input', { class: 'input', type: 'password', placeholder: '新密码，至少 8 位' });
  const new2Input = h('input', { class: 'input', type: 'password', placeholder: '再次输入新密码' });
  const okBtn = h('button', { class: 'btn', onclick: submit }, '设置新密码并进入');
  async function submit() {
    if (!newInput.value || newInput.value.length < 8) return toast('新密码至少 8 位', 'error');
    if (newInput.value !== new2Input.value) return toast('两次输入的新密码不一致', 'error');
    okBtn.disabled = true;
    try {
      const r = await api.post('/api/web/auth/change_password', { old_password: oldInput.value, new_password: newInput.value });
      if (r.token) setSession(r.token, { ...user, must_change_password: false });
      m.close();
      toast('密码已设置', 'success');
      location.hash = '#/generate';
      renderApp();
    } catch (e) { toast(e.message, 'error'); okBtn.disabled = false; }
  }
  const m = modal({
    title: '首次登录：请设置新密码',
    body: h('div', {},
      h('p', { style: { marginBottom: '12px', color: 'var(--text-2)' } },
        '为了安全，请把默认密码改成只有你知道的密码。'),
      field('原密码', oldInput),
      field('新密码', newInput),
      field('确认新密码', new2Input)),
    footer: [okBtn],
    onClose: () => { /* 强制修改：关闭即退出登录 */ if (getUser() && getUser().must_change_password) logout(); },
  });
}

function logout() {
  clearSession();
  location.hash = '';
  renderLogin();
}

window.addEventListener('hashchange', () => { if (getToken()) renderApp(); });
window.addEventListener('dk-unauthorized', () => renderLogin());

renderApp();
