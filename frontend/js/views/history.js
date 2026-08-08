// 生成记录：分页画廊 + 详情弹窗（重试 / 删除 / 下载）
import { api } from '../api.js';
import { confirmDialog, copyText, downloadUrl, emptyState, fmtTime, h, lightbox, modal, statusBadge, toast } from '../ui.js';

export function renderHistory(container) {
  const state = { page: 1, pageSize: 12, status: '', q: '', data: null, timer: null };

  const toolbar = h('div', { class: 'card', style: { display: 'flex', gap: '10px', alignItems: 'center' } });
  const grid = h('div', {});
  container.append(toolbar, h('div', { style: { height: '16px' } }), grid);

  const statusSel = h('select', { class: 'select', style: { width: '130px' }, onchange: (e) => { state.status = e.target.value; state.page = 1; load(); } },
    h('option', { value: '' }, '全部状态'),
    [['pending', '排队中'], ['processing', '生成中'], ['succeeded', '已完成'], ['failed', '失败']]
      .map(([v, label]) => h('option', { value: v }, label)));
  const qInput = h('input', {
    class: 'input', style: { width: '220px' }, placeholder: '搜索提示词 / 模板名…',
    onkeydown: (e) => { if (e.key === 'Enter') { state.q = qInput.value; state.page = 1; load(); } },
  });
  toolbar.append(statusSel, qInput,
    h('button', { class: 'btn ghost sm', onclick: () => { state.q = qInput.value; state.page = 1; load(); } }, '搜索'),
    h('div', { style: { flex: 1 } }),
    h('button', { class: 'btn ghost sm', onclick: load }, '刷新'));

  async function load() {
    if (state.stopped) return;
    try {
      const params = new URLSearchParams({ page: state.page, page_size: state.pageSize });
      if (state.status) params.set('status', state.status);
      if (state.q) params.set('q', state.q);
      state.data = await api.get('/api/web/generations?' + params);
      if (state.stopped) return;
      renderGrid();
      // 有进行中的任务时自动刷新
      const hasActive = state.data.items.some(j => j.status === 'pending' || j.status === 'processing');
      clearTimeout(state.timer);
      if (hasActive) state.timer = setTimeout(load, 3000);
    } catch (e) { toast(e.message, 'error'); }
  }

  function cover(job) {
    if (job.images && job.images.length) return h('img', { src: job.images[0].thumbnail_url || job.images[0].url });
    if (job.input_images && job.input_images.length) return h('img', { src: job.input_images[0], style: { opacity: .45 } });
    return h('div', { class: 'placeholder' }, job.status === 'failed' ? '😥' : '🎨');
  }

  function renderGrid() {
    grid.innerHTML = '';
    const { items, total } = state.data;
    if (!items.length) {
      // 删掉末页最后一条后会停在空页，自动回退到有内容的页
      if (total > 0 && state.page > 1) { state.page = Math.max(1, Math.ceil(total / state.pageSize)); return load(); }
      const msg = (state.q || state.status) ? '当前筛选条件下没有记录' : '还没有生成记录，去「生成工作台」试试吧';
      grid.append(h('div', { class: 'card' }, emptyState('🗂', msg)));
      return;
    }
    grid.append(h('div', { class: 'hist-grid' },
      items.map(job =>
        h('div', { class: 'hist-card', onclick: () => openDetail(job.job_id) },
          h('div', { class: 'hist-cover' }, cover(job)),
          h('div', { class: 'hist-info' },
            h('div', { class: 'hist-name' }, job.template_name || '自由提示词'),
            h('div', { class: 'hist-meta' },
              statusBadge(job.status),
              h('span', {}, fmtTime(job.created_at))))))));

    const pages = Math.max(1, Math.ceil(total / state.pageSize));
    grid.append(h('div', { class: 'pager' },
      h('button', { class: 'btn ghost sm', disabled: state.page <= 1, onclick: () => { state.page--; load(); } }, '上一页'),
      h('span', {}, `第 ${state.page} / ${pages} 页 · 共 ${total} 条`),
      h('button', { class: 'btn ghost sm', disabled: state.page >= pages, onclick: () => { state.page++; load(); } }, '下一页')));
  }

  async function openDetail(jobId) {
    let job;
    try { job = await api.get('/api/web/generations/' + jobId); }
    catch (e) { return toast(e.message, 'error'); }

    const body = h('div', {},
      h('div', { style: { marginBottom: '10px', display: 'flex', gap: '8px', alignItems: 'center' } },
        statusBadge(job.status),
        h('span', { style: { color: 'var(--text-3)', fontSize: '12px' } },
          `${fmtTime(job.created_at)} · ${job.params.size || ''} · ${job.params.n || 1} 张` + (job.source === 'api' ? ' · 来自 ERP/API' : ''))),
      job.error ? h('div', { class: 'error-box', style: { marginBottom: '10px' } }, job.error) : null,
      job.images.length ? h('div', { class: 'result-imgs', style: { marginBottom: '12px' } },
        job.images.map((img, i) =>
          h('div', { class: 'result-img' },
            h('img', { src: img.thumbnail_url || img.url, onclick: () => lightbox(img.url) }),
            h('button', { class: 'dl', onclick: (e) => { e.stopPropagation(); downloadUrl(img.url, 'designkit_' + job.job_id.slice(0, 8) + '_' + (i + 1) + '.' + (img.format || 'png')); } }, '下载')))) : null,
      (job.input_images && job.input_images.length) ? h('div', { style: { marginBottom: '12px' } },
        h('div', { class: 'kv' }, '原始商品图：'),
        h('div', { class: 'upload-thumbs' },
          job.input_images.map(url => h('div', { class: 'upload-thumb' }, h('img', { src: url, onclick: () => lightbox(url) }))))) : null,
      h('div', { class: 'kv' }, '最终提示词：'),
      h('div', { class: 'code-block', style: { whiteSpace: 'pre-wrap' } }, job.prompt),
      h('button', { class: 'btn ghost sm', onclick: () => copyText(job.prompt) }, '复制提示词'));

    const retryBtn = h('button', {
      class: 'btn', onclick: async () => {
        try { await api.post('/api/web/generations/' + job.job_id + '/retry'); m.close(); toast('已重新排队', 'success'); load(); }
        catch (e) { toast(e.message, 'error'); }
      },
    }, '重新生成');
    const delBtn = h('button', {
      class: 'btn danger', onclick: async () => {
        if (!(await confirmDialog('删除后图片文件也会一并删除，确定吗？', { danger: true, okText: '删除' }))) return;
        try { await api.del('/api/web/generations/' + job.job_id); m.close(); toast('已删除', 'success'); load(); }
        catch (e) { toast(e.message, 'error'); }
      },
    }, '删除');

    const m = modal({
      title: job.template_name || '自由提示词', body, wide: true,
      footer: [delBtn, (job.status === 'succeeded' || job.status === 'failed') ? retryBtn : null],
    });
  }

  load();
  return () => { state.stopped = true; clearTimeout(state.timer); }; // 离开页面时停止自动刷新
}
