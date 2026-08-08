// 提示词模板管理：CRUD + 分类 + JSON 导入导出 + 变量编辑器
import { api } from '../api.js';
import { confirmDialog, downloadUrl, emptyState, field, h, modal, toast } from '../ui.js';

export function renderTemplates(container, user) {
  const isAdmin = user.role === 'admin';
  const state = { templates: [], categories: [] };

  const toolbar = h('div', { class: 'card', style: { display: 'flex', gap: '10px', flexWrap: 'wrap' } });
  const listCard = h('div', { class: 'card', style: { marginTop: '16px' } });
  container.append(toolbar, listCard);

  async function load() {
    try {
      const [templates, categories] = await Promise.all([
        api.get('/api/web/templates?include_disabled=true'),
        api.get('/api/web/categories'),
      ]);
      state.templates = templates;
      state.categories = categories;
      renderToolbar(); renderList();
    } catch (e) { toast(e.message, 'error'); }
  }

  function renderToolbar() {
    toolbar.innerHTML = '';
    if (!isAdmin) {
      toolbar.append(h('div', { style: { color: 'var(--text-2)', fontSize: '13px' } },
        '共 ' + state.templates.length + ' 个模板（只有管理员可以编辑）'));
      return;
    }
    toolbar.append(
      h('button', { class: 'btn', onclick: () => editTemplate(null) }, '＋ 新建模板'),
      h('button', { class: 'btn ghost', onclick: manageCategories }, '管理分类'),
      h('button', { class: 'btn ghost', onclick: importJson }, '导入提示词库(JSON)'),
      h('button', {
        class: 'btn ghost', onclick: async () => {
          try {
            const data = await api.get('/api/web/templates/export');
            const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
            downloadUrl(URL.createObjectURL(blob), 'designkit_templates.json');
          } catch (e) { toast(e.message, 'error'); }
        },
      }, '导出(JSON)'));
  }

  function renderList() {
    listCard.innerHTML = '';
    if (!state.templates.length) {
      listCard.append(emptyState('📚', '还没有提示词模板'));
      return;
    }
    listCard.append(h('table', { class: 'table' },
      h('thead', {}, h('tr', {},
        h('th', {}, '模板'), h('th', {}, '分类'), h('th', {}, '变量'),
        h('th', {}, '需商品图'), h('th', {}, '状态'), isAdmin ? h('th', { style: { width: '150px' } }, '操作') : null)),
      h('tbody', {},
        state.templates.map(t =>
          h('tr', {},
            h('td', {},
              h('div', { style: { fontWeight: 600 } }, t.name),
              h('div', { style: { fontSize: '12px', color: 'var(--text-3)', maxWidth: '360px' } }, t.description || '')),
            h('td', {}, t.category_name || '-'),
            h('td', {}, (t.variables || []).length ? (t.variables || []).map(v => v.label || v.name).join('、') : '-'),
            h('td', {}, t.requires_input_image ? '是' : '否'),
            h('td', {}, t.is_enabled ? h('span', { class: 'badge green' }, '启用') : h('span', { class: 'badge gray' }, '停用')),
            isAdmin ? h('td', {},
              h('button', { class: 'btn ghost sm', onclick: () => editTemplate(t) }, '编辑'), ' ',
              h('button', {
                class: 'btn danger sm', onclick: async () => {
                  if (!(await confirmDialog(`确定删除模板「${t.name}」吗？`, { danger: true, okText: '删除' }))) return;
                  try { await api.del('/api/web/templates/' + t.id); toast('已删除', 'success'); load(); }
                  catch (e) { toast(e.message, 'error'); }
                },
              }, '删除')) : null)))));
  }

  // ------------------------------------------------------ 模板编辑弹窗

  function editTemplate(tpl) {
    const isNew = !tpl;
    const draft = tpl ? JSON.parse(JSON.stringify(tpl)) : {
      name: '', category_id: null, description: '', prompt_template: '',
      variables: [], default_params: { size: '1024x1024', n: 1, quality: 'high' },
      requires_input_image: true, is_enabled: true, sort: 0,
    };
    draft.default_params = draft.default_params || {};

    const nameInput = h('input', { class: 'input', value: draft.name, oninput: e => draft.name = e.target.value });
    const catSel = h('select', { class: 'select', onchange: e => draft.category_id = e.target.value ? parseInt(e.target.value) : null },
      h('option', { value: '' }, '（无分类）'),
      state.categories.map(c => h('option', { value: c.id, selected: draft.category_id === c.id }, c.name)));
    const descInput = h('textarea', { class: 'textarea', rows: 2, value: draft.description, oninput: e => draft.description = e.target.value });
    const promptInput = h('textarea', {
      class: 'textarea', rows: 5, value: draft.prompt_template,
      placeholder: '英文效果最佳。可用 {变量名} 占位，例如 {scene}',
      oninput: e => draft.prompt_template = e.target.value,
    });
    const needImg = h('input', { type: 'checkbox', checked: draft.requires_input_image, onchange: e => draft.requires_input_image = e.target.checked });
    const enabled = h('input', { type: 'checkbox', checked: draft.is_enabled, onchange: e => draft.is_enabled = e.target.checked });

    const varsWrap = h('div', {});
    function renderVars() {
      varsWrap.innerHTML = '';
      if (draft.variables.length) {
        varsWrap.append(h('div', { class: 'var-row' },
          ['变量名(英文)', '显示名称', '类型', '选项(逗号分隔)', '默认值', '必填', ''].map(t => h('div', { class: 'var-head' }, t))));
      }
      draft.variables.forEach((v, i) => {
        varsWrap.append(h('div', { class: 'var-row' },
          h('input', { class: 'input', value: v.name || '', placeholder: 'scene', oninput: e => v.name = e.target.value }),
          h('input', { class: 'input', value: v.label || '', placeholder: '场景', oninput: e => v.label = e.target.value }),
          h('select', { class: 'select', onchange: e => { v.type = e.target.value; renderVars(); } },
            h('option', { value: 'text', selected: v.type !== 'select' }, '文本'),
            h('option', { value: 'select', selected: v.type === 'select' }, '下拉')),
          h('input', {
            class: 'input', disabled: v.type !== 'select',
            value: (v.options || []).join(','), placeholder: '选项1,选项2',
            oninput: e => v.options = e.target.value.split(',').map(s => s.trim()).filter(Boolean),
          }),
          h('input', { class: 'input', value: v.default || '', oninput: e => v.default = e.target.value }),
          h('input', { type: 'checkbox', checked: !!v.required, onchange: e => v.required = e.target.checked }),
          h('button', { class: 'icon-btn', onclick: () => { draft.variables.splice(i, 1); renderVars(); } }, '🗑')));
      });
      varsWrap.append(h('button', {
        class: 'btn ghost sm', style: { marginTop: '4px' },
        onclick: () => { draft.variables.push({ name: '', label: '', type: 'text', options: [], default: '', required: false }); renderVars(); },
      }, '＋ 添加变量'));
    }
    renderVars();

    const sizeInput = h('input', { class: 'input', value: draft.default_params.size || '', placeholder: '1024x1024', oninput: e => draft.default_params.size = e.target.value });
    const nInput = h('input', { class: 'input', type: 'number', min: 1, max: 4, value: draft.default_params.n || 1, oninput: e => draft.default_params.n = parseInt(e.target.value) || 1 });
    const qualSel = h('select', { class: 'select', onchange: e => draft.default_params.quality = e.target.value },
      ['auto', 'high', 'medium', 'low'].map(q => h('option', { value: q, selected: (draft.default_params.quality || 'high') === q }, q)));

    const saveBtn = h('button', {
      class: 'btn', onclick: async () => {
        if (!draft.name.trim()) return toast('请填写模板名称', 'error');
        if (!draft.prompt_template.trim()) return toast('请填写提示词内容', 'error');
        draft.variables = draft.variables.filter(v => (v.name || '').trim());
        try {
          if (isNew) await api.post('/api/web/templates', draft);
          else await api.put('/api/web/templates/' + tpl.id, draft);
          m.close(); toast('已保存', 'success'); load();
        } catch (e) { toast(e.message, 'error'); }
      },
    }, '保存');

    const m = modal({
      title: isNew ? '新建提示词模板' : '编辑：' + tpl.name,
      wide: true,
      body: h('div', {},
        h('div', { class: 'row' }, field('模板名称', nameInput, { required: true }), field('分类', catSel)),
        field('模板描述', descInput, { help: '给使用者看的一句话说明' }),
        field('提示词内容', promptInput, { required: true, help: '可用 {变量名} 占位，生成时会替换成用户填的值' }),
        field('变量定义', varsWrap),
        h('div', { class: 'row' },
          field('默认尺寸', sizeInput), field('默认张数', nInput), field('默认画质', qualSel)),
        h('div', { class: 'row' },
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, needImg, '需要上传商品图'),
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, enabled, '启用该模板'))),
      footer: [saveBtn],
    });
  }

  // ------------------------------------------------------ 分类管理

  function manageCategories() {
    const wrap = h('div', {});
    function renderCats() {
      wrap.innerHTML = '';
      state.categories.forEach(c => {
        wrap.append(h('div', { style: { display: 'flex', gap: '8px', marginBottom: '8px', alignItems: 'center' } },
          h('input', { class: 'input', value: c.name, oninput: e => c.name = e.target.value }),
          h('button', {
            class: 'btn ghost sm', onclick: async () => {
              try { await api.put('/api/web/categories/' + c.id, { name: c.name, sort: c.sort || 0 }); toast('已保存', 'success'); load(); }
              catch (e) { toast(e.message, 'error'); }
            },
          }, '保存'),
          h('button', {
            class: 'btn danger sm', onclick: async () => {
              if (!(await confirmDialog(`删除分类「${c.name}」？（模板不会被删除，只会变成无分类）`, { danger: true, okText: '删除' }))) return;
              try { await api.del('/api/web/categories/' + c.id); load(); manageAgain(); }
              catch (e) { toast(e.message, 'error'); }
            },
          }, '删除')));
      });
      const newInput = h('input', { class: 'input', placeholder: '新分类名称' });
      wrap.append(h('div', { style: { display: 'flex', gap: '8px', marginTop: '12px' } },
        newInput,
        h('button', {
          class: 'btn sm', onclick: async () => {
            if (!newInput.value.trim()) return;
            try { await api.post('/api/web/categories', { name: newInput.value.trim(), sort: state.categories.length }); await load(); manageAgain(); }
            catch (e) { toast(e.message, 'error'); }
          },
        }, '添加')));
    }
    function manageAgain() { m.close(); manageCategories(); }
    renderCats();
    const m = modal({ title: '管理分类', body: wrap });
  }

  // ------------------------------------------------------ 导入

  function importJson() {
    const fileInput = h('input', {
      type: 'file', accept: '.json,application/json', style: { display: 'none' },
      onchange: async (e) => {
        const file = e.target.files[0];
        if (!file) return;
        try {
          const text = await file.text();
          const items = JSON.parse(text);
          if (!Array.isArray(items)) throw new Error('JSON 顶层必须是数组');
          const result = await api.post('/api/web/templates/import', items);
          toast(`导入完成：新建 ${result.created} 个，更新 ${result.updated} 个`, 'success');
          load();
        } catch (err) { toast('导入失败：' + err.message, 'error'); }
        finally { e.target.value = ''; } // 重置，否则重选同一文件不触发 change
      },
    });
    container.append(fileInput);
    modal({
      title: '导入提示词库',
      body: h('div', {},
        h('p', { style: { marginBottom: '10px' } }, '支持 JSON 数组格式，每个元素包含以下字段（与「导出」格式一致）：'),
        h('div', { class: 'code-block' },
`[
  {
    "name": "模板名称（同名会覆盖更新）",
    "description": "描述",
    "prompt_template": "提示词，可用 {scene} 占位",
    "variables": [
      {"name": "scene", "label": "场景", "type": "text",
       "default": "", "required": true}
    ],
    "default_params": {"size": "1024x1024", "n": 1, "quality": "high"},
    "requires_input_image": true,
    "is_enabled": true
  }
]`),
        h('p', { style: { fontSize: '12px', color: 'var(--text-3)' } },
          '你也可以把提示词库直接发给 AI 助手，让它帮你转成这个格式。')),
      footer: [h('button', { class: 'btn', onclick: () => fileInput.click() }, '选择 JSON 文件')],
    });
  }

  load();
}
