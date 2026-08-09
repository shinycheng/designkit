// 灵感库：浏览/搜索 YouMind 开源提示词库（1.4 万条），看中的采用为自己的模板或直接拿去生成
import { api } from '../api.js';
import {
  button,
  drawer,
  emptyState,
  fmtTime,
  h,
  icon,
  inlineAlert,
  skeleton,
  toast,
} from '../ui.js';

const PAGE_SIZE = 24;

export function renderInspiration(container, user) {
  const isAdmin = user.role === 'admin';
  const state = {
    q: '',
    categoryId: null,
    page: 1,
    data: null,
    loadSequence: 0,
    stopped: false,
    adoptPending: false,
    syncTimer: null,
    detailOverlay: null,
  };

  const page = h('section', { class: 'dk-page dk-page--inspiration', 'aria-labelledby': 'insp-title' });
  const toolbar = h('div', { class: 'dk-section dk-insp-toolbar' });
  const catBar = h('div', { class: 'dk-insp-cats chips', role: 'group', 'aria-label': '按分类筛选' });
  const grid = h('div', { class: 'dk-insp-grid', 'aria-busy': 'true' }, skeleton({ lines: 8, className: 'dk-loading' }));
  const pager = h('div', { class: 'dk-insp-pager' });
  const syncBar = h('div', { class: 'dk-insp-sync', 'aria-live': 'polite' });
  page.append(
    h('h1', { id: 'insp-title', class: 'dk-visually-hidden' }, '灵感库'),
    toolbar, syncBar, catBar, grid, pager,
  );
  container.append(page);

  // ------------------------------------------------------------ 工具栏

  const searchInput = h('input', {
    class: 'input', type: 'search', placeholder: '搜索提示词（英文关键词命中率更高）…',
    'aria-label': '搜索灵感库',
    onkeydown: (event) => { if (event.key === 'Enter') applySearch(); },
  });
  const searchBtn = button('搜索', { variant: 'secondary', iconName: 'search', onclick: applySearch });
  const syncBtn = isAdmin
    ? button('同步上游', { variant: 'quiet', iconName: 'refresh-cw', onclick: startSync })
    : null;
  toolbar.append(
    h('div', { class: 'dk-section-head' },
      h('div', {},
        h('h2', { class: 'dk-section-title' }, '灵感库'),
        h('p', { class: 'dk-section-meta' },
          '来自 YouMind 开源提示词库（CC BY 4.0，每日更新）。看中哪条可以直接拿去生成，或采用为自己的模板。')),
      h('div', { class: 'dk-inline-actions' }, searchBtn, syncBtn)),
    h('div', { class: 'dk-insp-search' }, searchInput),
  );

  function applySearch() {
    state.q = searchInput.value.trim();
    state.page = 1;
    void load();
  }

  // ------------------------------------------------------------ 数据加载

  async function load() {
    if (state.stopped) return;
    // 序列号模式：新请求直接发出并作废旧响应，而不是把用户的新操作丢掉
    const sequence = ++state.loadSequence;
    grid.setAttribute('aria-busy', 'true');
    try {
      const params = new URLSearchParams({ page: String(state.page), page_size: String(PAGE_SIZE) });
      if (state.q) params.set('q', state.q);
      if (state.categoryId != null) params.set('category_id', String(state.categoryId));
      const data = await api.get('/api/web/inspiration?' + params);
      if (state.stopped || sequence !== state.loadSequence) return;
      state.data = data;
      // 页码越界（例如筛选后条目变少）：自动回退到最后一页，而不是留在空页
      const pages = Math.max(1, Math.ceil(data.total / PAGE_SIZE));
      if (!data.items.length && data.total > 0 && state.page > pages) {
        state.page = pages;
        void load();
        return;
      }
      renderCats();
      renderGrid();
      renderSyncBar(data.sync);
      // 进入页面时上游同步还在跑：接上轮询，结束后自动刷新列表
      if (data.sync?.state === 'running') watchSync();
    } catch (error) {
      if (state.stopped || sequence !== state.loadSequence) return;
      grid.replaceChildren(
        inlineAlert(error.message, 'error', { title: '灵感库加载失败' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: () => void load() }),
      );
    } finally {
      if (sequence === state.loadSequence) grid.setAttribute('aria-busy', 'false');
    }
  }

  function scrollToTop() {
    // 页面滚动发生在主内容容器上，不是 window
    const scroller = document.querySelector('.dk-main') || page.closest('main') || window;
    if (scroller.scrollTo) scroller.scrollTo({ top: 0 });
    page.scrollIntoView({ block: 'start' });
  }

  function renderCats() {
    const cats = state.data?.categories || [];
    catBar.replaceChildren(
      chip('全部', null, state.categoryId === null),
      ...cats.map((cat) => chip(`${cat.name} ${cat.count}`, cat.id, state.categoryId === cat.id)),
    );
  }

  function chip(label, id, active) {
    return h('button', {
      type: 'button',
      class: 'chip' + (active ? ' active' : ''),
      'aria-pressed': String(active),
      onclick: () => { state.categoryId = id; state.page = 1; void load(); },
    }, label);
  }

  function renderGrid() {
    const { items, total } = state.data;
    if (!items.length) {
      const hasLibrary = total > 0 || state.q || state.categoryId != null;
      grid.replaceChildren(emptyState('lightbulb',
        hasLibrary ? '没有匹配的提示词' : '灵感库还是空的',
        {
          description: hasLibrary
            ? '换个关键词试试；库里 98% 是英文提示词，用英文搜命中率更高。'
            : (isAdmin ? '点右上角「同步上游」拉取 1.4 万条开源提示词（约 1-2 分钟）。' : '请管理员先执行一次同步。'),
        }));
      pager.replaceChildren();
      return;
    }
    grid.replaceChildren(...items.map((item) => card(item)));

    const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    const jumpInput = h('input', {
      class: 'input dk-insp-jump', type: 'number', min: 1, max: pages,
      value: String(state.page), 'aria-label': '跳转到页码', inputmode: 'numeric',
      onkeydown: (event) => { if (event.key === 'Enter') doJump(); },
    });
    const doJump = () => {
      const target = Math.max(1, Math.min(pages, Number.parseInt(jumpInput.value, 10) || 1));
      if (target === state.page) return;
      state.page = target;
      void load();
      scrollToTop();
    };
    pager.replaceChildren(
      button('上一页', {
        variant: 'secondary', size: 'sm', disabled: state.page <= 1,
        onclick: () => { state.page -= 1; void load(); scrollToTop(); },
      }),
      h('span', { class: 'dk-section-meta' }, `第 ${state.page} / ${pages} 页 · 共 ${total} 条`),
      button('下一页', {
        variant: 'secondary', size: 'sm', disabled: state.page >= pages,
        onclick: () => { state.page += 1; void load(); scrollToTop(); },
      }),
      h('span', { class: 'dk-insp-jump-wrap' }, '跳到', jumpInput, '页',
        button('跳转', { variant: 'quiet', size: 'sm', onclick: doJump })),
    );
  }

  function card(item) {
    const cover = item.thumbnail_url
      ? h('img', {
        src: item.thumbnail_url, alt: '', loading: 'lazy',
        onerror: (event) => { event.target.replaceWith(icon('lightbulb', { size: 28 })); },
      })
      : icon('lightbulb', { size: 28 });
    return h('article', { class: 'dk-insp-card', 'data-adopted': item.adopted ? 'true' : 'false' },
      h('button', {
        type: 'button', class: 'dk-insp-card__open',
        'aria-label': `查看提示词：${item.name}`,
        onclick: () => openDetail(item),
      },
        h('div', { class: 'dk-insp-card__media' }, cover),
        h('div', { class: 'dk-insp-card__body' },
          h('div', { class: 'dk-insp-card__title' }, item.name),
          h('div', { class: 'dk-insp-card__meta' },
            item.category_name ? h('span', { class: 'badge gray' }, item.category_name) : null,
            item.requires_input_image ? h('span', { class: 'badge blue' }, '需商品图') : null,
            item.adopted ? h('span', { class: 'badge green' }, '已采用') : null))));
  }

  // ------------------------------------------------------------ 详情抽屉

  function openDetail(item) {
    const variables = item.variables || [];
    const body = h('div', { class: 'dk-panel-stack' },
      h('div', { class: 'dk-inline-actions' },
        item.category_name ? h('span', { class: 'badge gray' }, item.category_name) : null,
        item.requires_input_image
          ? h('span', { class: 'badge blue' }, '需上传商品/参考图')
          : h('span', { class: 'badge gray' }, '纯文字生成'),
        item.adopted ? h('span', { class: 'badge green' }, '已采用为模板') : null),
      item.thumbnail_url
        ? h('img', {
          class: 'dk-insp-detail__preview', src: item.thumbnail_url,
          alt: '官方示例效果图', loading: 'lazy',
          onerror: (event) => { event.target.remove(); },
        })
        : null,
      variables.length
        ? h('section', {},
          h('h3', {}, '可填空的变量'),
          h('ul', { class: 'dk-insp-vars' }, variables.map((v) =>
            h('li', {}, h('strong', {}, v.label || v.name),
              v.default ? `（默认：${v.default}）` : ''))))
        : null,
      h('section', {},
        h('h3', {}, '提示词全文'),
        h('pre', { class: 'code-block', style: { whiteSpace: 'pre-wrap' } }, item.prompt_template)),
      item.source_url
        ? h('p', { class: 'dk-section-meta' },
          '来源：', h('a', { href: item.source_url, target: '_blank', rel: 'noopener noreferrer' },
            'YouMind 提示词库（CC BY 4.0）', icon('external-link', { size: 13 })))
        : null,
    );

    const useBtn = button('用它生成', {
      iconName: 'sparkles',
      onclick: () => { useForGeneration(item); },
    });
    const adoptBtn = isAdmin
      ? button(item.adopted ? '已采用' : '采用为模板', {
        variant: 'secondary', iconName: item.adopted ? 'circle-check' : 'circle-plus',
        disabled: item.adopted || state.adoptPending,
        onclick: () => void adopt(item, adoptBtn),
      })
      : null;

    state.detailOverlay = drawer({
      title: item.name,
      body,
      footer: [adoptBtn, useBtn].filter(Boolean),
      onClose: () => { state.detailOverlay = null; },
    });
  }

  async function adopt(item, adoptBtn) {
    if (state.adoptPending || state.stopped) return;
    state.adoptPending = true;
    if (adoptBtn) {  // 请求期间给出可见反馈，防止误以为没点上
      adoptBtn.disabled = true;
      adoptBtn.setAttribute('aria-busy', 'true');
      adoptBtn.replaceChildren(icon('loader-circle', { className: 'dk-spin' }),
        h('span', { class: 'dk-button-label' }, '采用中…'));
    }
    try {
      const result = await api.post(`/api/web/inspiration/${item.id}/adopt`);
      if (state.stopped) return;
      item.adopted = true;
      toast(result.already ? '之前已经采用过这条了' : '已采用为你的模板，可在「提示词模板」里编辑', 'success');
      state.detailOverlay?.close();
      void load();
    } catch (error) {
      if (state.stopped) return;
      toast(error.message, 'error');
      if (adoptBtn?.isConnected) {
        adoptBtn.disabled = false;
        adoptBtn.removeAttribute('aria-busy');
        adoptBtn.replaceChildren(icon('circle-plus'), h('span', { class: 'dk-button-label' }, '采用为模板'));
      }
    } finally {
      state.adoptPending = false;
    }
  }

  function useForGeneration(item) {
    // 变量先用默认值填充，交给生成页的自由模式，用户可继续改
    let prompt = item.prompt_template || '';
    for (const v of item.variables || []) {
      prompt = prompt.split(`{${v.name}}`).join(v.default || `[${v.label || v.name}]`);
    }
    try {
      sessionStorage.setItem('dk_apply_prompt', JSON.stringify({
        prompt,
        requiresImage: Boolean(item.requires_input_image),
        from: item.name,
      }));
    } catch { /* 隐私模式下 sessionStorage 可能不可用，直接跳过预填 */ }
    state.detailOverlay?.close();
    location.hash = '#/generate';
  }

  // ------------------------------------------------------------ 同步

  function renderSyncBar(sync) {
    if (!sync) { syncBar.replaceChildren(); return; }
    if (sync.state === 'running') {
      syncBar.replaceChildren(inlineAlert(sync.message || '正在同步…', 'info', { title: '正在同步上游' }));
    } else if (sync.state === 'failed') {
      syncBar.replaceChildren(inlineAlert(sync.message || '同步失败', 'error', { title: '上次同步失败' }));
    } else if (sync.state === 'success' && sync.finished_at) {
      syncBar.replaceChildren(h('p', { class: 'dk-section-meta' },
        icon('circle-check', { size: 14 }), ' ',
        `上次同步 ${fmtTime(sync.finished_at)} · ${sync.message || ''}`));
    } else {
      syncBar.replaceChildren();
    }
  }

  async function startSync() {
    try {
      await api.post('/api/web/inspiration/sync');
      if (state.stopped) return;
      toast('同步已开始，约 1-2 分钟（可离开此页）', 'success');
      watchSync();
    } catch (error) {
      if (state.stopped) return;
      toast(error.message, 'error');
      if (error.status === 409) watchSync();
    }
  }

  function watchSync() {
    clearTimeout(state.syncTimer);
    state.syncTimer = setTimeout(async () => {
      if (state.stopped) return;
      try {
        const sync = await api.get('/api/web/inspiration/sync_status');
        if (state.stopped) return;
        renderSyncBar(sync);
        if (sync.state === 'running') watchSync();
        else if (sync.state === 'success') { toast('灵感库同步完成', 'success'); void load(); }
        else if (sync.state === 'failed') toast(sync.message || '同步失败', 'error');
      } catch { if (!state.stopped) watchSync(); }
    }, 2500);
  }

  void load();

  return () => {
    state.stopped = true;
    clearTimeout(state.syncTimer);
  };
}
