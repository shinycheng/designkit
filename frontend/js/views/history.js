// 生成记录：可筛选的视觉画廊 + 局部轮询 + 响应式任务详情抽屉
import { api } from '../api.js';
import { navigate } from '../core/router.js';
import {
  button,
  confirmDialog,
  copyText,
  downloadUrl,
  drawer,
  emptyState,
  fmtTime,
  h,
  icon,
  inlineAlert,
  lightbox,
  skeleton,
  statusBadge,
  toast,
} from '../ui.js';

const ACTIVE_STATUSES = new Set(['pending', 'processing']);

export function renderHistory(container) {
  const state = {
    page: 1,
    pageSize: 12,
    status: '',
    q: '',
    data: null,
    stopped: false,
    actionPending: false,
    loading: false,
    loadSequence: 0,
    pollTimer: null,
    pollSequence: 0,
    cards: new Map(),
    detailJobId: null,
    detailRequestSequence: 0,
    detailBody: null,
    detailOverlay: null,
  };

  const page = h('section', { class: 'dk-page dk-page--history', 'aria-labelledby': 'history-page-title' });
  const toolbar = h('div', { class: 'dk-page-toolbar dk-history-toolbar' });
  const resultCount = h('span', { class: 'dk-result-count', 'aria-live': 'polite' }, '正在读取记录…');
  const content = h('div', { class: 'dk-section', 'aria-busy': 'true' }, skeleton({ lines: 6, className: 'dk-loading' }));

  const statusSelect = h('select', {
    class: 'select dk-history-status-filter',
    'aria-label': '按任务状态筛选',
    onchange: (event) => {
      state.status = event.target.value;
      state.page = 1;
      load();
    },
  },
  h('option', { value: '' }, '全部状态'),
  h('option', { value: 'pending' }, '排队中'),
  h('option', { value: 'processing' }, '生成中'),
  h('option', { value: 'succeeded' }, '已完成'),
  h('option', { value: 'failed' }, '失败'));

  const searchInput = h('input', {
    class: 'input dk-history-search-input',
    type: 'search',
    placeholder: '搜索提示词或模板名',
    'aria-label': '搜索生成记录',
    onkeydown: (event) => {
      if (event.key === 'Enter') applySearch();
    },
  });

  function applySearch() {
    const next = searchInput.value.trim();
    if (state.q === next && state.data) return;
    state.q = next;
    state.page = 1;
    load();
  }

  const toolbarMeta = h('div', { class: 'dk-history-toolbar__meta' },
    resultCount,
    button('刷新', { variant: 'quiet', iconName: 'refresh-cw', onclick: () => load({ announce: true }) }));

  toolbar.append(
    h('div', { class: 'dk-toolbar-group dk-history-toolbar__filters' }, statusSelect, searchInput,
      button('搜索', { variant: 'secondary', iconName: 'search', className: 'dk-history-search-button', onclick: applySearch })),
    h('div', { class: 'dk-toolbar-spacer' }),
    toolbarMeta,
  );

  page.append(
    h('h1', { id: 'history-page-title', class: 'dk-visually-hidden' }, '生成记录'),
    toolbar,
    content,
  );
  container.append(page);

  function makeParams() {
    const params = new URLSearchParams({ page: state.page, page_size: state.pageSize });
    if (state.status) params.set('status', state.status);
    if (state.q) params.set('q', state.q);
    return params;
  }

  async function load({ announce = false } = {}) {
    if (state.stopped) return;
    state.loading = true;
    const sequence = ++state.loadSequence;
    stopPolling();
    content.setAttribute('aria-busy', 'true');
    if (!state.data) content.replaceChildren(skeleton({ lines: 6, className: 'dk-loading' }));
    try {
      const data = await api.get('/api/web/generations?' + makeParams());
      if (state.stopped || sequence !== state.loadSequence) return;
      state.data = data;
      if (!data.items.length && data.total > 0 && state.page > 1) {
        state.page = Math.max(1, Math.ceil(data.total / state.pageSize));
        return load({ announce });
      }
      renderGallery();
      if (announce) toast('生成记录已更新', 'success');
      schedulePolling();
    } catch (error) {
      if (state.stopped || sequence !== state.loadSequence) return;
      content.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载生成记录' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: () => load() }),
      );
      resultCount.textContent = '加载失败';
    } finally {
      if (sequence === state.loadSequence) {
        state.loading = false;
        content.setAttribute('aria-busy', 'false');
      }
    }
  }

  function renderGallery() {
    content.replaceChildren();
    state.cards.clear();
    const { items, total } = state.data;
    resultCount.textContent = `共 ${total} 条记录`;

    if (!items.length) {
      const filtered = Boolean(state.q || state.status);
      const action = filtered
        ? button('清除筛选', {
          variant: 'secondary',
          onclick: () => {
            state.q = '';
            state.status = '';
            searchInput.value = '';
            statusSelect.value = '';
            state.page = 1;
            load();
          },
        })
        : button('去生成工作台', { iconName: 'sparkles', onclick: () => { location.hash = '#/generate'; } });
      content.append(emptyState(
        filtered ? 'search' : 'images',
        filtered ? '当前筛选下没有记录' : '还没有生成记录',
        {
          description: filtered ? '试试其他关键词或状态。' : '完成第一次商品图生成后，结果会保存在这里。',
          action,
        },
      ));
      return;
    }

    const grid = h('div', { class: 'dk-card-grid dk-history-grid' });
    items.forEach((job) => {
      const card = renderHistoryCard(job);
      state.cards.set(job.job_id, card);
      grid.append(card);
    });
    content.append(grid, renderPagination(total));
  }

  function renderHistoryCard(job) {
    const media = h('div', { class: 'dk-history-card__media' }, renderCover(job));
    const card = h('article', {
      class: 'dk-history-card',
      'data-job-id': job.job_id,
      'data-state': job.status,
    },
    h('button', {
      class: 'dk-history-card__open',
      type: 'button',
      'aria-label': `查看 ${job.template_name || '自由提示词'} 任务详情`,
      onclick: () => openDetail(job.job_id),
    }, media,
    h('div', { class: 'dk-history-card__body' },
      h('div', { class: 'dk-history-card__title' }, job.template_name || '自由提示词'),
      h('div', { class: 'dk-history-card__meta' },
        statusBadge(job.status),
        ACTIVE_STATUSES.has(job.status)
          ? h('span', { class: 'dk-live-indicator' }, '自动更新中')
          : null,
        h('time', { datetime: job.created_at || '' }, fmtTime(job.created_at))),
      job.source === 'api' ? h('div', { class: 'dk-source-badge' }, '来自 ERP / API') : null)));
    return card;
  }

  function renderCover(job) {
    const output = job.images && job.images[0];
    if (output) {
      return h('img', {
        src: output.thumbnail_url || output.url,
        alt: `${job.template_name || '商品图'}生成结果`,
        loading: 'lazy',
        width: output.width || 640,
        height: output.height || 640,
      });
    }
    const input = job.input_images && job.input_images[0];
    if (input) {
      return h('div', { class: 'dk-history-placeholder dk-history-placeholder--input' },
        h('img', { src: input, alt: '生成任务的商品原图', loading: 'lazy' }),
        h('span', {}, job.status === 'failed' ? '生成失败' : '正在生成'));
    }
    return h('div', { class: 'dk-history-placeholder' },
      icon(job.status === 'failed' ? 'circle-alert' : 'image', { size: 28 }),
      h('span', {}, job.status === 'failed' ? '生成失败' : '暂无预览'));
  }

  function renderPagination(total) {
    const pages = Math.max(1, Math.ceil(total / state.pageSize));
    return h('nav', { class: 'dk-pagination', 'aria-label': '生成记录分页' },
      button('上一页', {
        variant: 'quiet',
        size: 'sm',
        disabled: state.page <= 1,
        onclick: () => { state.page -= 1; load(); },
      }),
      h('span', { 'aria-live': 'polite' }, `第 ${state.page} / ${pages} 页`),
      button('下一页', {
        variant: 'quiet',
        size: 'sm',
        disabled: state.page >= pages,
        onclick: () => { state.page += 1; load(); },
      }));
  }

  function stopPolling() {
    clearTimeout(state.pollTimer);
    state.pollTimer = null;
    state.pollSequence += 1;
  }

  function schedulePolling(delay = 3200) {
    if (state.stopped || !state.data || !state.data.items.some((job) => ACTIVE_STATUSES.has(job.status))) return;
    const sequence = state.pollSequence;
    clearTimeout(state.pollTimer);
    state.pollTimer = setTimeout(() => pollActive(sequence), delay);
  }

  async function pollActive(sequence) {
    if (state.stopped || sequence !== state.pollSequence || document.hidden) {
      if (!state.stopped && sequence === state.pollSequence) schedulePolling(6000);
      return;
    }
    const active = state.data.items.filter((job) => ACTIVE_STATUSES.has(job.status));
    try {
      const settled = await Promise.allSettled(active.map((job) => api.get('/api/web/generations/' + job.job_id)));
      if (state.stopped || sequence !== state.pollSequence) return;
      let needsReload = false;
      settled.forEach((result, index) => {
        if (result.status !== 'fulfilled') {
          // 任务被删除（可能是别的标签页或 ERP 侧删的）：重拉列表让它消失，
          // 否则卡片会永远停在「生成中」并被无限轮询下去
          if (result.reason?.status === 404) needsReload = true;
          return;
        }
        const fresh = result.value;
        const old = active[index];
        if (state.status && fresh.status !== state.status) {
          needsReload = true;
          return;
        }
        const itemIndex = state.data.items.findIndex((item) => item.job_id === old.job_id);
        if (itemIndex >= 0) state.data.items[itemIndex] = fresh;
        replaceCard(fresh);
        if (state.detailJobId === fresh.job_id) updateDetail(fresh);
      });
      if (needsReload) void load();
    } finally {
      if (!state.stopped && sequence === state.pollSequence) schedulePolling();
    }
  }

  function replaceCard(job) {
    const current = state.cards.get(job.job_id);
    if (!current || !current.isConnected) return;
    const replacement = renderHistoryCard(job);
    current.replaceWith(replacement);
    state.cards.set(job.job_id, replacement);
  }

  async function openDetail(jobId) {
    const requestSequence = ++state.detailRequestSequence;
    let job;
    try {
      job = await api.get('/api/web/generations/' + jobId);
    } catch (error) {
      if (state.stopped || requestSequence !== state.detailRequestSequence) return;
      toast(error.message, 'error');
      return;
    }
    if (state.stopped || requestSequence !== state.detailRequestSequence) return;
    if (state.detailOverlay) state.detailOverlay.close();
    state.detailJobId = jobId;
    state.detailBody = h('div', { class: 'dk-job-detail' });
    updateDetail(job);
    state.detailOverlay = drawer({
      title: job.template_name || '自由提示词',
      body: state.detailBody,
      wide: true,
      onClose: () => {
        state.detailJobId = null;
        state.detailBody = null;
        state.detailOverlay = null;
      },
    });
  }

  function updateDetail(job) {
    if (!state.detailBody) return;
    const params = job.params || {};
    const summary = h('dl', { class: 'dk-job-facts' },
      h('div', {}, h('dt', {}, '状态'), h('dd', {}, statusBadge(job.status))),
      h('div', {}, h('dt', {}, '创建时间'), h('dd', {}, h('time', { datetime: job.created_at || '' }, fmtTime(job.created_at)))),
      h('div', {}, h('dt', {}, '输出规格'), h('dd', {}, `${params.size || '默认尺寸'} · ${params.n || 1} 张`)),
      h('div', {}, h('dt', {}, '来源'), h('dd', {}, job.source === 'api' ? 'ERP / API' : '生成工作台')));

    const imageGrid = job.images && job.images.length
      ? h('div', { class: 'dk-job-media-grid' }, job.images.map((image, index) =>
        h('figure', { class: 'dk-job-media' },
          h('button', {
            type: 'button',
            class: 'dk-job-media__preview',
            'aria-label': `预览第 ${index + 1} 张结果`,
            onclick: () => lightbox(image.url),
          }, h('img', {
            src: image.thumbnail_url || image.url,
            alt: `${job.template_name || '商品图'}结果 ${index + 1}`,
            loading: 'lazy',
            width: image.width || 640,
            height: image.height || 640,
          })),
          button('下载', {
            variant: 'secondary',
            size: 'sm',
            iconName: 'download',
            onclick: () => downloadUrl(
              image.url,
              `designkit_${job.job_id.slice(0, 8)}_${index + 1}.${image.format || 'png'}`,
            ),
          }),
          // 翻出旧图接着改是常见动作；以前只能下载到本地再拖回上传区
          button('继续改', {
            variant: 'quiet',
            size: 'sm',
            iconName: 'wand-sparkles',
            onclick: () => {
              try {
                sessionStorage.setItem('dk_reuse_image', JSON.stringify({
                  url: image.url,
                  name: `designkit_${job.job_id.slice(0, 8)}_${index + 1}.${image.format || 'png'}`,
                }));
              } catch {
                toast('当前浏览器不允许跨页传递，请在生成页手动上传', 'error');
                return;
              }
              navigate('#/generate');
            },
          }))))
      : null;

    const inputs = job.input_images && job.input_images.length
      ? h('div', { class: 'dk-job-media-grid dk-job-media-grid--inputs' }, job.input_images.map((url, index) =>
        h('button', {
          type: 'button',
          class: 'dk-job-media dk-job-media__preview',
          'aria-label': `预览第 ${index + 1} 张商品原图`,
          onclick: () => lightbox(url),
        }, h('img', { src: url, alt: `商品原图 ${index + 1}`, loading: 'lazy' }))))
      : null;

    const pending = Boolean(state.actionPending);
    const actions = h('div', { class: 'dk-inline-actions' },
      (job.status === 'succeeded' || job.status === 'failed')
        ? button(pending ? '正在提交…' : '重新生成', {
          variant: 'secondary',
          iconName: 'rotate-ccw',
          loading: pending,
          disabled: pending,
          onclick: () => retryJob(job),
        })
        : null,
      job.status !== 'processing'
        ? button('删除记录', {
          variant: 'danger',
          iconName: 'trash-2',
          disabled: pending,
          onclick: () => deleteJob(job),
        })
        : null);

    state.detailBody.replaceChildren(...[
      summary,
      job.error ? inlineAlert(job.error, 'error', { title: '生成失败' }) : null,
      ACTIVE_STATUSES.has(job.status)
        ? inlineAlert('任务仍在后台处理，可以离开此页，结果会自动更新。', 'info')
        : null,
      imageGrid ? h('section', { class: 'dk-panel-stack' }, h('h3', {}, '生成结果'), imageGrid) : null,
      inputs ? h('section', { class: 'dk-panel-stack' }, h('h3', {}, '商品原图'), inputs) : null,
      // AI 现场重写过时，实际发出的与配置产生的不同，两个都要能看到
      (job.prompt_sent && job.prompt_sent !== job.prompt)
        ? h('section', { class: 'dk-panel-stack' },
          h('div', { class: 'dk-section-head' },
            h('h3', {}, 'AI 实际发出的提示词'),
            button('复制', {
              variant: 'quiet', size: 'sm', iconName: 'copy',
              onclick: () => copyText(job.prompt_sent || ''),
            })),
          h('p', { class: 'dk-section-meta' }, 'AI 看过你的商品图后，结合模板风格与补充要求重写的版本'),
          h('pre', { class: 'code-block' }, job.prompt_sent))
        : null,
      h('section', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-section-head' },
          h('h3', {}, (job.prompt_sent && job.prompt_sent !== job.prompt) ? '你的配置（模板 + 补充要求）' : '最终提示词'),
          button('复制', { variant: 'quiet', size: 'sm', iconName: 'copy', onclick: () => copyText(job.prompt || '') })),
        h('pre', { class: 'code-block' }, job.prompt || '无')),
      actions,
    ].filter(Boolean));
  }

  async function retryJob(job) {
    // 防连点：重试会真实调用生图接口，连击等于重复付费
    if (state.actionPending) return;
    state.actionPending = true;
    updateDetail(job);
    try {
      const fresh = await api.post('/api/web/generations/' + job.job_id + '/retry');
      toast('任务已重新排队', 'success');
      const index = state.data.items.findIndex((item) => item.job_id === job.job_id);
      if (index >= 0) state.data.items[index] = fresh;
      replaceCard(fresh);
      state.actionPending = false;
      updateDetail(fresh);
      schedulePolling(800);
    } catch (error) {
      toast(error.message, 'error');
      state.actionPending = false;
      updateDetail(job);
    }
  }

  async function deleteJob(job) {
    if (state.actionPending) return;
    const confirmed = await confirmDialog(
      `删除任务「${job.template_name || '自由提示词'}」？生成的图片文件也会一并删除，此操作无法撤销。`,
      { danger: true, okText: '删除任务和图片' },
    );
    if (!confirmed || state.actionPending) return;
    state.actionPending = true;
    updateDetail(job);
    try {
      await api.del('/api/web/generations/' + job.job_id);
      if (state.detailOverlay) state.detailOverlay.close();
      toast('生成记录已删除', 'success');
      state.actionPending = false;
      await load();
    } catch (error) {
      toast(error.message, 'error');
      state.actionPending = false;
      updateDetail(job);
    }
  }

  function handleVisibility() {
    if (document.hidden) {
      clearTimeout(state.pollTimer);
      state.pollTimer = null;
    } else {
      schedulePolling(600);
    }
  }

  document.addEventListener('visibilitychange', handleVisibility);
  load();

  return () => {
    state.stopped = true;
    state.loadSequence += 1;
    state.detailRequestSequence += 1;
    stopPolling();
    document.removeEventListener('visibilitychange', handleVisibility);
    if (state.detailOverlay) state.detailOverlay.close();
  };
}
