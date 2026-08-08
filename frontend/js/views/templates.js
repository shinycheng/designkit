// 提示词模板：浏览画廊 + 管理表格 + 分组/变量编辑 + 导入预检
import { api } from '../api.js';
import {
  button,
  confirmDialog,
  downloadUrl,
  drawer,
  emptyState,
  field,
  fmtTime,
  h,
  icon,
  inlineAlert,
  modal,
  skeleton,
  statusBadge,
  toast,
} from '../ui.js';

const ALLOWED_SIZES = new Set(['1024x1024', '1536x1024', '1024x1536', 'auto']);
const ALLOWED_QUALITIES = new Set(['auto', 'high', 'medium', 'low']);

export function renderTemplates(container, user) {
  const isAdmin = user.role === 'admin';
  const state = {
    templates: [],
    categories: [],
    query: '',
    categoryId: '',
    status: 'all',
    mode: 'browse',
    loading: false,
    stopped: false,
    overlay: null,
  };

  const page = h('section', { class: 'dk-page dk-page--templates', 'aria-labelledby': 'templates-page-title' });
  const toolbar = h('div', { class: 'dk-page-toolbar dk-template-toolbar' });
  const content = h('div', { class: 'dk-section', 'aria-busy': 'true' }, skeleton({ lines: 7, className: 'dk-loading' }));
  const resultCount = h('span', { class: 'dk-result-count dk-template-toolbar__count', 'aria-live': 'polite' }, '正在读取模板…');

  const searchInput = h('input', {
    class: 'input dk-template-search-input',
    type: 'search',
    placeholder: '搜索模板名或描述',
    'aria-label': '搜索提示词模板',
    oninput: (event) => {
      state.query = event.target.value.trim().toLowerCase();
      renderContent();
    },
  });
  const categorySelect = h('select', {
    class: 'select dk-template-filter',
    'aria-label': '按分类筛选模板',
    onchange: (event) => { state.categoryId = event.target.value; renderContent(); },
  });
  const statusSelect = h('select', {
    class: 'select dk-template-filter',
    'aria-label': '按启用状态筛选模板',
    onchange: (event) => { state.status = event.target.value; renderContent(); },
  },
  h('option', { value: 'all' }, '全部状态'),
  h('option', { value: 'enabled' }, '已启用'),
  h('option', { value: 'disabled' }, '已停用'));

  page.append(
    h('h1', { id: 'templates-page-title', class: 'dk-visually-hidden' }, '提示词模板'),
    toolbar,
    content,
  );
  container.append(page);

  function renderToolbar() {
    categorySelect.replaceChildren(
      h('option', { value: '' }, '全部分类'),
      ...state.categories.map((category) => h('option', {
        value: String(category.id),
        selected: state.categoryId === String(category.id),
      }, category.name)),
    );
    const modeSwitch = isAdmin
      ? h('div', { class: 'dk-view-toggle dk-template-toolbar__modes', role: 'group', 'aria-label': '模板页视图' },
        button('浏览', {
          variant: state.mode === 'browse' ? 'secondary' : 'quiet',
          size: 'sm',
          iconName: 'layout-grid',
          'aria-pressed': state.mode === 'browse' ? 'true' : 'false',
          onclick: () => { state.mode = 'browse'; renderToolbar(); renderContent(); },
        }),
        button('管理', {
          variant: state.mode === 'manage' ? 'secondary' : 'quiet',
          size: 'sm',
          iconName: 'list',
          'aria-pressed': state.mode === 'manage' ? 'true' : 'false',
          onclick: () => { state.mode = 'manage'; renderToolbar(); renderContent(); },
        }))
      : null;

    const secondaryActions = isAdmin ? h('div', { class: 'dk-template-toolbar__secondary' },
      button('分类', { variant: 'quiet', iconName: 'folder-tree', onclick: manageCategories }),
      button('导入', { variant: 'quiet', iconName: 'upload', onclick: importJson }),
      button('导出', { variant: 'quiet', iconName: 'download', onclick: exportJson })) : null;
    const primaryAction = isAdmin
      ? button('新建模板', { iconName: 'plus', className: 'dk-template-toolbar__primary', onclick: () => editTemplate(null) })
      : null;
    const meta = h('div', { class: 'dk-template-toolbar__meta' }, resultCount, secondaryActions, primaryAction);

    toolbar.replaceChildren(...[
      modeSwitch,
      h('div', { class: 'dk-template-toolbar__filters' }, searchInput, categorySelect, isAdmin ? statusSelect : null),
      meta,
    ].filter(Boolean));
  }

  async function load() {
    if (state.loading || state.stopped) return;
    state.loading = true;
    content.setAttribute('aria-busy', 'true');
    try {
      const templatePath = isAdmin ? '/api/web/templates?include_disabled=true' : '/api/web/templates';
      const [templates, categories] = await Promise.all([
        api.get(templatePath),
        api.get('/api/web/categories'),
      ]);
      if (state.stopped) return;
      state.templates = templates;
      state.categories = categories;
      renderToolbar();
      renderContent();
    } catch (error) {
      content.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载模板' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
      resultCount.textContent = '加载失败';
    } finally {
      state.loading = false;
      content.setAttribute('aria-busy', 'false');
    }
  }

  function visibleTemplates() {
    return state.templates.filter((template) => {
      const haystack = `${template.name || ''} ${template.description || ''}`.toLowerCase();
      if (state.query && !haystack.includes(state.query)) return false;
      if (state.categoryId && String(template.category_id || '') !== state.categoryId) return false;
      if (state.status === 'enabled' && !template.is_enabled) return false;
      if (state.status === 'disabled' && template.is_enabled) return false;
      return true;
    });
  }

  function renderContent() {
    if (!state.templates) return;
    const templates = visibleTemplates();
    resultCount.textContent = `显示 ${templates.length} / ${state.templates.length} 个模板`;
    content.replaceChildren();
    if (!templates.length) {
      const noData = state.templates.length === 0;
      const action = isAdmin && noData
        ? button('新建第一个模板', { iconName: 'plus', onclick: () => editTemplate(null) })
        : button('清除筛选', { variant: 'secondary', onclick: clearFilters });
      content.append(emptyState(
        noData ? 'layout-template' : 'search',
        noData ? '还没有提示词模板' : '没有符合条件的模板',
        {
          description: noData ? '建立可复用的提示词、变量和默认生成参数。' : '请调整搜索关键词、分类或状态。',
          action,
        },
      ));
      return;
    }
    if (state.mode === 'manage' && isAdmin) content.append(renderManagementTable(templates));
    else content.append(renderTemplateGrid(templates));
  }

  function clearFilters() {
    state.query = '';
    state.categoryId = '';
    state.status = 'all';
    searchInput.value = '';
    categorySelect.value = '';
    statusSelect.value = 'all';
    renderContent();
  }

  function renderTemplateGrid(templates) {
    return h('div', { class: 'dk-card-grid dk-template-grid' }, templates.map((template) =>
      h('article', { class: 'dk-template-card', 'data-state': template.is_enabled ? 'enabled' : 'disabled' },
        h('button', {
          type: 'button',
          class: 'dk-template-card__open',
          'aria-label': `查看模板 ${template.name}`,
          onclick: () => previewTemplate(template),
        },
        h('div', { class: 'dk-template-card__media' }, template.thumbnail_url
          ? h('img', { src: template.thumbnail_url, alt: `${template.name}模板预览`, loading: 'lazy' })
          : h('div', { class: 'dk-template-placeholder' }, icon('layout-template', { size: 30 }), h('span', {}, '暂无缩略图'))),
        h('div', { class: 'dk-template-card__body' },
          h('div', { class: 'dk-template-card__title' }, template.name),
          h('p', { class: 'dk-template-card__description' }, template.description || '暂无描述'),
          h('div', { class: 'dk-template-card__meta' },
            h('span', {}, template.category_name || '未分类'),
            h('span', {}, `${(template.variables || []).length} 个变量`),
            isAdmin ? statusBadge(template.is_enabled ? 'active' : 'inactive') : null))))));
  }

  function renderManagementTable(templates) {
    return h('div', { class: 'dk-data-table', role: 'region', 'aria-label': '模板管理表', tabindex: '0' },
      h('table', { class: 'table' },
        h('thead', {}, h('tr', {},
          h('th', { scope: 'col' }, '模板'),
          h('th', { scope: 'col' }, '分类'),
          h('th', { scope: 'col' }, '变量'),
          h('th', { scope: 'col' }, '输入图'),
          h('th', { scope: 'col' }, '状态'),
          h('th', { scope: 'col' }, '更新时间'),
          h('th', { scope: 'col' }, '操作'))),
        h('tbody', {}, templates.map((template) => h('tr', {},
          h('th', { scope: 'row' },
            h('div', { class: 'dk-template-card__title' }, template.name),
            h('div', { class: 'dk-template-card__description' }, template.description || '暂无描述')),
          h('td', {}, template.category_name || '未分类'),
          h('td', {}, (template.variables || []).length
            ? (template.variables || []).map((variable) => variable.label || variable.name).join('、')
            : '无'),
          h('td', {}, template.requires_input_image ? '需要' : '不需要'),
          h('td', {}, statusBadge(template.is_enabled ? 'active' : 'inactive')),
          h('td', {}, fmtTime(template.updated_at)),
          h('td', {}, h('div', { class: 'dk-inline-actions' },
            button('编辑', { variant: 'quiet', size: 'sm', iconName: 'pencil', onclick: () => editTemplate(template) }),
            button('删除', { variant: 'danger', size: 'sm', iconName: 'trash-2', onclick: () => deleteTemplate(template) }))))))));
  }

  function previewTemplate(template) {
    let overlay;
    let saving = false;
    overlay = drawer({
      title: template.name,
      body: h('div', { class: 'dk-panel-stack' },
        template.thumbnail_url
          ? h('img', { class: 'dk-template-preview', src: template.thumbnail_url, alt: `${template.name}模板预览` })
          : h('div', { class: 'dk-template-placeholder' }, icon('layout-template', { size: 36 }), h('span', {}, '暂无缩略图')),
        h('p', {}, template.description || '暂无描述'),
        h('div', { class: 'dk-template-card__meta' },
          h('span', {}, template.category_name || '未分类'),
          h('span', {}, template.requires_input_image ? '需要商品原图' : '无需商品原图')),
        h('section', { class: 'dk-panel-stack' },
          h('h3', {}, '提示词'),
          h('pre', { class: 'code-block' }, template.prompt_template)),
        (template.variables || []).length
          ? h('section', { class: 'dk-panel-stack' },
            h('h3', {}, '可配置变量'),
            h('ul', {}, template.variables.map((variable) => h('li', {}, variable.label || variable.name))))
          : null),
      footer: [
        isAdmin ? button('编辑模板', { variant: 'secondary', iconName: 'pencil', onclick: () => { overlay.close(); editTemplate(template); } }) : null,
        button('去生成工作台', { iconName: 'sparkles', onclick: () => { location.hash = '#/generate'; } }),
      ],
      onClose: () => { state.overlay = null; },
    });
    state.overlay = overlay;
  }

  function editTemplate(template) {
    const isNew = !template;
    const draft = template ? structuredCloneSafe(template) : {
      name: '',
      category_id: null,
      description: '',
      prompt_template: '',
      variables: [],
      default_params: { size: '1024x1024', n: 1, quality: 'high' },
      requires_input_image: true,
      is_enabled: true,
      sort: 0,
    };
    draft.variables = Array.isArray(draft.variables) ? draft.variables : [];
    draft.default_params = draft.default_params || {};

    const nameInput = h('input', { class: 'input', value: draft.name, oninput: (event) => { draft.name = event.target.value; } });
    const categoryInput = h('select', { class: 'select', onchange: (event) => { draft.category_id = event.target.value ? Number(event.target.value) : null; } },
      h('option', { value: '' }, '未分类'),
      state.categories.map((category) => h('option', {
        value: String(category.id),
        selected: Number(draft.category_id) === Number(category.id),
      }, category.name)));
    const descriptionInput = h('textarea', { class: 'textarea', rows: 3, value: draft.description || '', oninput: (event) => { draft.description = event.target.value; } });
    const promptInput = h('textarea', {
      class: 'textarea',
      rows: 9,
      value: draft.prompt_template || '',
      placeholder: '例如：将商品置于 {scene} 中，使用 {lighting} 光线…',
      oninput: (event) => { draft.prompt_template = event.target.value; },
    });
    const variablesRoot = h('div', { class: 'dk-variable-list' });
    const sizeInput = h('select', { class: 'select', onchange: (event) => { draft.default_params.size = event.target.value; } },
      [['1024x1024', '方形 · 1024 × 1024'], ['1536x1024', '横幅 · 1536 × 1024'], ['1024x1536', '竖幅 · 1024 × 1536'], ['auto', '自动']]
        .map(([value, label]) => h('option', { value, selected: (draft.default_params.size || '1024x1024') === value }, label)));
    const countInput = h('input', { class: 'input', type: 'number', min: 1, max: 4, value: draft.default_params.n || 1, oninput: (event) => { draft.default_params.n = Number(event.target.value) || 1; } });
    const qualityInput = h('select', { class: 'select', onchange: (event) => { draft.default_params.quality = event.target.value; } },
      [['auto', '自动'], ['high', '高画质'], ['medium', '中画质'], ['low', '低画质']]
        .map(([value, label]) => h('option', { value, selected: (draft.default_params.quality || 'high') === value }, label)));
    const requiresInput = h('input', { type: 'checkbox', checked: draft.requires_input_image, onchange: (event) => { draft.requires_input_image = event.target.checked; } });
    const enabledInput = h('input', { type: 'checkbox', checked: draft.is_enabled, onchange: (event) => { draft.is_enabled = event.target.checked; } });
    const sortInput = h('input', { class: 'input', type: 'number', value: draft.sort || 0, oninput: (event) => { draft.sort = Number(event.target.value) || 0; } });
    const formAlert = h('div', { 'aria-live': 'polite' });

    function renderVariables() {
      variablesRoot.replaceChildren();
      if (!draft.variables.length) {
        variablesRoot.append(h('p', { class: 'dk-section-meta' }, '当前模板没有变量，用户将直接使用固定提示词。'));
      }
      draft.variables.forEach((variable, index) => {
        const details = h('details', { class: 'dk-variable-item', open: index === draft.variables.length - 1 ? true : null },
          h('summary', { class: 'dk-variable-head' },
            h('span', { class: 'dk-variable-summary' }, variable.label || variable.name || `变量 ${index + 1}`),
            h('span', {}, variable.required ? '必填' : '可选')),
          h('div', { class: 'dk-variable-body' },
            h('div', { class: 'dk-field-grid' },
              field('变量名', h('input', {
                class: 'input', value: variable.name || '', placeholder: 'scene',
                oninput: (event) => { variable.name = event.target.value; updateVariableSummary(details, variable, index); },
              }), { required: true, help: '可用中文、英文字母、数字和下划线' }),
              field('显示名称', h('input', {
                class: 'input', value: variable.label || '', placeholder: '例如：场景',
                oninput: (event) => { variable.label = event.target.value; updateVariableSummary(details, variable, index); },
              })),
              field('类型', h('select', {
                class: 'select', onchange: (event) => { variable.type = event.target.value; renderVariables(); },
              },
              h('option', { value: 'text', selected: variable.type !== 'select' }, '文本'),
              h('option', { value: 'select', selected: variable.type === 'select' }, '下拉选择'))),
              field('默认值', h('input', {
                class: 'input', value: variable.default || '',
                oninput: (event) => { variable.default = event.target.value; },
              }))),
            variable.type === 'select'
              ? field('选项', h('input', {
                class: 'input', value: (variable.options || []).join(', '), placeholder: '室内, 工作室, 户外',
                oninput: (event) => { variable.options = event.target.value.split(',').map((item) => item.trim()).filter(Boolean); },
              }), { required: true, help: '使用英文逗号分隔多个选项' })
              : null,
            h('label', { class: 'dk-checkbox-row' },
              h('input', { type: 'checkbox', checked: Boolean(variable.required), onchange: (event) => { variable.required = event.target.checked; renderVariables(); } }),
              h('span', {}, '用户生成时必须填写此变量')),
            button('删除变量', {
              variant: 'danger', size: 'sm', iconName: 'trash-2',
              onclick: () => { draft.variables.splice(index, 1); renderVariables(); },
            })));
        variablesRoot.append(details);
      });
      variablesRoot.append(button('添加变量', {
        variant: 'secondary', iconName: 'plus',
        onclick: () => {
          draft.variables.push({ name: '', label: '', type: 'text', options: [], default: '', required: false });
          renderVariables();
        },
      }));
    }

    function updateVariableSummary(details, variable, index) {
      const summary = details.querySelector('.dk-variable-summary');
      if (summary) summary.textContent = variable.label || variable.name || `变量 ${index + 1}`;
    }

    renderVariables();

    const body = h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); save(); } },
      h('section', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-section-head' }, h('div', {}, h('h3', {}, '基本信息'), h('p', { class: 'dk-section-meta' }, '用清晰名称和分类帮助使用者找到模板。'))),
        h('div', { class: 'dk-field-grid' },
          field('模板名称', nameInput, { required: true }),
          field('分类', categoryInput)),
        field('模板描述', descriptionInput, { help: '一句话说明模板的构图、场景或适用商品。' })),
      h('section', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-section-head' }, h('div', {}, h('h3', {}, '提示词'), h('p', { class: 'dk-section-meta' }, '用 {variable_name} 引用下方定义的变量。'))),
        field('提示词内容', promptInput, { required: true })),
      h('section', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-section-head' }, h('div', {}, h('h3', {}, '模板变量'), h('p', { class: 'dk-section-meta' }, '每个变量使用独立的可折叠编辑单元。'))),
        variablesRoot),
      h('section', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-section-head' }, h('div', {}, h('h3', {}, '默认生成参数'), h('p', { class: 'dk-section-meta' }, '用户选中模板时会预先带入。'))),
        h('div', { class: 'dk-field-grid' },
          field('默认尺寸', sizeInput),
          field('默认张数', countInput),
          field('默认画质', qualityInput),
          field('排序值', sortInput, { help: '数值越小越靠前' })),
        h('label', { class: 'dk-checkbox-row' }, requiresInput, h('span', {}, '此模板需要上传商品原图')),
        h('label', { class: 'dk-checkbox-row' }, enabledInput, h('span', {}, '启用此模板'))),
      !isNew ? renderThumbnailEditor(template) : null,
      formAlert,
    );

    let overlay;
    const saveButton = button(isNew ? '创建模板' : '保存更改', { iconName: 'save', type: 'submit', form: '' });
    // button 在 footer 中不属于 form，显式绑定同一 save 函数。
    saveButton.removeAttribute('form');
    saveButton.addEventListener('click', save);
    overlay = drawer({
      title: isNew ? '新建提示词模板' : `编辑：${template.name}`,
      body,
      wide: true,
      footer: [button('取消', { variant: 'quiet', onclick: () => overlay.close() }), saveButton],
      onClose: () => { if (state.overlay === overlay) state.overlay = null; },
    });
    state.overlay = overlay;

    async function save(event) {
      if (event) event.preventDefault();
      if (saving) return;
      formAlert.replaceChildren();
      const validation = validateTemplateDraft(draft);
      if (validation.length) {
        formAlert.append(inlineAlert(validation.join('；'), 'error', { title: '请完善模板内容' }));
        return;
      }
      const payload = normaliseTemplate(draft);
      saving = true;
      saveButton.disabled = true;
      saveButton.setAttribute('aria-busy', 'true');
      try {
        if (isNew) await api.post('/api/web/templates', payload);
        else await api.put('/api/web/templates/' + template.id, payload);
        overlay.close();
        toast(isNew ? '模板已创建' : '模板已保存', 'success');
        await load();
      } catch (error) {
        formAlert.append(inlineAlert(error.message, 'error', { title: '保存失败' }));
      } finally {
        saving = false;
        saveButton.disabled = false;
        saveButton.removeAttribute('aria-busy');
      }
    }
  }

  function renderThumbnailEditor(template) {
    const preview = h('div', { class: 'dk-template-card__media' }, template.thumbnail_url
      ? h('img', { src: template.thumbnail_url, alt: `${template.name}当前缩略图` })
      : h('div', { class: 'dk-template-placeholder' }, icon('image', { size: 28 }), h('span', {}, '暂无缩略图')));
    const fileInput = h('input', {
      type: 'file',
      accept: 'image/png,image/jpeg,image/webp',
      hidden: true,
      onchange: async (event) => {
        const file = event.target.files && event.target.files[0];
        if (!file) return;
        const form = new FormData();
        form.append('file', file);
        try {
          await api.post('/api/web/templates/' + template.id + '/thumbnail', form);
          toast('缩略图已更新', 'success');
          await load();
        } catch (error) {
          toast(error.message, 'error');
        } finally {
          event.target.value = '';
        }
      },
    });
    return h('section', { class: 'dk-panel-stack' },
      h('div', { class: 'dk-section-head' }, h('div', {}, h('h3', {}, '模板缩略图'), h('p', { class: 'dk-section-meta' }, '用真实效果图帮助使用者快速判断构图风格。'))),
      preview,
      fileInput,
      button('上传新缩略图', { variant: 'secondary', iconName: 'upload', onclick: () => fileInput.click() }));
  }

  function validateTemplateDraft(draft) {
    const errors = [];
    if (!String(draft.name || '').trim()) errors.push('请填写模板名称');
    if (String(draft.name || '').trim().length > 128) errors.push('模板名称不能超过 128 个字符');
    if (!String(draft.prompt_template || '').trim()) errors.push('请填写提示词内容');
    const names = new Set();
    draft.variables.forEach((variable, index) => {
      const name = String(variable.name || '').trim();
      if (!/^[A-Za-z0-9_\u4e00-\u9fff]+$/u.test(name)) errors.push(`第 ${index + 1} 个变量名格式不正确`);
      if (names.has(name)) errors.push(`变量名 ${name} 重复`);
      names.add(name);
      if (variable.type === 'select' && !(variable.options || []).length) errors.push(`变量 ${name || index + 1} 需要至少一个选项`);
    });
    const count = Number(draft.default_params.n || 1);
    if (!Number.isInteger(count) || count < 1 || count > 4) errors.push('默认张数必须为 1–4');
    return errors;
  }

  function normaliseTemplate(draft) {
    return {
      name: String(draft.name || '').trim(),
      category_id: draft.category_id == null || draft.category_id === '' ? null : Number(draft.category_id),
      description: String(draft.description || '').trim(),
      prompt_template: String(draft.prompt_template || '').trim(),
      variables: draft.variables.map((variable) => ({
        name: String(variable.name || '').trim(),
        label: String(variable.label || '').trim(),
        type: variable.type === 'select' ? 'select' : 'text',
        options: variable.type === 'select' ? (variable.options || []).map(String) : [],
        default: variable.default == null ? '' : String(variable.default),
        required: Boolean(variable.required),
      })),
      default_params: {
        size: draft.default_params.size || '1024x1024',
        n: Number(draft.default_params.n || 1),
        quality: draft.default_params.quality || 'high',
      },
      requires_input_image: Boolean(draft.requires_input_image),
      is_enabled: Boolean(draft.is_enabled),
      sort: Number(draft.sort || 0),
    };
  }

  async function deleteTemplate(template) {
    const confirmed = await confirmDialog(
      `删除模板「${template.name}」？已有生成记录不会被删除，但此模板无法恢复。`,
      { danger: true, okText: '删除模板' },
    );
    if (!confirmed) return;
    try {
      await api.del('/api/web/templates/' + template.id);
      toast('模板已删除', 'success');
      await load();
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  function manageCategories() {
    const body = h('div', { class: 'dk-category-list' });
    let overlay;

    function renderCategories() {
      body.replaceChildren();
      if (!state.categories.length) {
        body.append(emptyState('folder-open', '还没有分类', { description: '在下方创建第一个模板分类。' }));
      }
      state.categories.forEach((category) => {
        const nameInput = h('input', { class: 'input', value: category.name, 'aria-label': `分类 ${category.name} 的名称` });
        const sortInput = h('input', { class: 'input', type: 'number', value: category.sort || 0, 'aria-label': `分类 ${category.name} 的排序值` });
        body.append(h('div', { class: 'dk-category-item' },
          nameInput,
          sortInput,
          button('保存', {
            variant: 'secondary', size: 'sm', iconName: 'save',
            onclick: async () => {
              if (!nameInput.value.trim()) return toast('分类名称不能为空', 'error');
              try {
                await api.put('/api/web/categories/' + category.id, { name: nameInput.value.trim(), sort: Number(sortInput.value) || 0 });
                toast('分类已保存', 'success');
                await load();
                renderCategories();
              } catch (error) { toast(error.message, 'error'); }
            },
          }),
          button('删除', {
            variant: 'danger', size: 'sm', iconName: 'trash-2',
            onclick: async () => {
              const confirmed = await confirmDialog(
                `删除分类「${category.name}」？分类中的模板会变为未分类。`,
                { danger: true, okText: '删除分类' },
              );
              if (!confirmed) return;
              try {
                await api.del('/api/web/categories/' + category.id);
                toast('分类已删除', 'success');
                await load();
                renderCategories();
              } catch (error) { toast(error.message, 'error'); }
            },
          })));
      });

      const newName = h('input', { class: 'input', placeholder: '新分类名称', 'aria-label': '新分类名称' });
      body.append(h('form', { class: 'dk-category-item', onsubmit: async (event) => {
        event.preventDefault();
        if (!newName.value.trim()) return;
        try {
          await api.post('/api/web/categories', { name: newName.value.trim(), sort: state.categories.length });
          toast('分类已创建', 'success');
          await load();
          renderCategories();
        } catch (error) { toast(error.message, 'error'); }
      } }, newName, button('添加分类', { type: 'submit', iconName: 'plus' })));
    }

    renderCategories();
    overlay = drawer({
      title: '管理模板分类',
      body,
      onClose: () => { if (state.overlay === overlay) state.overlay = null; },
    });
    state.overlay = overlay;
  }

  async function exportJson() {
    try {
      const data = await api.get('/api/web/templates/export');
      const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
      downloadUrl(url, 'designkit_templates.json');
      setTimeout(() => URL.revokeObjectURL(url), 0);
      toast('模板 JSON 已导出', 'success');
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  function importJson() {
    let parsedItems = null;
    let report = null;
    const preview = h('div', { class: 'dk-import-preview', 'aria-live': 'polite' },
      inlineAlert('选择 JSON 文件后，系统会先在本地校验并预览变更，确认后才会写入。', 'info'));
    const fileInput = h('input', {
      type: 'file',
      accept: '.json,application/json',
      hidden: true,
      onchange: async (event) => {
        const file = event.target.files && event.target.files[0];
        if (!file) return;
        try {
          const raw = JSON.parse(await file.text());
          report = validateImport(raw, state.templates, state.categories);
          parsedItems = report.errors.length ? null : report.items;
          renderImportPreview(preview, file.name, report);
          confirmButton.disabled = !parsedItems || parsedItems.length === 0;
        } catch (error) {
          parsedItems = null;
          report = { errors: ['JSON 无法解析：' + error.message], warnings: [], created: 0, updated: 0, items: [] };
          renderImportPreview(preview, file.name, report);
          confirmButton.disabled = true;
        } finally {
          event.target.value = '';
        }
      },
    });
    const selectButton = button('选择 JSON 文件', { variant: 'secondary', iconName: 'file-json', onclick: () => fileInput.click() });
    const confirmButton = button('确认导入', {
      iconName: 'upload',
      disabled: true,
      onclick: async () => {
        if (!parsedItems || !report || report.errors.length) return;
        confirmButton.disabled = true;
        confirmButton.setAttribute('aria-busy', 'true');
        try {
          const result = await api.post('/api/web/templates/import', parsedItems);
          overlay.close();
          toast(`导入完成：新建 ${result.created} 个，更新 ${result.updated} 个`, 'success');
          await load();
        } catch (error) {
          preview.prepend(inlineAlert(error.message, 'error', { title: '导入失败' }));
          confirmButton.disabled = false;
        } finally {
          confirmButton.removeAttribute('aria-busy');
        }
      },
    });
    const body = h('div', { class: 'dk-panel-stack' },
      h('p', {}, '导入格式与「导出 JSON」一致。同名模板会被更新，其他模板会新建。'),
      fileInput,
      selectButton,
      preview);
    const overlay = modal({
      title: '导入提示词模板',
      body,
      wide: true,
      footer: [button('取消', { variant: 'quiet', onclick: () => overlay.close() }), confirmButton],
    });
  }

  function renderImportPreview(root, filename, report) {
    root.replaceChildren(...[
      h('div', { class: 'dk-section-head' },
        h('div', {}, h('h3', {}, filename), h('p', { class: 'dk-section-meta' }, `已读取 ${report.items.length + report.errors.filter((item) => item.startsWith('第 ')).length} 项`))),
      report.errors.length
        ? inlineAlert(`发现 ${report.errors.length} 个错误，修正后重新选择文件。`, 'error', { title: '校验未通过' })
        : inlineAlert(`校验通过：将新建 ${report.created} 个，更新 ${report.updated} 个。`, 'success', { title: '可以导入' }),
      report.errors.length
        ? h('ul', { class: 'dk-validation-list dk-validation-list--error' }, report.errors.map((error) => h('li', {}, error)))
        : null,
      report.warnings.length
        ? h('div', {},
          h('h4', {}, '提示'),
          h('ul', { class: 'dk-validation-list' }, report.warnings.map((warning) => h('li', {}, warning))))
        : null,
      report.items.length
        ? h('div', { class: 'dk-data-table', role: 'region', 'aria-label': '导入模板预览', tabindex: '0' },
          h('table', { class: 'table' },
            h('thead', {}, h('tr', {}, h('th', {}, '模板'), h('th', {}, '变更'), h('th', {}, '变量'), h('th', {}, '状态'))),
            h('tbody', {}, report.items.map((item) => h('tr', {},
              h('td', {}, item.name),
              h('td', {}, report.existingNames.has(item.name) ? '更新' : '新建'),
              h('td', {}, `${item.variables.length} 个`),
              h('td', {}, item.is_enabled ? '启用' : '停用'))))))
        : null,
    ].filter(Boolean));
  }

  function validateImport(raw, templates, categories) {
    const errors = [];
    const warnings = [];
    const items = [];
    const existingNames = new Set(templates.map((template) => template.name));
    const knownNames = new Set(existingNames);
    const categoryIds = new Set(categories.map((category) => Number(category.id)));
    let created = 0;
    let updated = 0;

    if (!Array.isArray(raw)) {
      return { errors: ['JSON 顶层必须是数组'], warnings, items, existingNames, created, updated };
    }
    if (!raw.length) {
      return { errors: ['JSON 数组不能为空'], warnings, items, existingNames, created, updated };
    }

    raw.forEach((source, index) => {
      const prefix = `第 ${index + 1} 项`;
      const itemErrors = [];
      if (!source || typeof source !== 'object' || Array.isArray(source)) {
        errors.push(`${prefix}必须是对象`);
        return;
      }
      const name = typeof source.name === 'string' ? source.name.trim() : '';
      const prompt = typeof source.prompt_template === 'string' ? source.prompt_template.trim() : '';
      if (!name) itemErrors.push('缺少模板名称');
      if (name.length > 128) itemErrors.push('模板名称超过 128 个字符');
      if (!prompt) itemErrors.push('缺少提示词内容');
      const categoryId = source.category_id == null || source.category_id === '' ? null : Number(source.category_id);
      if (categoryId != null && (!Number.isInteger(categoryId) || !categoryIds.has(categoryId))) itemErrors.push('分类 ID 不存在');
      const variables = Array.isArray(source.variables) ? source.variables : [];
      if (source.variables != null && !Array.isArray(source.variables)) itemErrors.push('variables 必须是数组');
      const variableNames = new Set();
      const normalisedVariables = variables.map((variable, variableIndex) => {
        const variableName = String(variable && variable.name || '').trim();
        const variableType = variable && variable.type === 'select' ? 'select' : 'text';
        if (!/^[A-Za-z0-9_\u4e00-\u9fff]+$/u.test(variableName)) itemErrors.push(`第 ${variableIndex + 1} 个变量名格式不正确`);
        if (variableNames.has(variableName)) itemErrors.push(`变量名 ${variableName} 重复`);
        variableNames.add(variableName);
        const options = Array.isArray(variable && variable.options) ? variable.options.map(String).map((value) => value.trim()).filter(Boolean) : [];
        if (variableType === 'select' && !options.length) itemErrors.push(`变量 ${variableName || variableIndex + 1} 没有下拉选项`);
        return {
          name: variableName,
          label: String(variable && variable.label || '').trim(),
          type: variableType,
          options,
          default: variable && variable.default != null ? String(variable.default) : '',
          required: Boolean(variable && variable.required),
        };
      });
      const params = source.default_params && typeof source.default_params === 'object' && !Array.isArray(source.default_params)
        ? source.default_params
        : {};
      const size = params.size || '1024x1024';
      const count = Number(params.n || 1);
      const quality = params.quality || 'high';
      if (!ALLOWED_SIZES.has(size)) itemErrors.push('默认尺寸不受支持');
      if (!Number.isInteger(count) || count < 1 || count > 4) itemErrors.push('默认张数必须为 1–4');
      if (!ALLOWED_QUALITIES.has(quality)) itemErrors.push('默认画质不受支持');
      if (itemErrors.length) {
        itemErrors.forEach((error) => errors.push(`${prefix}：${error}`));
        return;
      }
      if (knownNames.has(name)) {
        updated += 1;
        if (!existingNames.has(name)) warnings.push(`${prefix}与本文件中前面的「${name}」同名，将以后一项更新。`);
      } else {
        created += 1;
        knownNames.add(name);
      }
      items.push({
        name,
        category_id: categoryId,
        description: String(source.description || '').trim(),
        prompt_template: prompt,
        variables: normalisedVariables,
        default_params: { size, n: count, quality },
        requires_input_image: source.requires_input_image !== false,
        is_enabled: source.is_enabled !== false,
        sort: Number.isFinite(Number(source.sort)) ? Number(source.sort) : 0,
      });
    });
    return { errors, warnings, items, existingNames, created, updated };
  }

  function structuredCloneSafe(value) {
    if (typeof structuredClone === 'function') return structuredClone(value);
    return JSON.parse(JSON.stringify(value));
  }

  renderToolbar();
  load();
  return () => {
    state.stopped = true;
    if (state.overlay) state.overlay.close();
  };
}
