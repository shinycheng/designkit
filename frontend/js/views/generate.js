// 商品视觉生成工作台：稳定配置面板 + 局部更新的结果画布。
import { api } from '../api.js';
import { Poller } from '../core/poller.js';
import {
  button,
  closeOverlays,
  downloadUrl,
  field,
  h,
  icon,
  inlineAlert,
  lightbox,
  modal,
  skeleton,
  statusBadge,
  toast,
} from '../ui.js';
import {
  isAcceptedImageFile,
  MAX_UPLOAD_BYTES,
  MAX_UPLOADS,
  QUALITY_OPTIONS,
  SIZE_OPTIONS,
  applySizePresets,
  isUsableSize,
  buildGenerationPayload,
  configSignature,
  deriveSubmitState,
  uploadedItems,
  visibleUploads,
} from './generate/model.js';
import { FIT_MODES, PLATFORM_SIZES, loadImage, renderToBlob } from './generate/export-sizes.js';

const UPLOAD_STATUS = {
  queued: '等待上传',
  uploading: '正在上传',
  uploaded: '已上传',
  failed: '上传失败',
};

let uploadClientId = 0;

const generateSessions = new Map();

function cloneJson(value) {
  if (value == null) return value;
  try { return JSON.parse(JSON.stringify(value)); } catch { return null; }
}

function createGenerateState(snapshot = null) {
  const state = {
    uploads: [],
    templates: [],
    categories: [],
    activeCategory: null,
    selected: undefined,
    selectedCategorySlug: null,
    freePrompt: '',
    extra: '',
    size: '1024x1024',
    n: 1,
    quality: 'high',
    job: null,
    jobSummary: null,
    missingJobId: '',
    selectedResultIndex: 0,
    carriedImages: [],
    submittedSignature: '',
    settingsTouched: false,
    submitting: false,
    submitError: '',
    pollError: '',
    templatesLoading: true,
    templatesError: '',
    mobileView: 'config',
  };

  if (!snapshot) return state;
  state.activeCategory = snapshot.activeCategory ?? null;
  state.selected = snapshot.selection === 'free' ? null : undefined;
  state.selectedCategorySlug = snapshot.selection === 'category' ? snapshot.categorySlug : null;
  state.freePrompt = snapshot.freePrompt || '';
  state.extra = snapshot.extra || '';
  state.size = isUsableSize(snapshot.size) ? snapshot.size : state.size;
  state.n = Math.max(1, Math.min(4, Number.parseInt(snapshot.n, 10) || 1));
  state.quality = QUALITY_OPTIONS.some((option) => option.value === snapshot.quality)
    ? snapshot.quality
    : state.quality;
  state.job = cloneJson(snapshot.job);
  state.jobSummary = cloneJson(snapshot.jobSummary);
  state.missingJobId = snapshot.missingJobId || '';
  state.selectedResultIndex = Math.max(0, Number.parseInt(snapshot.selectedResultIndex, 10) || 0);
  state.submittedSignature = snapshot.submittedSignature || '';
  state.settingsTouched = Boolean(snapshot.settingsTouched);
  state.mobileView = snapshot.mobileView === 'result' ? 'result' : 'config';
  return state;
}

function generateSessionKey(user) {
  return String(user?.id ?? user?.username ?? 'anonymous');
}

function getGenerateSession(user) {
  const key = generateSessionKey(user);
  let session = generateSessions.get(key);
  if (!session) {
    session = {
      snapshot: null,
      pendingSubmission: null,
      uploads: [],
      uploadRunner: null,
      activeUpload: null,
      uploadListeners: new Set(),
      disposed: false,
      epoch: 0,
    };
    generateSessions.set(key, session);
  }
  return session;
}

function releaseUploadPreview(item) {
  if (item.objectUrl && item.previewUrl) URL.revokeObjectURL(item.previewUrl);
  item.objectUrl = false;
}

function notifyUploadSession(session) {
  for (const listener of session.uploadListeners) {
    try { listener(); } catch { /* 已卸载或异常视图不应阻塞上传队列。 */ }
  }
}

function subscribeUploadSession(session, listener) {
  if (session.disposed) return () => {};
  session.uploadListeners.add(listener);
  return () => session.uploadListeners.delete(listener);
}

function runSessionUploadQueue(session) {
  if (session.disposed || session.uploadRunner) return session.uploadRunner;

  const runnerEpoch = session.epoch;
  const runner = (async () => {
    while (!session.disposed && session.epoch === runnerEpoch) {
      const item = session.uploads.find((candidate) => candidate.status === 'queued');
      if (!item) break;

      if (!item.file) {
        item.status = 'failed';
        item.error = '原始图片已不可用，请移除后重新选择';
        notifyUploadSession(session);
        continue;
      }

      const itemSequence = ++item.sequence;
      const controller = new AbortController();
      const activeUpload = { item, itemSequence, controller, epoch: runnerEpoch };
      session.activeUpload = activeUpload;
      item.status = 'uploading';
      item.error = '';
      notifyUploadSession(session);

      const data = new FormData();
      data.append('file', item.file);
      try {
        const uploaded = await api.post('/api/web/uploads', data, { signal: controller.signal });
        if (
          session.disposed
          || session.epoch !== runnerEpoch
          || session.activeUpload !== activeUpload
          || item.sequence !== itemSequence
          || item.status === 'removed'
        ) continue;
        releaseUploadPreview(item);
        Object.assign(item, uploaded, {
          file: null,
          previewUrl: uploaded.url,
          objectUrl: false,
          status: 'uploaded',
          error: '',
        });
      } catch (error) {
        if (
          session.disposed
          || session.epoch !== runnerEpoch
          || session.activeUpload !== activeUpload
          || item.sequence !== itemSequence
          || item.status === 'removed'
        ) continue;
        item.status = 'failed';
        item.error = error?.name === 'AbortError'
          ? '上传已取消，请重试'
          : (error?.message || '上传失败，请重试');
      } finally {
        if (session.activeUpload === activeUpload) session.activeUpload = null;
      }
      notifyUploadSession(session);
    }
  })();

  session.uploadRunner = runner;
  const finishRunner = (error = null) => {
    if (error && !session.disposed && session.activeUpload?.epoch === runnerEpoch) {
      const { item, itemSequence } = session.activeUpload;
      if (item.status === 'uploading' && item.sequence === itemSequence) {
        item.status = 'failed';
        item.error = error?.message || '上传队列异常，请重试';
      }
      session.activeUpload = null;
      notifyUploadSession(session);
    }
    if (session.uploadRunner === runner) session.uploadRunner = null;
    if (!session.disposed && session.uploads.some((item) => item.status === 'queued')) {
      void runSessionUploadQueue(session);
    }
  };
  void runner.then(() => finishRunner(), (error) => finishRunner(error));
  return runner;
}

function disposeGenerateSession(session) {
  if (session.disposed) return;
  session.disposed = true;
  session.epoch += 1;
  session.activeUpload?.controller.abort();
  session.activeUpload = null;
  session.uploadListeners.clear();
  for (const item of session.uploads) {
    item.sequence += 1;
    releaseUploadPreview(item);
    item.file = null;
  }
  session.uploads.length = 0;
  session.pendingSubmission = null;
  session.snapshot = null;
}

function getGenerateState(user) {
  const session = getGenerateSession(user);
  const state = createGenerateState(session.snapshot);
  state.uploads = session.uploads;
  return state;
}

function saveGenerateState(user, state) {
  const selection = state.selected === null
    ? 'free'
    : (state.selected?.slug || state.selectedCategorySlug ? 'category' : 'none');
  getGenerateSession(user).snapshot = {
    activeCategory: state.activeCategory,
    selection,
    categorySlug: state.selected?.slug ?? state.selectedCategorySlug ?? null,
    freePrompt: state.freePrompt,
    extra: state.extra,
    size: state.size,
    n: state.n,
    quality: state.quality,
    job: cloneJson(state.job),
    jobSummary: cloneJson(state.jobSummary),
    missingJobId: state.missingJobId || '',
    selectedResultIndex: state.selectedResultIndex,
    submittedSignature: state.submittedSignature || '',
    settingsTouched: Boolean(state.settingsTouched),
    mobileView: state.mobileView,
  };
}

function setPendingSubmission(user, pendingSubmission) {
  getGenerateSession(user).pendingSubmission = pendingSubmission;
}

function clearPendingSubmission(user, pendingSubmission) {
  const session = generateSessions.get(generateSessionKey(user));
  if (session?.pendingSubmission === pendingSubmission) session.pendingSubmission = null;
}

export function clearGenerateSessions() {
  for (const session of generateSessions.values()) disposeGenerateSession(session);
  generateSessions.clear();
}

export function renderGenerate(container, user) {
  const session = getGenerateSession(user);
  const state = getGenerateState(user);
  // 这些字段属于单次视图挂载，不应在返回页面时保留旧请求或旧错误。
  state.categories = [];
  state.submitting = false;
  state.submitError = '';
  state.pollError = '';
  state.templatesLoading = true;
  state.templatesError = '';

  let disposed = false;
  let lifecycleSequence = 0;
  let submitSequence = 0;
  let unsubscribeUploads = () => {};
  let poller = null;
  let canvasRenderKey = '';
  let lastFailedActionKey = '';

  const configTab = h('button', {
    class: 'dk-mobile-tab is-active',
    type: 'button',
    role: 'tab',
    id: 'dk-generate-config-tab',
    'aria-controls': 'dk-generate-config',
    'aria-selected': 'true',
    onclick: () => setMobileView('config'),
    onkeydown: handleMobileTabKey,
  }, icon('sliders-horizontal', { size: 18 }), '配置');
  const resultTab = h('button', {
    class: 'dk-mobile-tab',
    type: 'button',
    role: 'tab',
    id: 'dk-generate-result-tab',
    'aria-controls': 'dk-generate-result',
    'aria-selected': 'false',
    tabindex: '-1',
    onclick: () => setMobileView('result'),
    onkeydown: handleMobileTabKey,
  }, icon('image', { size: 18 }), '结果');
  const mobileTabs = h('div', {
    class: 'dk-mobile-tabs',
    role: 'tablist',
    'aria-label': '生成工作台视图',
  }, configTab, resultTab);

  const uploadSection = configSection('01', '商品原图', '最多 4 张，商品图会作为生成参考');
  const templateSection = configSection('02', '生成方式', '选一个分类，AI 从该分类的提示词库里挑风格');
  const variablesSection = configSection('03', '画面描述', '自由模式下自己描述画面');
  const settingsSection = configSection('04', '生成设置', '设置成图比例、张数与画质');
  const extraSection = configSection('05', '补充要求', '可选，用于强调构图、色调或禁用内容');

  const configScroll = h('div', { class: 'dk-config-scroll' },
    uploadSection.root,
    templateSection.root,
    variablesSection.root,
    settingsSection.root,
    extraSection.root);

  const actionError = h('div', { class: 'dk-config-action-error' });
  const actionHint = h('p', {
    class: 'dk-config-action-hint dk-config-actions__hint',
    id: 'dk-generate-action-hint',
    role: 'status',
    'aria-live': 'polite',
  });
  const primaryAction = button('请选择生成方式', {
    variant: 'primary',
    size: 'lg',
    className: 'dk-generate-submit',
    type: 'button',
    disabled: true,
    'aria-describedby': 'dk-generate-action-hint',
    onclick: handlePrimaryAction,
  });
  const configActions = h('div', { class: 'dk-config-actions' }, actionError, actionHint, primaryAction);
  const configPanel = h('aside', {
    class: 'dk-config-panel is-mobile-active',
    id: 'dk-generate-config',
    role: 'tabpanel',
    'aria-labelledby': 'dk-generate-config-tab',
    tabindex: '-1',
  }, configScroll, configActions);

  const canvasStatus = h('div', {
    class: 'dk-canvas-status',
    role: 'status',
    'aria-live': 'polite',
    'aria-atomic': 'true',
  });
  const canvasHeader = h('div', { class: 'dk-canvas-header' },
    h('h2', { class: 'dk-canvas-header__title' }, '生成结果'),
    canvasStatus);
  const canvasBody = h('div', {
    class: 'dk-canvas-body',
  });
  const resultCanvas = h('section', {
    class: 'dk-result-canvas dk-canvas',
    id: 'dk-generate-result',
    role: 'tabpanel',
    'aria-labelledby': 'dk-generate-result-tab',
    tabindex: '-1',
  }, canvasHeader, canvasBody);

  const root = h('div', {
    class: 'dk-generate dk-workspace',
    'data-mobile-view': state.mobileView,
  }, mobileTabs, configPanel, resultCanvas);
  container.replaceChildren(root);
  setMobileView(state.mobileView);

  setupUploadSection();
  setupTemplateSection();
  setupSettingsSection();
  setupExtraSection();
  unsubscribeUploads = subscribeUploadSession(session, () => {
    if (disposed) return;
    updateUploadView();
    updatePrimaryAction();
  });
  applyPendingPrompt();  // 灵感库「用它生成」带过来的提示词
  applyPendingReuse();   // 生成记录「继续改」带过来的结果图
  updateVariablesSection();
  updatePrimaryAction();
  updateCanvas(true);
  // 必须等比例清单到位再读系统默认尺寸：loadRuntimeDefaults 会用 SIZE_OPTIONS
  // 校验后端返回的 default_size，清单还是兜底版时会把管理员设的比例静默丢掉
  void loadSizePresets().then(() => { if (!disposed) return loadRuntimeDefaults(); });
  void loadCategories();
  void restoreJobState();

  function applyPendingPrompt() {
    let pending = null;
    try {
      const raw = sessionStorage.getItem('dk_apply_prompt');
      if (raw) { pending = JSON.parse(raw); sessionStorage.removeItem('dk_apply_prompt'); }
    } catch { return; }
    if (!pending?.prompt) return;
    state.selected = null;  // 灵感库接力 → 自由模式，原文照发
    state.selectedCategorySlug = null;
    state.freePrompt = pending.prompt;
    if (pending.from) toast(`已带入灵感库提示词「${pending.from}」，可直接生成或继续修改`, 'success');
    if (pending.requiresImage) toast('这条提示词需要先上传商品/参考图', 'info', 4200);
  }

  /** 接住「生成记录」页点「继续改」带过来的那张结果图。 */
  function applyPendingReuse() {
    let pending = null;
    try {
      const raw = sessionStorage.getItem('dk_reuse_image');
      if (raw) { pending = JSON.parse(raw); sessionStorage.removeItem('dk_reuse_image'); }
    } catch { return; }
    if (!pending?.url) return;
    void reuseResultAsInput({ url: pending.url, format: 'png' }, null, pending.name);
  }

  async function loadRuntimeDefaults() {
    if (user?.role !== 'admin') return;
    try {
      const settings = await api.get('/api/web/settings');
      // 选了具体模板（模板默认优先）或用户手动动过设置才跳过；
      // 灵感库接力进入的自由模式（selected === null）仍应套用系统默认尺寸/张数
      if (disposed || state.settingsTouched || state.selected || state.selectedCategorySlug) return;
      if (isUsableSize(settings.default_size)) state.size = settings.default_size;
      const defaultCount = Number.parseInt(settings.default_n, 10);
      if (Number.isFinite(defaultCount)) state.n = Math.max(1, Math.min(4, defaultCount));
      updateSettingsControls();
      updatePrimaryAction();
    } catch {
      // 普通成员无法读取管理设置；保留安全的网页端默认值。
    }
  }

  function configSection(step, title, description) {
    const body = h('div', { class: 'dk-config-section-body' });
    const root = h('section', { class: 'dk-config-section' },
      h('div', { class: 'dk-config-heading' },
        h('span', { class: 'dk-config-step', 'aria-hidden': 'true' }, step),
        h('div', { class: 'dk-config-heading__copy' },
          h('h3', {}, title),
          h('p', { class: 'dk-config-heading__description' }, description))),
      body);
    return { root, body };
  }

  // ---------------------------------------------------------------- Uploads

  function setupUploadSection() {
    const input = h('input', {
      type: 'file',
      accept: 'image/png,image/jpeg,image/webp,image/heic,image/heif,.heic,.heif',
      multiple: true,
      hidden: true,
      onchange: (event) => {
        addFiles([...event.target.files]);
        event.target.value = '';
      },
    });
    const dropTitle = h('span', { class: 'dk-upload-title' });
    const dropMeta = h('span', { class: 'dk-upload-meta dk-upload-dropzone__meta' });
    const dropzone = h('button', {
      class: 'dk-upload-dropzone',
      type: 'button',
      onclick: () => input.click(),
      ondragenter: (event) => {
        event.preventDefault();
        dropzone.classList.add('is-dragover');
      },
      ondragover: (event) => {
        event.preventDefault();
        if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
      },
      ondragleave: (event) => {
        if (!dropzone.contains(event.relatedTarget)) dropzone.classList.remove('is-dragover');
      },
      ondrop: (event) => {
        event.preventDefault();
        dropzone.classList.remove('is-dragover');
        addFiles([...event.dataTransfer.files]);
      },
    },
      h('span', { class: 'dk-upload-dropzone__icon', 'aria-hidden': 'true' }, icon('upload-cloud', { size: 22 })),
      h('span', { class: 'dk-upload-copy' }, dropTitle, dropMeta));
    const list = h('div', { class: 'dk-upload-list', role: 'list', 'aria-label': '待生成商品图' });

    uploadSection.body.append(input, dropzone, list);
    uploadSection.input = input;
    uploadSection.dropzone = dropzone;
    uploadSection.dropTitle = dropTitle;
    uploadSection.dropMeta = dropMeta;
    uploadSection.list = list;
    updateUploadView();
  }

  function addFiles(files) {
    const imageFiles = files.filter((file) => file instanceof File);
    if (!imageFiles.length) return;

    let remaining = MAX_UPLOADS - visibleUploads(state).length;
    if (remaining <= 0) {
      toast('最多上传 4 张商品图', 'error');
      return;
    }

    for (const file of imageFiles) {
      if (remaining <= 0) {
        toast('最多上传 4 张商品图，多余文件未加入', 'error');
        break;
      }
      remaining -= 1;
      const item = {
        clientId: `upload-${Date.now()}-${++uploadClientId}`,
        id: null,
        file,
        original_name: file.name || '未命名图片',
        previewUrl: URL.createObjectURL(file),
        objectUrl: true,
        status: 'queued',
        error: '',
        sequence: 0,
      };

      if (!isAcceptedImageFile(file)) {
        item.status = 'failed';
        item.error = '仅支持 PNG、JPG、WEBP';
      } else if (file.size > MAX_UPLOAD_BYTES) {
        item.status = 'failed';
        item.error = '单张图片不能超过 20MB';
      } else if (!file.size) {
        item.status = 'failed';
        item.error = '图片内容为空';
      }
      state.uploads.push(item);
    }

    state.submitError = '';
    notifyUploadSession(session);
    void runSessionUploadQueue(session);
  }

  function retryUpload(item) {
    if (item.status !== 'failed' || !item.file) return;
    item.sequence += 1;
    item.status = 'queued';
    item.error = '';
    notifyUploadSession(session);
    void runSessionUploadQueue(session);
  }

  function removeUpload(item) {
    item.sequence += 1;
    item.status = 'removed';
    if (session.activeUpload?.item === item) session.activeUpload.controller.abort();
    releaseUploadPreview(item);
    item.file = null;
    const index = session.uploads.indexOf(item);
    if (index >= 0) session.uploads.splice(index, 1);
    notifyUploadSession(session);
  }

  function updateUploadView() {
    const items = visibleUploads(state);
    const remaining = Math.max(0, MAX_UPLOADS - items.length);
    uploadSection.dropzone.classList.toggle('is-compact', items.length > 0);
    uploadSection.dropzone.disabled = remaining === 0;
    uploadSection.dropTitle.textContent = items.length ? '继续添加商品图' : '点击或拖拽上传商品图';
    uploadSection.dropMeta.textContent = remaining
      ? `还可上传 ${remaining} 张 · PNG / JPG / WEBP / HEIC · 单张 20MB`
      : '已达到 4 张上限，移除后可继续添加';

    uploadSection.list.replaceChildren(...items.map((item) => {
      const preview = h('button', {
        class: 'dk-upload-preview',
        type: 'button',
        'aria-label': `预览 ${item.original_name}`,
        onclick: () => lightbox(item.previewUrl, { alt: item.original_name }),
      }, h('img', {
        class: 'dk-upload-item__image',
        src: item.previewUrl,
        alt: item.original_name,
        width: '64',
        height: '64',
      }));
      const statusClass = item.status === 'failed' ? ' is-failed' : '';
      const retry = item.status === 'failed' && item.file
        ? button('重试', {
          variant: 'quiet', size: 'sm', iconName: 'rotate-ccw', type: 'button', onclick: () => retryUpload(item),
        })
        : null;
      const remove = h('button', {
        class: 'dk-upload-remove dk-upload-item__remove',
        type: 'button',
        'aria-label': `移除 ${item.original_name}`,
        onclick: () => removeUpload(item),
      }, icon('x', { size: 18 }));

      return h('div', { class: `dk-upload-item${statusClass}`, role: 'listitem' },
        preview,
        h('div', { class: 'dk-upload-info' },
          h('strong', { title: item.original_name }, item.original_name),
          h('span', { class: 'dk-upload-status dk-upload-item__status' },
            item.status === 'uploading' ? icon('loader-circle', { size: 14, className: 'dk-spin' }) : null,
            item.error || UPLOAD_STATUS[item.status] || item.status)),
        h('div', { class: 'dk-upload-actions' }, retry, remove));
    }));
  }

  // --------------------------------------------------------------- Templates

  function setupTemplateSection() {
    // 「更多分类」的折叠开关：上游 11 个分类里有一半（漫画分镜、游戏素材…）
    // 拿来出商品图纯属误导，默认只露出电商能用的那几个
    const categoryList = h('div', {
      class: 'dk-template-categories',
      role: 'group',
      'aria-label': '更多分类',
    });
    const grid = h('div', {
      class: 'dk-template-grid',
      role: 'listbox',
      'aria-label': '生成方式',
      onkeydown: handleTemplateGridKey,
    });
    const summary = h('div', { class: 'dk-template-summary' });

    templateSection.body.append(grid, categoryList, summary);
    templateSection.categoryList = categoryList;
    templateSection.grid = grid;
    templateSection.summary = summary;
    renderTemplateLoading();
  }

  function renderTemplateLoading() {
    templateSection.categoryList.replaceChildren();
    templateSection.grid.replaceChildren(
      skeleton({ lines: 2, className: 'dk-template-skeleton' }),
      skeleton({ lines: 2, className: 'dk-template-skeleton' }));
    templateSection.summary.replaceChildren();
  }

  async function loadCategories() {
    const sequence = lifecycleSequence;
    state.templatesLoading = true;
    state.templatesError = '';
    renderTemplateLoading();
    try {
      const data = await api.get('/api/web/generate-categories');
      if (disposed || sequence !== lifecycleSequence) return;
      state.categories = Array.isArray(data?.categories) ? data.categories : [];
      // 恢复快照里选中的分类：切页回来时它可能还没加载完
      if (state.selectedCategorySlug) {
        const found = state.categories.find((c) => c.slug === state.selectedCategorySlug);
        state.selected = found || undefined;
        state.selectedCategorySlug = found ? found.slug : null;
      }
      state.templatesLoading = false;
      renderTemplateOptions();
      updateSettingsControls();
      updatePrimaryAction();
    } catch (error) {
      if (disposed || sequence !== lifecycleSequence) return;
      state.templatesLoading = false;
      state.templatesError = error?.message || '无法加载生成方式';
      templateSection.grid.replaceChildren(
        inlineAlert(state.templatesError, 'error'),
        button('重试', {
          variant: 'secondary', size: 'sm', type: 'button', onclick: () => void loadCategories(),
        }));
      updatePrimaryAction();
    }
  }

  function renderTemplateOptions() {
    const main = state.categories.filter((c) => !c.collapsed);
    const extra = state.categories.filter((c) => c.collapsed);

    const freeMode = categoryCard(null);
    templateSection.grid.replaceChildren(freeMode, ...main.map(categoryCard));

    templateSection.categoryList.replaceChildren();
    if (extra.length) {
      const extraGridId = 'dk-more-categories-grid';
      const selectedExtra = Boolean(state.selected
        && extra.some((category) => category.slug === state.selected.slug));
      const extraGrid = h('div', {
        id: extraGridId,
        class: 'dk-template-grid dk-template-grid--extra',
        role: 'listbox',
        'aria-label': '更多生成分类',
        hidden: !selectedExtra,
        onkeydown: handleTemplateGridKey,
      },
        ...extra.map(categoryCard));
      const toggle = button(`更多分类（${extra.length}）`, {
        variant: 'quiet', size: 'sm', type: 'button', iconName: 'chevron-down',
        className: 'dk-more-categories-toggle',
        'aria-controls': extraGridId,
        onclick: () => {
          extraGrid.hidden = !extraGrid.hidden;
          toggle.setAttribute('aria-expanded', String(!extraGrid.hidden));
        },
      });
      toggle.setAttribute('aria-expanded', String(selectedExtra));
      templateSection.categoryList.append(toggle, extraGrid);
    }
    updateTemplateSelection();
  }

  function categoryCard(category) {
    const isFree = category === null;
    const key = isFree ? 'free' : category.slug;
    const title = isFree ? '自由模式' : category.name;
    const meta = isFree
      ? '自己描述画面，不参考提示词库'
      : `${category.count} 条提示词可参考`;

    return h('button', {
      class: 'dk-template-card',
      type: 'button',
      role: 'option',
      'data-category-slug': key,
      'aria-selected': 'false',
      onclick: () => selectCategory(category),
    },
      h('span', { class: 'dk-template-icon dk-template-card__media', 'aria-hidden': 'true' },
        icon(isFree ? 'pencil' : 'layout-template', { size: 22 })),
      h('span', { class: 'dk-template-card-copy' },
        h('strong', { class: 'dk-template-card__title' }, title),
        h('span', { class: 'dk-template-card__description' }, meta)),
      h('span', { class: 'dk-template-check', 'aria-hidden': 'true' }, icon('check', { size: 16 })));
  }

  function selectCategory(category) {
    state.selected = category;
    state.selectedCategorySlug = category ? category.slug : null;
    state.submitError = '';
    updateTemplateSelection();
    updatePrimaryAction();
  }

  function updateTemplateSelection() {
    const selectedKey = state.selected === null
      ? 'free'
      : (state.selected ? state.selected.slug : '');
    for (const control of document.querySelectorAll('[data-category-slug]')) {
      const selected = control.dataset.categorySlug === selectedKey;
      control.classList.toggle('is-selected', selected);
      control.setAttribute('aria-selected', String(selected));
    }

    if (state.selected === undefined) {
      templateSection.summary.replaceChildren(
        h('p', { class: 'dk-section-note' },
          '选一个分类：出图时 AI 会先看你的商品图，再从该分类的提示词库里挑几条最搭的风格，'
          + '结合你的补充要求现场写提示词。'));
    } else if (state.selected === null) {
      templateSection.summary.replaceChildren(
        h('div', { class: 'dk-selected-template' },
          icon('pencil', { size: 18 }),
          h('div', {}, h('strong', {}, '自由模式'), h('p', {}, '完全按你写的描述出图，不参考提示词库。'))));
    } else {
      templateSection.summary.replaceChildren(
        h('div', { class: 'dk-selected-template' },
          icon('check-circle-2', { size: 18 }),
          h('div', {},
            h('strong', {}, `已选择：${state.selected.name}`),
            h('p', {}, `AI 会从这 ${state.selected.count} 条提示词里挑最适合你商品的几条作为风格参考。`))));
    }
  }

  function updateVariablesSection() {
    // 按分类生成没有「模板变量」这回事，这一段只在自由模式下用来写画面描述
    variablesSection.root.hidden = state.selected !== null;
    variablesSection.body.replaceChildren();
    if (state.selected !== null) return;

    const prompt = h('textarea', {
      class: 'dk-textarea textarea',
      id: 'dk-free-prompt',
      rows: '4',
      value: state.freePrompt,
      placeholder: '例如：把商品放在明亮的木质桌面上，柔和自然光，背景干净，不出现文字',
      oninput: (event) => {
        state.freePrompt = event.target.value;
        state.submitError = '';
        updatePrimaryAction();
      },
    });
    variablesSection.body.append(field('画面描述', prompt, {
      help: '越具体越好：场景、光线、角度、色调。商品本身不用描述，AI 会看图。',
    }));
  }

  function handleTemplateGridKey(event) {
    if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(event.key)) return;
    const grid = event.currentTarget?.matches?.('[role="listbox"]')
      ? event.currentTarget
      : templateSection.grid;
    const controls = [...grid.querySelectorAll('[role="option"]:not([hidden]):not([disabled])')];
    if (!controls.length) return;
    event.preventDefault();
    const current = Math.max(0, controls.indexOf(document.activeElement));
    const columnCount = Math.max(1, getComputedStyle(grid).gridTemplateColumns.split(' ').length);
    let next = current;
    if (event.key === 'ArrowLeft') next = Math.max(0, current - 1);
    if (event.key === 'ArrowRight') next = Math.min(controls.length - 1, current + 1);
    if (event.key === 'ArrowUp') next = Math.max(0, current - columnCount);
    if (event.key === 'ArrowDown') next = Math.min(controls.length - 1, current + columnCount);
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = controls.length - 1;
    controls[next].focus();
  }

  function moveRovingFocus(event, controls) {
    if (!controls.length) return;
    event.preventDefault();
    const current = Math.max(0, controls.indexOf(document.activeElement));
    let next = current;
    if (event.key === 'ArrowLeft') next = (current - 1 + controls.length) % controls.length;
    if (event.key === 'ArrowRight') next = (current + 1) % controls.length;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = controls.length - 1;
    controls[next].focus();
  }

  // ---------------------------------------------------------- Generate setup

  function setupSettingsSection() {
    const sizeGroup = h('div', {
      class: 'dk-size-options',
      role: 'group',
      'aria-label': '图片尺寸',
    });

    const countValue = h('output', { class: 'dk-count-value', for: 'dk-count-minus dk-count-plus' }, String(state.n));
    const countStepper = h('div', { class: 'dk-count-stepper', role: 'group', 'aria-label': '生成张数' },
      h('button', {
        class: 'dk-stepper-button', id: 'dk-count-minus', type: 'button', 'aria-label': '减少生成张数',
        onclick: () => changeCount(-1),
      }, h('span', { 'aria-hidden': 'true' }, '−')),
      countValue,
      h('button', {
        class: 'dk-stepper-button', id: 'dk-count-plus', type: 'button', 'aria-label': '增加生成张数',
        onclick: () => changeCount(1),
      }, icon('plus', { size: 18 })));
    const quality = h('select', {
      class: 'dk-select select',
      id: 'dk-generate-quality',
      value: state.quality,
      onchange: (event) => {
        state.quality = event.target.value;
        state.submitError = '';
        updatePrimaryAction();
      },
    }, QUALITY_OPTIONS.map((option) => h('option', {
      value: option.value,
      selected: state.quality === option.value,
    }, option.label)));

    const compactSettings = h('div', { class: 'dk-generate-settings-row' },
      field('生成张数', countStepper, { help: '每次 1–4 张' }),
      field('画质', quality, { help: '推荐使用高画质' }));
    settingsSection.body.append(
      h('div', { class: 'dk-field-group' }, h('p', { class: 'dk-field-label' }, '图片尺寸'), sizeGroup),
      compactSettings);
    settingsSection.sizeGroup = sizeGroup;
    renderSizeOptions();
    settingsSection.countValue = countValue;
    settingsSection.countMinus = countStepper.querySelector('#dk-count-minus');
    settingsSection.countPlus = countStepper.querySelector('#dk-count-plus');
    settingsSection.quality = quality;
  }

  function changeCount(delta) {
    state.settingsTouched = true;
    state.n = Math.max(1, Math.min(4, state.n + delta));
    state.submitError = '';
    updateSettingsControls();
    updatePrimaryAction();
  }

  // 比例选项由后端下发，所以要能重画一次——不能在建面板时一次性写死
  function renderSizeOptions() {
    if (!settingsSection.sizeGroup) return;
    const options = SIZE_OPTIONS.slice();
    // 当前尺寸不在推荐清单里时（管理员设了自定义合法尺寸），补一个临时选项，
    // 否则一个按钮都不高亮，用户看不出现在到底是什么比例
    if (state.size && !options.some((option) => option.value === state.size)) {
      options.push({ value: state.size, label: '自定义', ratio: state.size, usage: '来自系统或模板设置' });
    }
    settingsSection.sizeGroup.replaceChildren(...options.map((option) => h('button', {
      // 选中态在这里就地写好：本函数会在面板其余控件就绪之前被调用一次，
      // 此时不能去碰 updateSettingsControls（它要读 countValue 等还没赋值的引用）
      class: `dk-size-option${state.size === option.value ? ' is-selected' : ''}`,
      type: 'button',
      'data-size': option.value,
      'data-ratio': option.ratio,
      'aria-pressed': String(state.size === option.value),
      onclick: () => {
        state.settingsTouched = true;
        state.size = option.value;
        state.submitError = '';
        updateSettingsControls();
        updatePrimaryAction();
      },
    },
      h('span', {
        class: `dk-ratio-shape dk-size-option__shape is-${String(option.ratio).replace(':', '-')}`,
        'aria-hidden': 'true',
      }),
      h('span', {}, h('strong', {}, `${option.label} ${option.ratio}`), h('small', {}, option.usage)))));
  }

  async function loadSizePresets() {
    try {
      const data = await api.get('/api/web/size-presets');
      if (disposed) return;
      applySizePresets(data?.presets);
      renderSizeOptions();
    } catch {
      // 拉不到就用内置兜底清单，不影响生成
    }
  }

  function updateSettingsControls() {
    for (const control of settingsSection.sizeGroup.querySelectorAll('[data-size]')) {
      const selected = control.dataset.size === state.size;
      control.classList.toggle('is-selected', selected);
      control.setAttribute('aria-pressed', String(selected));
    }
    settingsSection.countValue.value = state.n;
    settingsSection.countValue.textContent = String(state.n);
    settingsSection.countMinus.disabled = state.n <= 1;
    settingsSection.countPlus.disabled = state.n >= 4;
    settingsSection.quality.value = state.quality;
  }

  function setupExtraSection() {
    const textarea = h('textarea', {
      class: 'dk-textarea textarea',
      id: 'dk-extra-instructions',
      rows: '3',
      value: state.extra,
      placeholder: '例如：整体偏暖色；商品占画面约 60%；背景不要出现文字',
      oninput: (event) => {
        state.extra = event.target.value;
        state.submitError = '';
        updatePrimaryAction(); // 补充要求也是配置的一部分，改了要重新判断能否重试
      },
    });
    const details = h('details', { class: 'dk-extra-requirements' },
      h('summary', {}, icon('chevron-right', { size: 18 }), '添加补充要求'),
      field('补充要求', textarea, { help: '可留空；尽量使用清晰、可执行的短句。' }));
    extraSection.body.append(details);
  }

  // ------------------------------------------------------------ Submission

  function updatePrimaryAction() {
    const derived = deriveSubmitState(state);
    primaryAction.disabled = !derived.canSubmit;
    primaryAction.setAttribute('aria-disabled', String(!derived.canSubmit));
    primaryAction.classList.toggle('is-loading', state.submitting);
    primaryAction.replaceChildren(
      icon(state.submitting ? 'loader-circle' : (derived.action === 'retry' ? 'rotate-ccw' : 'sparkles'), {
        size: 18,
        className: state.submitting ? 'dk-spin' : '',
      }),
      document.createTextNode(derived.label));
    // 不能生成时把原因显示出来，否则用户只看到一个灰按钮而不知道缺什么
    actionHint.textContent = derived.canSubmit
      ? `${state.n} 张 · ${formatSize(state.size)} · ${formatQuality(state.quality)}画质`
      : (derived.reason || '');
    actionError.replaceChildren(...(state.submitError ? [inlineAlert(state.submitError, 'error')] : []));
    // 失败面板的文案与按钮同样取决于「配置有没有改过」，需要跟着变化。
    // 只在 retry/submit 真正切换时重建，否则每敲一个字都会重绘面板、丢失焦点。
    const failedActionKey = state.job?.status === 'failed' ? `${derived.action}|${derived.label}` : '';
    if (failedActionKey !== lastFailedActionKey) {
      lastFailedActionKey = failedActionKey;
      if (failedActionKey) updateCanvas(true);
    }
  }

  function handlePrimaryAction() {
    const derived = deriveSubmitState(state);
    if (!derived.canSubmit) {
      toast(derived.reason, 'error');
      return;
    }
    if (derived.action === 'retry') void retryJob();
    else void submitGeneration();
  }

  async function submitGeneration() {
    const derived = deriveSubmitState(state);
    if (!derived.canSubmit || state.submitting || disposed) return;

    const sequence = ++submitSequence;
    const lifecycle = lifecycleSequence;
    state.submitting = true;
    state.submitError = '';
    state.missingJobId = '';
    state.jobSummary = captureJobSummary();
    updatePrimaryAction();
    updateCanvas(true);

    const signature = configSignature(state);
    const pendingSubmission = {
      request: api.post('/api/web/generations', buildGenerationPayload(state)),
      signature,
      jobSummary: cloneJson(state.jobSummary),
      failureMessage: '创建生成任务失败',
    };
    setPendingSubmission(user, pendingSubmission);

    try {
      const job = await pendingSubmission.request;
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      clearPendingSubmission(user, pendingSubmission);
      state.carriedImages = [];
      state.job = job;
      state.submittedSignature = signature;
      state.pollError = '';
      state.selectedResultIndex = 0;
      setMobileView('result', window.matchMedia('(max-width: 959px)').matches);
      updateCanvas(true);
      startPolling();
    } catch (error) {
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      clearPendingSubmission(user, pendingSubmission);
      state.submitError = error?.message || '创建生成任务失败';
      toast(state.submitError, 'error', 5000);
    } finally {
      if (!disposed && lifecycle === lifecycleSequence && sequence === submitSequence) {
        state.submitting = false;
        updatePrimaryAction();
        updateCanvas(true);
      }
    }
  }

  async function retryJob() {
    if (!state.job?.job_id || state.submitting || disposed) return;
    const sequence = ++submitSequence;
    const lifecycle = lifecycleSequence;
    const jobId = state.job.job_id;
    state.submitting = true;
    state.submitError = '';
    updatePrimaryAction();
    updateCanvas(true);

    const pendingSubmission = {
      request: api.post(`/api/web/generations/${jobId}/retry`),
      signature: state.submittedSignature,
      jobSummary: cloneJson(state.jobSummary),
      failureMessage: '重新生成失败',
    };
    setPendingSubmission(user, pendingSubmission);

    try {
      const job = await pendingSubmission.request;
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      clearPendingSubmission(user, pendingSubmission);
      state.carriedImages = [];
      state.job = job;
      state.pollError = '';
      setMobileView('result', window.matchMedia('(max-width: 959px)').matches);
      updateCanvas(true);
      startPolling();
    } catch (error) {
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      clearPendingSubmission(user, pendingSubmission);
      state.submitError = error?.message || '重新生成失败';
      toast(state.submitError, 'error', 5000);
    } finally {
      if (!disposed && lifecycle === lifecycleSequence && sequence === submitSequence) {
        state.submitting = false;
        updatePrimaryAction();
        updateCanvas(true);
      }
    }
  }

  function captureJobSummary() {
    return {
      templateName: state.selected ? state.selected.name : '自由模式',
      size: state.size,
      n: state.n,
      quality: state.quality,
      inputs: uploadedItems(state).map((item) => ({
        url: item.previewUrl || item.url,
        name: item.original_name,
      })),
    };
  }

  // --------------------------------------------------------------- Polling

  async function restoreJobState() {
    const pendingSubmission = getGenerateSession(user).pendingSubmission;
    if (pendingSubmission) {
      const sequence = lifecycleSequence;
      state.submitting = true;
      state.jobSummary = cloneJson(pendingSubmission.jobSummary) || state.jobSummary;
      updatePrimaryAction();
      updateCanvas(true);
      try {
        const job = await pendingSubmission.request;
        if (disposed || sequence !== lifecycleSequence) return;
        clearPendingSubmission(user, pendingSubmission);
        state.job = job;
        state.submittedSignature = pendingSubmission.signature || state.submittedSignature;
        state.missingJobId = '';
        state.pollError = '';
        state.selectedResultIndex = 0;
        setMobileView('result', window.matchMedia('(max-width: 959px)').matches);
        updateCanvas(true);
        startPolling();
      } catch (error) {
        if (disposed || sequence !== lifecycleSequence) return;
        clearPendingSubmission(user, pendingSubmission);
        state.submitError = error?.message || pendingSubmission.failureMessage;
        toast(state.submitError, 'error', 5000);
      } finally {
        if (!disposed && sequence === lifecycleSequence) {
          state.submitting = false;
          updatePrimaryAction();
          updateCanvas(true);
        }
      }
      return;
    }

    const jobId = state.job?.job_id;
    if (!jobId || state.missingJobId) return;
    const sequence = lifecycleSequence;
    const submissionSequence = submitSequence;

    try {
      const job = await api.get(`/api/web/generations/${jobId}`);
      if (
        disposed
        || sequence !== lifecycleSequence
        || submissionSequence !== submitSequence
        || state.job?.job_id !== jobId
      ) return;
      state.job = job;
      state.missingJobId = '';
      state.pollError = '';
      updatePrimaryAction();
      updateCanvas(true);
      if (job.status === 'pending' || job.status === 'processing') startPolling();
    } catch (error) {
      if (
        disposed
        || sequence !== lifecycleSequence
        || submissionSequence !== submitSequence
        || state.job?.job_id !== jobId
      ) return;
      if (error?.status === 404) {
        state.missingJobId = jobId;
        state.job = null;
        state.pollError = '';
      } else if (error?.status !== 401) {
        state.pollError = error?.message || '暂时无法刷新任务状态';
        if (state.job?.status === 'pending' || state.job?.status === 'processing') startPolling();
      }
      updatePrimaryAction();
      updateCanvas(true);
    }
  }

  function startPolling() {
    stopPolling();
    const jobId = state.job?.job_id;
    if (!jobId) return;

    poller = new Poller(async ({ signal }) => {
      const job = await api.get(`/api/web/generations/${jobId}`, { signal });
      if (signal.aborted) {
        const abortError = new Error('Polling aborted');
        abortError.name = 'AbortError';
        throw abortError;
      }
      return job;
    }, {
      interval: 2000,
      hiddenInterval: 12000,
      maxInterval: 30000,
      onResult: (job) => {
        state.job = job;
        state.pollError = '';
        updatePrimaryAction();
        updateCanvas();
        if (job.status === 'succeeded') {
          toast('商品图生成完成', 'success');
          return false;
        }
        return job.status !== 'failed';
      },
      onError: (error) => {
        if (error?.status === 404) {
          state.missingJobId = jobId;
          state.job = null;
          state.pollError = '';
          updatePrimaryAction();
          updateCanvas(true);
          return false;
        }
        if (error?.status === 401) return false;
        state.pollError = error?.message || '暂时无法刷新任务状态';
        updateCanvas();
        return true;
      },
    });
    poller.start();
  }

  function stopPolling() {
    if (!poller) return;
    poller.dispose();
    poller = null;
  }

  // --------------------------------------------------------- Result canvas

  function currentCanvasState() {
    if (state.missingJobId) return 'missing';
    if (!state.job) return 'none';
    if (['pending', 'processing', 'succeeded', 'failed'].includes(state.job.status)) return state.job.status;
    return 'missing';
  }

  function updateCanvas(force = false) {
    const canvasState = currentCanvasState();
    const imageKey = (state.job?.images || []).map((image) => image.id || image.url).join(',');
    const key = [
      canvasState,
      state.job?.job_id || state.missingJobId,
      state.job?.error || '',
      imageKey,
      state.pollError,
      state.submitting,
    ].join('|');
    if (!force && key === canvasRenderKey) return;
    canvasRenderKey = key;
    resultCanvas.dataset.state = canvasState;
    canvasStatus.replaceChildren(canvasState === 'none'
      ? h('span', { class: 'dk-neutral-status' }, '等待配置')
      : (canvasState === 'missing'
        ? h('span', { class: 'dk-neutral-status is-error' }, '任务不可用')
        : statusBadge(canvasState)));

    if (canvasState === 'none') canvasBody.replaceChildren(renderNoneState());
    if (canvasState === 'pending' || canvasState === 'processing') canvasBody.replaceChildren(renderProgressState(canvasState));
    if (canvasState === 'failed') canvasBody.replaceChildren(renderFailedState());
    if (canvasState === 'missing') canvasBody.replaceChildren(renderMissingState());
    if (canvasState === 'succeeded') canvasBody.replaceChildren(renderSucceededState());
    canvasBody.scrollTop = 0;
  }

  function renderNoneState() {
    const action = button('开始配置', {
      variant: 'secondary', size: 'md', iconName: 'sliders-horizontal', type: 'button', onclick: focusConfiguration,
    });
    return h('div', { class: 'dk-result-state is-empty' },
      h('span', { class: 'dk-result-state__icon', 'aria-hidden': 'true' }, icon('image', { size: 28 })),
      h('h2', { class: 'dk-result-state__title' }, '结果会显示在这里'),
      h('p', { class: 'dk-result-state__description' }, '上传商品图，选一个分类并设置参数后，生成结果会持续显示在这块画布中。'),
      action);
  }

  function renderProgressState(status) {
    const summary = effectiveJobSummary();
    const inputStrip = summary.inputs.length
      ? h('div', { class: 'dk-job-inputs', 'aria-label': '本次任务输入图片' },
        ...summary.inputs.map((item, index) => h('img', {
          src: item.url,
          alt: item.name || `输入商品图 ${index + 1}`,
          width: '56',
          height: '56',
        })))
      : null;
    return h('div', { class: `dk-result-state is-${status}` },
      h('div', { class: 'dk-progress-hero' },
        h('span', { class: 'dk-progress-icon', 'aria-hidden': 'true' },
          icon(status === 'pending' ? 'clock' : 'wand-sparkles', { size: 28 })),
        h('div', {},
          h('h3', {}, status === 'pending' ? '任务正在排队' : '正在生成商品图'),
          h('p', {}, status === 'pending'
            ? '任务已创建，开始处理后会自动更新。'
            : '可以离开当前页面，生成完成后仍可在生成记录中查看。'))),
      state.pollError ? inlineAlert('网络暂时不稳定，状态将自动重试刷新。', 'warning') : null,
      h('div', { class: 'dk-job-summary' },
        h('div', { class: 'dk-job-summary-copy dk-job-summary__copy' },
          h('dl', {},
            summaryRow('生成方式', summary.templateName),
            summaryRow('图片尺寸', formatSize(summary.size)),
            summaryRow('预计产出', `${summary.n} 张 · ${formatQuality(summary.quality)}`)),
          inputStrip),
        h('div', { class: 'dk-job-progress-preview' }, skeleton({ lines: 4, className: 'dk-canvas-skeleton' }))));
  }

  function renderFailedState() {
    const error = state.job?.error || '生成服务没有返回具体原因。';
    const derived = deriveSubmitState(state);
    // 配置改过之后这里会变成「按新配置生成」，避免用户以为改了设置却仍重跑旧任务
    const isRetry = derived.action === 'retry';
    return h('div', { class: 'dk-result-state is-failed' },
      h('div', { class: 'dk-result-message' },
        h('span', { class: 'dk-result-message-icon', 'aria-hidden': 'true' }, icon('circle-alert', { size: 30 })),
        h('div', {}, h('h3', {}, '这次没有生成成功'),
          h('p', {}, isRetry ? '输入配置已保留，可以直接重试。' : '左侧配置已改动，将按新的配置重新生成。'))),
      inlineAlert(error, 'error'),
      h('ul', { class: 'dk-recovery-list' },
        h('li', {}, '检查商品图是否清晰且内容完整'),
        h('li', {}, '减少互相冲突的补充要求'),
        h('li', {}, '如果服务繁忙，请稍后重试')),
      button(state.submitting ? '正在提交…' : (isRetry ? '重新生成' : derived.label), {
        variant: 'primary',
        size: 'lg',
        iconName: isRetry ? 'rotate-ccw' : 'sparkles',
        loading: state.submitting,
        disabled: state.submitting || !derived.canSubmit,
        disabledReason: derived.canSubmit ? '' : derived.reason,
        type: 'button',
        onclick: handlePrimaryAction,
      }));
  }

  function renderMissingState() {
    return h('div', { class: 'dk-result-state is-missing' },
      h('span', { class: 'dk-result-message-icon', 'aria-hidden': 'true' }, icon('circle-help', { size: 32 })),
      h('h3', {}, '找不到这个生成任务'),
      h('p', {}, '任务可能已被删除，或者当前账号没有查看权限。已有配置仍然保留。'),
      button('返回新任务', {
        variant: 'primary', size: 'lg', iconName: 'plus', type: 'button', onclick: resetMissingJob,
      }));
  }

  function renderSucceededState() {
    // 补图产生的是一个新任务，但上一批已出好的图要一起显示，
    // 否则用户会以为「补图把原来的图弄没了」，进而去重跑整单——正好是想省的那笔钱
    const fresh = Array.isArray(state.job?.images) ? state.job.images : [];
    const carried = Array.isArray(state.carriedImages) ? state.carriedImages : [];
    const images = carried.length ? carried.concat(fresh) : fresh;
    if (!images.length) {
      return h('div', { class: 'dk-result-state is-missing' },
        inlineAlert('任务已完成，但没有可展示的结果图。', 'warning'),
        button('再生成一次', { variant: 'primary', iconName: 'rotate-ccw', type: 'button', onclick: () => void retryJob() }));
    }

    state.selectedResultIndex = Math.min(state.selectedResultIndex, images.length - 1);
    const summary = effectiveJobSummary();
    const selected = () => images[state.selectedResultIndex];
    const previewImage = h('img', {
      src: selected().url,
      alt: `生成结果 ${state.selectedResultIndex + 1}`,
      width: selected().width || null,
      height: selected().height || null,
    });
    const previewLabel = h('strong', { class: 'dk-result-selection' },
      `结果 ${state.selectedResultIndex + 1} / ${images.length}`);
    const previewDimensions = h('span', {}, selected().width && selected().height
      ? `${selected().width} × ${selected().height}`
      : formatSize(summary.size));
    const selectionAnnouncement = h('span', {
      class: 'dk-visually-hidden',
      role: 'status',
      'aria-live': 'polite',
      'aria-atomic': 'true',
    });
    const previewButton = h('button', {
      class: 'dk-result-stage__media',
      type: 'button',
      'aria-label': `查看结果 ${state.selectedResultIndex + 1} 大图`,
      onclick: () => lightbox(selected().url, {
        alt: `生成结果 ${state.selectedResultIndex + 1}`,
        filename: resultFilename(selected(), state.selectedResultIndex),
      }),
    }, previewImage, h('span', { class: 'dk-preview-zoom' },
      icon('eye', { size: 18 }), h('span', { class: 'dk-preview-zoom__label' }, '查看大图')));
    const download = button(`下载结果 ${state.selectedResultIndex + 1}`, {
      variant: 'primary',
      size: 'md',
      iconName: 'download',
      className: 'dk-result-download',
      type: 'button',
      onclick: () => {
        downloadUrl(selected().url, resultFilename(selected(), state.selectedResultIndex));
        toast(`已开始下载结果 ${state.selectedResultIndex + 1}`, 'success');
      },
    });
    // 电商修图的主循环是「在满意的那一版上继续改」，不是每次从原图重赌。
    // 没有这个入口时，用户只能下载到本地再拖回上传区，等于把上一版的构图光影全丢掉。
    const continueEdit = button('用这张继续改', {
      variant: 'secondary',
      size: 'md',
      iconName: 'wand-sparkles',
      type: 'button',
      onclick: () => void reuseResultAsInput(selected(), continueEdit),
    });

    const exportSized = button('导出平台尺寸', {
      variant: 'quiet',
      size: 'md',
      iconName: 'grid-2x2',
      type: 'button',
      onclick: () => openExportDialog(selected(), state.selectedResultIndex),
    });

    let thumbnailControls = [];

    function selectResult(index, { focus = false, announce = true } = {}) {
      const nextIndex = (index + images.length) % images.length;
      const image = images[nextIndex];
      state.selectedResultIndex = nextIndex;
      previewImage.src = image.url;
      previewImage.alt = `生成结果 ${nextIndex + 1}`;
      if (image.width) previewImage.setAttribute('width', image.width);
      else previewImage.removeAttribute('width');
      if (image.height) previewImage.setAttribute('height', image.height);
      else previewImage.removeAttribute('height');
      previewButton.setAttribute('aria-label', `查看结果 ${nextIndex + 1} 大图`);
      previewLabel.textContent = `结果 ${nextIndex + 1} / ${images.length}`;
      previewDimensions.textContent = image.width && image.height
        ? `${image.width} × ${image.height}`
        : formatSize(summary.size);
      download.querySelector('.dk-button-label').textContent = `下载结果 ${nextIndex + 1}`;
      for (const [controlIndex, control] of thumbnailControls.entries()) {
        const active = controlIndex === nextIndex;
        control.classList.toggle('is-selected', active);
        control.setAttribute('aria-pressed', String(active));
        control.tabIndex = active ? 0 : -1;
        control.querySelector('.dk-result-thumb__state').textContent = active ? '当前' : '';
      }
      if (focus && thumbnailControls[nextIndex]) {
        const control = thumbnailControls[nextIndex];
        const strip = control.parentElement;
        const canvasScrollTop = canvasBody.scrollTop;
        control.focus({ preventScroll: true });
        const controlRect = control.getBoundingClientRect();
        const stripRect = strip.getBoundingClientRect();
        if (controlRect.left < stripRect.left) strip.scrollLeft -= stripRect.left - controlRect.left;
        if (controlRect.right > stripRect.right) strip.scrollLeft += controlRect.right - stripRect.right;
        requestAnimationFrame(() => {
          if (!disposed) canvasBody.scrollTop = canvasScrollTop;
        });
      }
      if (announce) selectionAnnouncement.textContent = `已选择结果 ${nextIndex + 1}，共 ${images.length} 张。`;
    }

    function handleResultKey(event, index) {
      if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      if (event.key === 'Home') selectResult(0, { focus: true });
      else if (event.key === 'End') selectResult(images.length - 1, { focus: true });
      else selectResult(index + (event.key === 'ArrowLeft' || event.key === 'ArrowUp' ? -1 : 1), { focus: true });
    }

    thumbnailControls = images.map((image, index) => h('button', {
      class: `dk-result-thumb${index === state.selectedResultIndex ? ' is-selected' : ''}`,
      type: 'button',
      'aria-label': `选择结果 ${index + 1}`,
      'aria-pressed': String(index === state.selectedResultIndex),
      tabindex: index === state.selectedResultIndex ? '0' : '-1',
      onclick: () => selectResult(index),
      onkeydown: (event) => handleResultKey(event, index),
    },
      h('span', { class: 'dk-result-thumb__media' },
        h('img', {
          src: image.thumbnail_url || image.url,
          alt: '',
          loading: 'lazy',
          width: '112',
          height: '112',
        })),
      h('span', { class: 'dk-result-thumb__number', 'aria-hidden': 'true' }, String(index + 1).padStart(2, '0')),
      h('span', { class: 'dk-result-thumb__state', 'aria-hidden': 'true' },
        index === state.selectedResultIndex ? '当前' : '')));

    const filmstrip = images.length > 1
      ? h('div', {
        class: 'dk-result-filmstrip',
        role: 'group',
        'aria-label': `生成结果列表，共 ${images.length} 张`,
      }, ...thumbnailControls)
      : null;

    // 网关中途出错时可能只出了一部分图；要明确告诉用户，别让人以为本来就这么多
    const requested = Number(state.job?.params?.n) || images.length;
    const missing = Math.max(0, requested - images.length);
    const shortfall = missing
      ? inlineAlert(
        `本次只生成了 ${images.length} 张（原本请求 ${requested} 张）。生成服务中途出错，已保留成功的部分。`
        + `点「再补 ${missing} 张」只按缺的张数出图，不会把已经出好的重跑一遍。`,
        'warning')
      : null;

    // 只补缺的那几张。以前唯一的办法是「重新生成」整单——4 张里坏 1 张要花 4 张的钱
    const supplement = button(`再补 ${missing || 1} 张`, {
      variant: 'secondary',
      size: 'md',
      iconName: 'circle-plus',
      type: 'button',
      onclick: (event) => void supplementJob(missing || 1, event.currentTarget),
    });

    return h('div', { class: 'dk-result-state is-succeeded' },
      shortfall,
      h('div', { class: 'dk-result-toolbar' },
        h('div', { class: 'dk-result-toolbar__copy' },
          h('strong', {}, `${images.length} 张生成结果`),
          h('span', {}, `${summary.templateName} · ${formatSize(summary.size)} · ${formatQuality(summary.quality)}画质`)),
        h('div', { class: 'dk-result-actions' },
          button(state.submitting ? '正在创建…' : '按当前配置再生成', {
            variant: 'secondary',
            size: 'md',
            iconName: 'rotate-ccw',
            loading: state.submitting,
            disabled: state.submitting,
            type: 'button',
            onclick: () => void submitGeneration(),
          }), supplement)),
      state.submitError ? inlineAlert(state.submitError, 'error', { className: 'dk-result-submit-error' }) : null,
      h('div', { class: 'dk-result-viewer' },
        h('section', { class: 'dk-result-stage', 'aria-label': '当前结果预览' },
          h('div', { class: 'dk-result-stage__bar' },
            previewLabel,
            previewDimensions),
          previewButton),
        h('aside', { class: 'dk-result-inspector', 'aria-label': '结果选择与下载' },
          h('div', { class: 'dk-result-inspector__head' },
            h('strong', {}, images.length > 1 ? '选择结果' : '结果信息'),
            h('span', {}, `${images.length} 张`)),
          filmstrip,
          selectionAnnouncement,
          h('dl', { class: 'dk-result-meta' },
            summaryRow('生成方式', summary.templateName),
            summaryRow('图片尺寸', formatSize(summary.size)),
            summaryRow('画质', `${formatQuality(summary.quality)}画质`)),
          h('div', { class: 'dk-result-inspector__actions' },
            download, continueEdit, exportSized))));
  }

  /** 在已完成的任务上只补出所缺的张数，沿用上一次的提示词。 */
  async function supplementJob(count, trigger) {
    const jobId = state.job?.job_id;
    if (!jobId || state.submitting) return;
    if (trigger) trigger.disabled = true;
    const lifecycle = lifecycleSequence;
    const sequence = ++submitSequence;
    state.submitting = true;
    state.submitError = '';
    updatePrimaryAction();
    updateCanvas(true);
    try {
      // 先把这一批已经出好的图记下来。补图会新建一个任务，state.job 被整个替换后
      // 结果区只会显示新任务的图——刚刚还告诉用户「不会重跑已出好的」，
      // 转头那些图就从画布上消失了，读起来就像被弄丢了
      const carried = (state.job?.images || []).slice();
      const job = await api.post(`/api/web/generations/${jobId}/supplement`, { n: count });
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      state.carriedImages = carried;
      state.job = job;
      state.pollError = '';
      state.missingJobId = '';
      state.selectedResultIndex = 0;
      setMobileView('result', window.matchMedia('(max-width: 959px)').matches);
      updateCanvas(true);
      startPolling();
      toast(`已提交补图任务（${count} 张），原有 ${carried.length} 张仍会一起显示`, 'success');
    } catch (error) {
      if (disposed || lifecycle !== lifecycleSequence || sequence !== submitSequence) return;
      state.submitError = error?.message || '补图失败';
      toast(state.submitError, 'error', 5000);
    } finally {
      if (!disposed && lifecycle === lifecycleSequence && sequence === submitSequence) {
        state.submitting = false;
        updatePrimaryAction();
        updateCanvas(true);
      }
      if (trigger && !disposed) trigger.disabled = false;
    }
  }

  /** 本地把结果图裁成各平台常用尺寸并下载，全程不调用生图接口。 */
  function openExportDialog(image, index) {
    if (!image?.url) return;
    let mode = 'cover';
    let busy = false;

    const hint = h('p', { class: 'dk-field-help' }, FIT_MODES[0].hint);
    const modeGroup = h('div', { class: 'dk-filter-chips', role: 'group', 'aria-label': '填充方式' },
      ...FIT_MODES.map((item) => h('button', {
        class: `dk-filter-chip${item.key === mode ? ' is-selected' : ''}`,
        type: 'button',
        'aria-pressed': String(item.key === mode),
        onclick: (event) => {
          mode = item.key;
          hint.textContent = item.hint;
          for (const chip of modeGroup.querySelectorAll('.dk-filter-chip')) {
            const on = chip === event.currentTarget;
            chip.classList.toggle('is-selected', on);
            chip.setAttribute('aria-pressed', String(on));
          }
        },
      }, item.label)));

    const sizeList = h('div', { class: 'dk-export-sizes', role: 'group', 'aria-label': '导出尺寸' },
      ...PLATFORM_SIZES.map((target) => button(
        `${target.label} ${target.width}×${target.height}`,
        {
          variant: 'secondary',
          size: 'sm',
          type: 'button',
          onclick: async (event) => {
            if (busy) return;
            busy = true;
            const trigger = event.currentTarget;
            trigger.disabled = true;
            try {
              const loaded = await loadImage(image.url);
              const blob = await renderToBlob(loaded, target, mode);
              const objectUrl = URL.createObjectURL(blob);
              downloadUrl(objectUrl, `designkit-${target.key}-${index + 1}.png`);
              // 立刻回收会让部分浏览器来不及真正下载，等一拍再释放
              setTimeout(() => URL.revokeObjectURL(objectUrl), 10000);
              toast(`已导出 ${target.label} ${target.width}×${target.height}`, 'success');
            } catch (error) {
              if (disposed) return;
              toast(error?.message || '导出失败', 'error', 5000);
            } finally {
              busy = false;
              trigger.disabled = false;
            }
          },
        })));

    modal({
      title: '导出平台尺寸',
      body: h('div', { class: 'dk-export-dialog' },
        h('p', { class: 'dk-field-help' },
          '在你的电脑本地裁切，不会重新生成、不产生费用。'),
        field('填充方式', modeGroup, { help: '' }),
        hint,
        field('选择尺寸', sizeList, { help: '点一下即导出并下载' })),
      footer: [button('完成', { variant: 'secondary', type: 'button', onclick: () => closeOverlays() })],
    });
  }

  /** 把生成结果送回输入槽，作为下一轮的参考图。 */
  async function reuseResultAsInput(image, trigger, overrideName = '') {
    if (!image?.url) return;
    if (visibleUploads(state).length >= MAX_UPLOADS) {
      toast(`商品图已达 ${MAX_UPLOADS} 张上限，请先移除一张再继续`, 'error');
      return;
    }
    if (trigger) trigger.disabled = true;
    try {
      // 直接取回结果图再走一遍正常上传：这样它在后端是一份独立的上传记录，
      // 以后删掉那次生成任务也不会把这张输入图一起删掉
      const resp = await fetch(image.url);
      if (!resp.ok) throw new Error(`取回图片失败（HTTP ${resp.status}）`);
      const blob = await resp.blob();
      const name = overrideName || resultFilename(image, state.selectedResultIndex);
      const file = new File([blob], name, { type: blob.type || 'image/png' });

      const data = new FormData();
      data.append('file', file);
      const uploaded = await api.post('/api/web/uploads', data);
      if (disposed) return;
      // 上面两次 await 期间用户可能又拖了几张图进来，这里必须重新查一次上限，
      // 否则会攒出 5 张、提交时被后端 422 挡下
      if (visibleUploads(state).length >= MAX_UPLOADS) {
        toast(`商品图已达 ${MAX_UPLOADS} 张上限，这张没有加入`, 'error');
        return;
      }

      state.uploads.push({
        clientId: `upload-reuse-${Date.now()}-${++uploadClientId}`,
        ...uploaded,
        file: null,
        previewUrl: uploaded.url,
        objectUrl: false,
        status: 'uploaded',
        error: '',
        sequence: 0,
      });
      state.submitError = '';
      notifyUploadSession(session);
      setMobileView('config', window.matchMedia('(max-width: 959px)').matches);
      toast('已放进商品图，接着描述要改成什么样', 'success', 4200);
    } catch (error) {
      if (disposed) return;  // 已经切走了，别在别的页面弹一条没头没尾的提示
      toast(error?.message || '放回商品图失败，请重试', 'error', 5000);
    } finally {
      if (trigger && !disposed) trigger.disabled = false;
    }
  }

  function effectiveJobSummary() {
    const params = state.job?.params || {};
    const inputs = state.job?.input_images?.map((url, index) => ({ url, name: `输入商品图 ${index + 1}` }))
      || state.jobSummary?.inputs
      || [];
    return {
      templateName: state.job?.template_name || state.jobSummary?.templateName || '自由模式',
      size: params.size || state.jobSummary?.size || state.size,
      n: params.n || state.jobSummary?.n || state.n,
      quality: params.quality || state.jobSummary?.quality || state.quality,
      inputs,
    };
  }

  function formatSize(value) {
    const option = SIZE_OPTIONS.find((item) => item.value === value);
    return option ? `${option.label} ${option.ratio}` : value;
  }

  function formatQuality(value) {
    return QUALITY_OPTIONS.find((item) => item.value === value)?.label || value;
  }

  function summaryRow(label, value) {
    return h('div', {}, h('dt', {}, label), h('dd', {}, value));
  }

  function resultFilename(image, index) {
    const id = state.job?.job_id?.slice(0, 8) || 'result';
    return `designkit_${id}_${index + 1}.${image.format || 'png'}`;
  }

  function resetMissingJob() {
    state.missingJobId = '';
    state.job = null;
    state.pollError = '';
    updatePrimaryAction();
    updateCanvas(true);
    focusConfiguration();
  }

  // ---------------------------------------------------------- Mobile / cleanup

  function handleMobileTabKey(event) {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === 'ArrowLeft' || event.key === 'Home' ? 'config' : 'result';
    setMobileView(next, true);
  }

  function setMobileView(view, focusTab = false) {
    state.mobileView = view === 'result' ? 'result' : 'config';
    const showConfig = state.mobileView === 'config';
    root.dataset.mobileView = state.mobileView;
    configPanel.classList.toggle('is-mobile-active', showConfig);
    resultCanvas.classList.toggle('is-mobile-active', !showConfig);
    configTab.classList.toggle('is-active', showConfig);
    resultTab.classList.toggle('is-active', !showConfig);
    configTab.setAttribute('aria-selected', String(showConfig));
    resultTab.setAttribute('aria-selected', String(!showConfig));
    configTab.tabIndex = showConfig ? 0 : -1;
    resultTab.tabIndex = showConfig ? -1 : 0;
    if (focusTab) (showConfig ? configTab : resultTab).focus();
  }

  function focusConfiguration() {
    setMobileView('config');
    requestAnimationFrame(() => uploadSection.dropzone.focus());
  }

  function dispose() {
    if (disposed) return;
    // 路由只销毁本次视图；上传队列、File 与 blob 预览由登录会话继续持有。
    saveGenerateState(user, state);
    disposed = true;
    lifecycleSequence += 1;
    submitSequence += 1;
    unsubscribeUploads();
    stopPolling();
  }

  return dispose;
}
