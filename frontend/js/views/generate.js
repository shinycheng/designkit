// 生成工作台：上传商品图 → 选模板/填变量 → 生成 → 实时看结果
import { api } from '../api.js';
import { downloadUrl, emptyState, field, h, lightbox, statusBadge, toast } from '../ui.js';

const SIZES = [
  ['1024x1024', '方图 1:1（主图）'],
  ['1536x1024', '横图 3:2（场景/横幅）'],
  ['1024x1536', '竖图 2:3（详情/社媒）'],
];
const QUALITIES = [['auto', '自动'], ['high', '高'], ['medium', '中'], ['low', '低']];

export function renderGenerate(container) {
  const state = {
    uploads: [],          // [{id, url, original_name}]
    templates: [],
    categories: [],
    activeCategory: null, // null=全部
    selected: undefined,  // undefined=未选, null=自由提示词, 或模板对象
    varValues: {},
    freePrompt: '',
    extra: '',
    size: '1024x1024',
    n: 1,
    quality: 'high',
    job: null,
    polling: null,
  };

  const left = h('div', {});
  const right = h('div', {});
  container.append(h('div', { class: 'gen-layout' }, left, right));

  // ---------------------------------------------------------- 左侧表单

  function renderLeft() {
    left.innerHTML = '';
    left.append(uploadCard(), templateCard(), paramsCard());
  }

  function uploadCard() {
    const fileInput = h('input', {
      type: 'file', accept: 'image/png,image/jpeg,image/webp', multiple: true,
      style: { display: 'none' },
      onchange: (e) => addFiles([...e.target.files]),
    });
    const dz = h('div', {
      class: 'dropzone',
      onclick: () => fileInput.click(),
      ondragover: (e) => { e.preventDefault(); dz.classList.add('dragover'); },
      ondragleave: () => dz.classList.remove('dragover'),
      ondrop: (e) => {
        e.preventDefault(); dz.classList.remove('dragover');
        addFiles([...e.dataTransfer.files].filter(f => f.type.startsWith('image/')));
      },
    },
      h('div', { class: 'dropzone-icon' }, '📦'),
      h('div', {}, '点击或拖拽上传商品图（最多 4 张）'),
      h('div', { style: { fontSize: '12px', marginTop: '4px' } }, '支持 PNG / JPG / WEBP，单张不超过 20MB'));

    const thumbs = h('div', { class: 'upload-thumbs' },
      state.uploads.map((u, i) =>
        h('div', { class: 'upload-thumb' },
          h('img', { src: u.url, onclick: () => lightbox(u.url) }),
          h('button', { class: 'rm', onclick: () => { state.uploads.splice(i, 1); renderLeft(); } }, '✕'))));

    async function addFiles(files) {
      for (const file of files) {
        if (state.uploads.length >= 4) { toast('最多上传 4 张商品图', 'error'); break; }
        const fd = new FormData();
        fd.append('file', file);
        try {
          const up = await api.post('/api/web/uploads', fd);
          state.uploads.push(up);
        } catch (e) { toast(file.name + '：' + e.message, 'error'); }
      }
      renderLeft();
    }

    return h('div', { class: 'card' },
      h('div', { class: 'card-title' }, '① 商品图',
        h('span', { class: 'hint' }, '生成时 AI 会保留商品原貌，替换背景与场景')),
      fileInput, dz, state.uploads.length ? thumbs : null);
  }

  function templateCard() {
    const cats = h('div', { class: 'chips' },
      h('button', {
        class: 'chip' + (state.activeCategory === null ? ' active' : ''),
        onclick: () => { state.activeCategory = null; renderLeft(); },
      }, '全部'),
      state.categories.map(c =>
        h('button', {
          class: 'chip' + (state.activeCategory === c.id ? ' active' : ''),
          onclick: () => { state.activeCategory = c.id; renderLeft(); },
        }, c.name)));

    const shown = state.templates.filter(t => state.activeCategory === null || t.category_id === state.activeCategory);

    const freeCard = h('div', {
      class: 'tpl-card' + (state.selected === null ? ' active' : ''),
      onclick: () => { state.selected = null; renderLeft(); },
    },
      h('div', { class: 'tpl-card-name' }, '✍️ 自由提示词'),
      h('div', { class: 'tpl-card-desc' }, '不用模板，自己描述想要的画面'));

    const grid = h('div', { class: 'tpl-grid' },
      freeCard,
      shown.map(t =>
        h('div', {
          class: 'tpl-card' + (state.selected && state.selected.id === t.id ? ' active' : ''),
          onclick: () => { state.selected = t; state.varValues = {}; applyTemplateDefaults(t); renderLeft(); },
        },
          t.thumbnail_url ? h('img', { class: 'tpl-card-thumb', src: t.thumbnail_url }) : null,
          h('div', { class: 'tpl-card-name' }, t.name),
          h('div', { class: 'tpl-card-desc' }, t.description || '（无描述）'))));

    const detail = h('div', { style: { marginTop: '14px' } });
    if (state.selected === null) {
      detail.append(field('画面描述', h('textarea', {
        class: 'textarea', rows: 3, value: state.freePrompt,
        placeholder: '例如：把商品放在温馨的圣诞客厅茶几上，暖色灯光，营造节日氛围',
        oninput: (e) => { state.freePrompt = e.target.value; },
      }), { required: true }));
    } else if (state.selected) {
      const t = state.selected;
      for (const v of (t.variables || [])) {
        const name = v.name;
        let control;
        if (v.type === 'select' && Array.isArray(v.options) && v.options.length) {
          // 默认值必须是选项之一，否则回退到第一个选项（避免空默认值导致显示已选、实际提交空串）
          const defVal = v.options.includes(v.default) ? v.default : v.options[0];
          if (state.varValues[name] === undefined) state.varValues[name] = defVal;
          const cur = state.varValues[name];
          control = h('select', {
            class: 'select',
            onchange: (e) => { state.varValues[name] = e.target.value; },
          }, v.options.map(opt => h('option', { value: opt, selected: cur === opt }, opt)));
        } else {
          control = h('input', {
            class: 'input', value: state.varValues[name] ?? v.default ?? '',
            placeholder: v.placeholder || '',
            oninput: (e) => { state.varValues[name] = e.target.value; },
          });
        }
        detail.append(field(v.label || name, control, { required: !!v.required }));
      }
      if (!(t.variables || []).length) {
        detail.append(h('div', { class: 'field-help' }, '该模板无需填写变量，直接生成即可。'));
      }
    }

    return h('div', { class: 'card' },
      h('div', { class: 'card-title' }, '② 选择提示词模板'),
      cats, grid, detail);
  }

  function paramsCard() {
    const busy = state.submitting || (state.job && (state.job.status === 'pending' || state.job.status === 'processing'));
    const genBtn = h('button', { class: 'btn lg', onclick: submit },
      busy
        ? [h('span', { class: 'spinner', style: { borderTopColor: '#fff', borderColor: 'rgba(255,255,255,.35)' } }), ' 生成中…']
        : '🚀 开始生成');
    if (busy) genBtn.disabled = true;

    return h('div', { class: 'card' },
      h('div', { class: 'card-title' }, '③ 生成参数'),
      h('div', { class: 'row' },
        field('图片尺寸', h('select', { class: 'select', onchange: (e) => { state.size = e.target.value; } },
          SIZES.map(([v, label]) => h('option', { value: v, selected: state.size === v }, label)))),
        field('生成张数', h('select', { class: 'select', onchange: (e) => { state.n = parseInt(e.target.value); } },
          [1, 2, 3, 4].map(n => h('option', { value: n, selected: state.n === n }, n + ' 张')))),
        field('画质', h('select', { class: 'select', onchange: (e) => { state.quality = e.target.value; } },
          QUALITIES.map(([v, label]) => h('option', { value: v, selected: state.quality === v }, label))))),
      field('补充要求（可选）', h('textarea', {
        class: 'textarea', rows: 2, value: state.extra,
        placeholder: '例如：整体偏暖色调；商品放大一些；背景不要出现文字',
        oninput: (e) => { state.extra = e.target.value; },
      })),
      genBtn);
  }

  function applyTemplateDefaults(t) {
    const p = t.default_params || {};
    if (p.size && SIZES.some(([v]) => v === p.size)) state.size = p.size; // 只接受下拉里存在的尺寸
    if (p.n) state.n = Math.max(1, Math.min(4, parseInt(p.n) || 1));
    if (p.quality && QUALITIES.some(([v]) => v === p.quality)) state.quality = p.quality;
  }

  // ---------------------------------------------------------- 提交与轮询

  async function submit() {
    if (state.submitting) return; // 防止请求在途时连点创建多个付费任务
    if (state.selected === undefined) return toast('请先选择一个提示词模板', 'error');
    if (state.selected === null && !state.freePrompt.trim()) return toast('请填写画面描述', 'error');
    if (state.selected && state.selected.requires_input_image && !state.uploads.length) {
      return toast('该模板需要先上传商品图', 'error');
    }
    for (const v of (state.selected?.variables || [])) {
      if (v.required && !String(state.varValues[v.name] ?? v.default ?? '').trim()) {
        return toast('请填写：' + (v.label || v.name), 'error');
      }
    }
    const body = {
      template_id: state.selected ? state.selected.id : null,
      prompt: state.selected === null ? state.freePrompt : null,
      variables: state.varValues,
      extra_instructions: state.extra || null,
      upload_ids: state.uploads.map(u => u.id),
      n: state.n, size: state.size, quality: state.quality,
    };
    state.submitting = true;
    renderLeft();
    try {
      const job = await api.post('/api/web/generations', body);
      state.job = job;
      startPolling();
    } catch (e) { toast(e.message, 'error'); }
    finally { state.submitting = false; renderLeft(); renderRight(); }
  }

  function startPolling() {
    stopPolling();
    state.polling = setInterval(async () => {
      if (!state.job) return stopPolling();
      try {
        const job = await api.get('/api/web/generations/' + state.job.job_id);
        state.job = job;
        if (job.status === 'succeeded' || job.status === 'failed') {
          stopPolling();
          renderLeft();
          if (job.status === 'succeeded') toast('生成完成 🎉', 'success');
        }
        renderRight();
      } catch (e) {
        if (e && e.status === 404) { // 任务被删除：停止轮询
          stopPolling();
          state.job = null;
          renderLeft(); renderRight();
          toast('任务不存在或已被删除', 'error');
        } // 其他错误视为网络抖动，下个周期再试
      }
    }, 2000);
  }
  function stopPolling() {
    if (state.polling) { clearInterval(state.polling); state.polling = null; }
  }

  // ---------------------------------------------------------- 右侧结果

  function renderRight() {
    right.innerHTML = '';
    const card = h('div', { class: 'card' });
    card.append(h('div', { class: 'card-title' }, '生成结果'));
    const job = state.job;

    if (!job) {
      card.append(emptyState('🎨', '上传商品图并选择模板后，点击「开始生成」'));
    } else if (job.status === 'pending' || job.status === 'processing') {
      card.append(h('div', { class: 'status-panel' },
        h('div', { class: 'spinner lg' }),
        h('div', { class: 'big' }, job.status === 'pending' ? '排队等待中…' : 'AI 正在绘制商品图…'),
        h('div', { class: 'sub' }, '通常需要 20 秒 ~ 2 分钟，可离开此页，稍后在「生成记录」查看')));
    } else if (job.status === 'failed') {
      card.append(
        h('div', { class: 'status-panel' }, h('div', { style: { fontSize: '34px' } }, '😥'),
          h('div', { class: 'big' }, '生成失败')),
        h('div', { class: 'error-box' }, job.error || '未知错误'),
        h('div', { style: { marginTop: '12px' } },
          h('button', { class: 'btn', onclick: retry }, '重试一次')));
    } else {
      card.append(
        h('div', { style: { marginBottom: '10px' } }, statusBadge(job.status), ' ',
          h('span', { style: { fontSize: '12px', color: 'var(--text-3)' } },
            job.template_name || '自由提示词')),
        h('div', { class: 'result-imgs' },
          job.images.map((img, i) =>
            h('div', { class: 'result-img' },
              h('img', { src: img.url, onclick: () => lightbox(img.url) }),
              h('button', {
                class: 'dl',
                onclick: (e) => { e.stopPropagation(); downloadUrl(img.url, 'designkit_' + job.job_id.slice(0, 8) + '_' + (i + 1) + '.' + (img.format || 'png')); },
              }, '下载')))),
        h('div', { style: { marginTop: '12px', display: 'flex', gap: '8px' } },
          h('button', { class: 'btn ghost sm', onclick: retry }, '再生成一批'),
          h('button', { class: 'btn ghost sm', onclick: () => { location.hash = '#/history'; } }, '查看全部记录')));
    }
    right.append(card);
  }

  async function retry() {
    if (!state.job || state.submitting) return;
    state.submitting = true; renderRight();
    try {
      const job = await api.post('/api/web/generations/' + state.job.job_id + '/retry');
      state.job = job;
      startPolling();
    } catch (e) { toast(e.message, 'error'); }
    finally { state.submitting = false; renderLeft(); renderRight(); }
  }

  // ---------------------------------------------------------- 初始化

  (async () => {
    try {
      const [templates, categories] = await Promise.all([
        api.get('/api/web/templates'),
        api.get('/api/web/categories'),
      ]);
      state.templates = templates;
      state.categories = categories;
    } catch (e) { toast(e.message, 'error'); }
    renderLeft(); renderRight();
  })();

  return stopPolling; // 离开页面时停止轮询
}
