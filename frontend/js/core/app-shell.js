import { button, h, icon } from '../ui.js';
import { visibleRoutes } from './router.js';

function navLink(route, current, className) {
  const label = className.includes('bottom') ? route.title : (route.navLabel || route.title);
  return h('a', {
    class: `${className}${current ? ' is-current' : ''}`,
    href: route.hash,
    'aria-current': current ? 'page' : null,
    'aria-label': route.title,
    title: route.title,
  }, icon(route.icon, { size: className.includes('bottom') ? 20 : 22 }), h('span', {}, label));
}

export function renderAppShell({ root, user, route, routes, onLogout }) {
  const visible = visibleRoutes(routes, user);
  const main = h('main', {
    id: 'main-content',
    class: `dk-main${route.hash === '#/generate' ? ' dk-main--studio' : ''}`,
    tabindex: '-1',
    'data-route': route.hash.slice(2),
    'aria-labelledby': 'page-title',
  });

  const userName = user.display_name || user.username || '用户';
  const userMenu = h('details', { class: 'dk-user-menu' },
    h('summary', { class: 'dk-user-trigger', 'aria-label': `打开 ${userName} 的账户菜单` },
      h('span', { class: 'dk-avatar', 'aria-hidden': 'true' }, userName.slice(0, 1).toUpperCase()),
      h('span', { class: 'dk-user-name' }, userName),
      icon('chevron-down', { size: 16 })),
    h('div', { class: 'dk-user-popover' },
      h('div', { class: 'dk-user-meta' },
        h('strong', {}, userName),
        h('span', {}, user.role === 'admin' ? '管理员' : '成员')),
      h('a', { class: 'dk-user-action', href: '/docs', target: '_blank', rel: 'noreferrer' }, icon('book-open'), '接口文档'),
      button('退出登录', { variant: 'quiet', size: 'sm', iconName: 'log-out', className: 'dk-user-action', onclick: onLogout })),
  );

  root.replaceChildren(
    h('a', {
      class: 'dk-skip-link',
      href: '#main-content',
      onclick: (event) => {
        event.preventDefault();
        window.setTimeout(() => main.focus(), 0);
      },
    }, '跳到主要内容'),
    h('div', { class: 'dk-app dk-app-shell' },
      h('header', { class: 'dk-topbar' },
        h('a', { class: 'dk-wordmark', href: '#/generate', 'aria-label': 'DesignKit 首页' }, 'DesignKit'),
        h('div', { class: 'dk-topbar-context' },
          h('span', { class: 'dk-topbar__divider', 'aria-hidden': 'true' }),
          h('h1', { id: 'page-title', class: 'dk-page-title' }, route.title)),
        h('div', { class: 'dk-topbar-actions' },
          h('a', { class: 'dk-help-link', href: '/docs', target: '_blank', rel: 'noreferrer' }, icon('circle-help'), h('span', {}, '帮助')),
          userMenu)),
      h('div', { class: 'dk-app-body' },
        h('nav', { class: 'dk-tool-rail', 'aria-label': '主要导航' },
          visible.map((item) => navLink(item, item.hash === route.hash, 'dk-tool-link'))),
        main),
      h('nav', { class: 'dk-bottom-nav', 'aria-label': '移动端主要导航' },
        visible.map((item) => navLink(item, item.hash === route.hash, 'dk-bottom-link')))),
  );
  return { main };
}
