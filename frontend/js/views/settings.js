// 系统设置：四个独立 section，各自管理 draft / dirty / saving / result
import { api, getSessionEpoch, getUser, setSession } from '../api.js';
import { button, field, h, inlineAlert, skeleton, toast } from '../ui.js';

// 比例清单以后端 /api/web/size-presets 为准（后端拿同一份做校验）。
// 这里的初值只是接口没返回时的兜底，拉到后整体替换。
let SIZE_PRESETS = [
  { value: '1024x1024', label: '方图', ratio: '1:1' },
  { value: '1024x1360', label: '竖图', ratio: '3:4' },
  { value: '1024x1280', label: '竖图', ratio: '4:5' },
  { value: '1024x1536', label: '长竖图', ratio: '2:3' },
  { value: '1024x1824', label: '超竖图', ratio: '9:16' },
  { value: '1536x1024', label: '横图', ratio: '3:2' },
  { value: '1360x1024', label: '横图', ratio: '4:3' },
  { value: '1824x1024', label: '超横图', ratio: '16:9' },
  { value: 'auto', label: '自动', ratio: '自适应' },
];

function sizeOptionLabel(item) {
  return item.value === 'auto' ? '自动' : `${item.label} ${item.ratio} · ${item.value}`;
}

async function loadSizePresets() {
  try {
    const data = await api.get('/api/web/size-presets');
    if (Array.isArray(data?.presets) && data.presets.length) SIZE_PRESETS = data.presets;
  } catch { /* 用兜底清单 */ }
}

export function renderSettings(container) {
  const state = { settings: null, stopped: false, controllers: [] };
  const page = h('section', { class: 'dk-page dk-page--settings', 'aria-labelledby': 'settings-page-title' });
  const stack = h('div', { class: 'dk-settings-stack', 'aria-busy': 'true' }, skeleton({ lines: 10, className: 'dk-loading' }));
  page.append(h('h1', { id: 'settings-page-title', class: 'dk-visually-hidden' }, '系统设置'), stack);
  container.append(page);

  async function load() {
    try {
      // 比例清单要先到位，否则下拉会渲染成兜底的那几项，
      // 用户看到的选项和后端实际放行的对不上
      await loadSizePresets();
      state.settings = await api.get('/api/web/settings');
      if (!state.stopped) renderSections();
    } catch (error) {
      stack.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载系统设置' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
    } finally {
      stack.setAttribute('aria-busy', 'false');
    }
  }

  function renderSections() {
    stack.replaceChildren();
    state.controllers = [
      createProviderSection(),
      createRuntimeSection(),
      createSecuritySection(),
      createPasswordSection(),
    ];
    stack.append(...state.controllers.map((controller) => controller.element));
  }

  function createProviderSection() {
    let mockNotice;
    let testButton;
    const controller = createSettingsController({
      id: 'provider-settings',
      title: '生图服务',
      description: '配置图像生成供应商、接口地址和模型。',
      keys: ['provider', 'openai_base_url', 'openai_api_key', 'image_model',
        'text_model', 'prompt_synthesis', 'normalize_input_ratio', 'image_background'],
      renderFields: (form) => {
        const provider = h('select', { class: 'select' },
          h('option', { value: 'mock' }, '模拟生图（验证流程）'),
          h('option', { value: 'openai' }, 'OpenAI 兼容接口'));
        const baseUrl = h('input', { class: 'input', type: 'url', placeholder: 'http://192.168.31.235:8090/v1' });
        const apiKey = h('input', { class: 'input', type: 'password', autocomplete: 'off', placeholder: 'sk-…' });
        const model = h('input', { class: 'input', placeholder: 'gpt-image-2' });
        const normalize = h('input', { type: 'checkbox' });
        const textModel = h('input', { class: 'input', placeholder: 'gpt-5.6-sol' });
        const synthesis = h('input', { type: 'checkbox' });
        const background = h('select', { class: 'select' },
          h('option', { value: 'auto' }, '跟随模型（通常是实色底）'),
          h('option', { value: 'transparent' }, '透明底 PNG（免抠图）'));
        form.register('provider', provider);
        form.register('openai_base_url', baseUrl);
        form.register('openai_api_key', apiKey);
        form.register('image_model', model);
        form.register('text_model', textModel);
        form.register('prompt_synthesis', synthesis, checkboxBinding(true));
        form.register('normalize_input_ratio', normalize, checkboxBinding(true));
        form.register('image_background', background);
        mockNotice = inlineAlert('模拟模式会返回占位图，适合验证上传、队列和 ERP 回调流程，不会产生模型费用。', 'info');
        const syncModeNotice = () => { mockNotice.hidden = provider.value !== 'mock'; };
        provider.addEventListener('change', syncModeNotice);
        queueMicrotask(syncModeNotice);
        return h('div', { class: 'dk-panel-stack' },
          field('服务模式', provider),
          mockNotice,
          h('div', { class: 'dk-field-grid' },
            field('API 地址（Base URL）', baseUrl, { help: '可填 OpenAI 官方、兼容中转或自建网关的地址。' }),
            field('生图模型', model, { help: '默认 gpt-image-2（自建网关）；平台如有要求，使用其指定模型名。' })),
          field('API Key', apiKey, { help: '已保存的值仅显示掩码；不修改即不会覆盖原密钥。' }),
          field('AI 现场写提示词',
            h('label', { class: 'dk-checkbox-row' }, synthesis,
              h('span', { class: 'dk-checkbox-row__copy' }, '每次生成前，先让 AI 看一眼你的商品图再写提示词',
                h('small', { class: 'dk-checkbox-row__description' },
                  '模板和灵感库只作风格参考；AI 会结合实际商品与你的补充要求现场重写。'
                  + '每次多花约 30 秒文本模型开销，出图更贴合商品。'))),
            { help: '关闭后直接把模板原文发给生图模型（快，但通用模板对具体商品的贴合度较低）。' }),
          field('写提示词用的文本模型', textModel, {
            help: '需要能看图（视觉）的模型，默认 gpt-5.6-sol。改完先保存，再点下方「测试文本模型」。',
          }),
          field('发送前按所选比例预处理商品图',
            h('label', { class: 'dk-checkbox-row' }, normalize,
              h('span', { class: 'dk-checkbox-row__copy' }, '补边到目标比例（不裁产品）+ HEIC 自动转码；输入图的透明通道按下方「出图底色」处理')),
            { help: '自建网关会忽略 size 参数、出图比例跟随输入图，开启才能控制比例；对官方接口同样安全。' }),
          field('出图底色', background, {
            help: '选「透明底」后，出图直接带透明通道，换背景不用再抠图。'
              + '需要你的网关支持 background 参数：多数网关不支持时会直接报错，'
              + '但也有网关会静默忽略、照常出实底图。第一次用请先出 1 张确认。',
          }));
      },
      validate: (draft) => {
        if (!['mock', 'openai'].includes(draft.provider)) return '服务模式不受支持。';
        if (draft.provider === 'openai') {
          if (!isHttpUrl(draft.openai_base_url)) return '请填写有效的 HTTP(S) API 地址。';
          if (!String(draft.image_model || '').trim()) return '请填写生图模型。';
          if (!String(draft.openai_api_key || '').trim()) return '请填写 API Key。';
        }
        return '';
      },
      extraActions: (form) => {
        testButton = button('测试已保存的连接', {
          variant: 'secondary',
          iconName: 'plug',
          onclick: async () => {
            if (form.dirty) {
              form.setStatus('请先保存当前更改，连接测试只使用已保存的配置。', 'warning');
              return;
            }
            testButton.disabled = true;
            testButton.setAttribute('aria-busy', 'true');
            form.setStatus('正在测试连接…', 'saving');
            try {
              const result = await api.post('/api/web/settings/test_provider');
              form.setStatus(result.message, result.ok ? 'success' : 'error');
            } catch (error) {
              form.setStatus(error.message, 'error');
            } finally {
              testButton.disabled = false;
              testButton.removeAttribute('aria-busy');
            }
          },
        });
        const testTextButton = button('测试文本模型', {
          variant: 'secondary',
          iconName: 'wand-sparkles',
          onclick: async () => {
            if (form.dirty) {
              form.setStatus('请先保存当前更改，测试只使用已保存的配置。', 'warning');
              return;
            }
            testTextButton.disabled = true;
            testTextButton.setAttribute('aria-busy', 'true');
            form.setStatus('正在测试文本模型…', 'saving');
            try {
              const result = await api.post('/api/web/settings/test_text_model');
              form.setStatus(result.message, result.ok ? 'success' : 'error');
            } catch (error) {
              form.setStatus(error.message, 'error');
            } finally {
              testTextButton.disabled = false;
              testTextButton.removeAttribute('aria-busy');
            }
          },
        });
        return [testButton, testTextButton];
      },
    });
    return controller;
  }

  function createRuntimeSection() {
    return createSettingsController({
      id: 'runtime-settings',
      title: '运行参数',
      description: '调整生成并发、重试、超时和新任务的默认输出。',
      keys: ['worker_concurrency', 'max_attempts', 'request_timeout', 'default_size', 'default_n',
        'inspiration_auto_sync', 'inspiration_sync_interval_hours', 'inspiration_proxy'],
      validate: (draft) => {
        const ranges = [
          ['worker_concurrency', 1, 8, '并发生成数'],
          ['max_attempts', 1, 5, '失败尝试次数'],
          ['request_timeout', 30, 900, '生图超时'],
          ['default_n', 1, 4, '默认生成张数'],
          ['inspiration_sync_interval_hours', 1, 168, '灵感库自动同步间隔'],
        ];
        for (const [key, low, high, label] of ranges) {
          const v = Number(draft[key]);
          if (!Number.isInteger(v) || v < low || v > high) return `${label}需填 ${low}–${high} 之间的整数。`;
        }
        return '';
      },
      renderFields: (form) => {
        const concurrency = h('input', { class: 'input', type: 'number', min: 1, max: 8, inputmode: 'numeric' });
        const attempts = h('input', { class: 'input', type: 'number', min: 1, max: 5, inputmode: 'numeric' });
        const timeout = h('input', { class: 'input', type: 'number', min: 30, max: 900, inputmode: 'numeric' });
        // 选项由后端 /api/web/size-presets 下发（见下方 loadSizePresets），
        // 不在前端另写一份，否则加比例时必漏改
        const defaultSize = h('select', { class: 'select' },
          ...SIZE_PRESETS.map((item) => h('option', { value: item.value }, sizeOptionLabel(item))));
        const defaultCount = h('input', { class: 'input', type: 'number', min: 1, max: 4, inputmode: 'numeric' });
        const autoSync = h('input', { type: 'checkbox' });
        const syncInterval = h('input', { class: 'input', type: 'number', min: 1, max: 168, inputmode: 'numeric' });
        const syncProxy = h('input', {
          class: 'input', type: 'text', autocomplete: 'off',
          placeholder: 'http://127.0.0.1:7890（留空即直连）',
        });
        form.register('worker_concurrency', concurrency, numberBinding());
        form.register('max_attempts', attempts, numberBinding());
        form.register('request_timeout', timeout, numberBinding());
        form.register('default_size', defaultSize);
        form.register('default_n', defaultCount, numberBinding());
        form.register('inspiration_auto_sync', autoSync, checkboxBinding(true));
        form.register('inspiration_sync_interval_hours', syncInterval, numberBinding());
        form.register('inspiration_proxy', syncProxy);
        return h('div', { class: 'dk-panel-stack' },
          inlineAlert('修改并发生成数后需要重启服务才能对 worker 生效。', 'info', { title: '需要重启' }),
          h('div', { class: 'dk-field-grid' },
            field('并发生成数', concurrency, { help: '同时执行的生成任务数，范围 1–8。' }),
            field('失败尝试次数', attempts, { help: '包含首次请求，范围 1–5。' }),
            field('生图超时', timeout, { help: '单次请求最长等待秒数，范围 30–900。' }),
            field('默认输出尺寸', defaultSize),
            field('默认生成张数', defaultCount, { help: '新任务的预设值，范围 1–4。' }),
            field('灵感库自动同步间隔', syncInterval, { help: '每隔多少小时拉一次上游更新，范围 1–168（上游每天更新两次，建议 12）。' })),
          field('自动同步灵感库',
            h('label', { class: 'dk-checkbox-row' }, autoSync,
              h('span', { class: 'dk-checkbox-row__copy' }, '按上面的间隔自动拉取上游提示词库更新',
                h('small', { class: 'dk-checkbox-row__description' },
                  '后台执行，不影响使用；多进程部署时只有一个进程会真正同步。'
                  + '连续失败会自动退避重试，不会反复冲击上游。'))),
            { help: '关闭后只能在「灵感库」页手动点「同步上游」。' }),
          field('同步代理', syncProxy, {
            help: '同步灵感库要访问 GitHub（raw.githubusercontent.com），部分地区直连不通，'
              + '这时填一个代理地址。留空即直连。'
              + '只影响灵感库同步——生图网关是局域网地址，永远直连、不走代理。'
              + '改完先保存，再点下方「测试同步连接」。',
          }));
      },
      extraActions: (form) => {
        const testSync = button('测试同步连接', {
          variant: 'secondary',
          iconName: 'refresh-cw',
          onclick: async () => {
            if (form.dirty) {
              form.setStatus('请先保存当前更改，测试只使用已保存的配置。', 'warning');
              return;
            }
            testSync.disabled = true;
            testSync.setAttribute('aria-busy', 'true');
            form.setStatus('正在测试…（只拉一个很小的文件，几秒出结果）', 'saving');
            try {
              const result = await api.post('/api/web/settings/test_sync_proxy');
              form.setStatus(result.message, result.ok ? 'success' : 'error');
            } catch (error) {
              form.setStatus(error.message, 'error');
            } finally {
              testSync.disabled = false;
              testSync.removeAttribute('aria-busy');
            }
          },
        });
        return [testSync];
      },
      validate: (draft) => {
        const proxy = String(draft.inspiration_proxy || '').trim();
        if (proxy && !/^(https?|socks5h?):\/\//.test(proxy)) {
          return '代理地址要以 http:// 或 socks5:// 开头，例如 http://127.0.0.1:7890。';
        }
        if (!integerBetween(draft.worker_concurrency, 1, 8)) return '并发生成数必须为 1–8 的整数。';
        if (!integerBetween(draft.max_attempts, 1, 5)) return '失败尝试次数必须为 1–5 的整数。';
        if (!integerBetween(draft.request_timeout, 30, 900)) return '生图超时必须为 30–900 秒。';
        if (!SIZE_PRESETS.some((item) => item.value === draft.default_size)) return '默认尺寸不受支持。';
        if (!integerBetween(draft.default_n, 1, 4)) return '默认生成张数必须为 1–4 的整数。';
        return '';
      },
    });
  }

  function createSecuritySection() {
    return createSettingsController({
      id: 'security-settings',
      title: '安全与网络',
      description: '设置对外访问地址和服务器取图、回调的内网访问策略。',
      keys: ['public_base_url', 'allow_internal_targets'],
      renderFields: (form) => {
        const publicUrl = h('input', { class: 'input', type: 'url', placeholder: 'https://designkit.example.com' });
        const allowInternal = h('input', { type: 'checkbox' });
        form.register('public_base_url', publicUrl);
        form.register('allow_internal_targets', allowInternal, checkboxBinding(true));
        return h('div', { class: 'dk-panel-stack' },
          field('对外访问地址', publicUrl, { required: true, help: '结果图地址和 Webhook 回调数据会使用此基础地址。' }),
          h('label', { class: 'dk-checkbox-row' },
            allowInternal,
            h('span', { class: 'dk-checkbox-row__copy' },
              h('strong', {}, '允许访问内网图片和回调地址'),
              h('small', { class: 'dk-checkbox-row__description' }, '仅限完全受信的企业内网部署。'))),
          inlineAlert('如果服务暴露在公网，应关闭内网目标访问，以降低 SSRF 风险。无论此选项是否开启，云元数据等高风险保留地址都会被拦截。', 'warning', { title: '公网部署安全提示' }));
      },
      validate: (draft) => isHttpUrl(draft.public_base_url) ? '' : '请填写有效的 HTTP(S) 对外访问地址。',
    });
  }

  function createPasswordSection() {
    const section = h('section', { class: 'dk-settings-section', 'aria-labelledby': 'password-settings-title' });
    const oldPassword = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
    const newPassword = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const confirmPassword = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const status = h('div', { class: 'dk-settings-section__status', 'aria-live': 'polite' });
    const saveButton = button('修改密码', { iconName: 'lock', disabled: true });
    let saving = false;

    const syncDirty = () => {
      const dirty = Boolean(oldPassword.value || newPassword.value || confirmPassword.value);
      saveButton.disabled = !dirty || saving;
      if (dirty && status.dataset.state !== 'error') {
        status.dataset.state = 'dirty';
        status.textContent = '有未提交的更改';
      } else if (!dirty && status.dataset.state === 'dirty') {
        status.textContent = '';
        delete status.dataset.state;
      }
    };
    [oldPassword, newPassword, confirmPassword].forEach((input) => input.addEventListener('input', syncDirty));

    saveButton.addEventListener('click', async () => {
      status.replaceChildren();
      if (!oldPassword.value) return setPasswordError('请填写当前密码。', oldPassword);
      if (newPassword.value.length < 8) return setPasswordError('新密码至少需要 8 位。', newPassword);
      if (newPassword.value !== confirmPassword.value) return setPasswordError('两次输入的新密码不一致。', confirmPassword);
      saving = true;
      const sessionAtSubmit = getSessionEpoch();
      saveButton.disabled = true;
      saveButton.setAttribute('aria-busy', 'true');
      [oldPassword, newPassword, confirmPassword].forEach((input) => { input.disabled = true; });
      status.dataset.state = 'saving';
      status.textContent = '正在修改密码…';
      try {
        const result = await api.post('/api/web/auth/change_password', {
          old_password: oldPassword.value,
          new_password: newPassword.value,
        });
        if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
        if (result.token) setSession(result.token, getUser() || {});
        oldPassword.value = '';
        newPassword.value = '';
        confirmPassword.value = '';
        status.dataset.state = 'success';
        status.textContent = '密码已修改，当前会话已安全续期。';
        toast('密码已修改', 'success');
      } catch (error) {
        if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
        status.dataset.state = 'error';
        status.textContent = error.message;
      } finally {
        saving = false;
        [oldPassword, newPassword, confirmPassword].forEach((input) => { input.disabled = false; });
        saveButton.removeAttribute('aria-busy');
        syncDirty();
      }
    });

    function setPasswordError(message, input) {
      status.dataset.state = 'error';
      status.textContent = message;
      input.focus();
    }

    section.append(
      sectionHeader('password-settings-title', '账号安全', '修改当前管理员账号的登录密码。'),
      h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); saveButton.click(); } },
        h('div', { class: 'dk-field-grid' },
          field('当前密码', oldPassword, { required: true }),
          field('新密码', newPassword, { required: true, help: '至少 8 位，建议使用长句和数字、符号组合。' }),
          field('确认新密码', confirmPassword, { required: true })),
        h('div', { class: 'dk-settings-section__actions' }, status, saveButton)),
    );
    return { element: section };
  }

  function createSettingsController({ id, title, description, keys, renderFields, validate, extraActions }) {
    const baseline = {};
    const draft = {};
    const bindings = new Map();
    let dirty = false;
    let saving = false;
    keys.forEach((key) => {
      baseline[key] = state.settings[key];
      draft[key] = state.settings[key];
    });

    const status = h('div', { class: 'dk-settings-section__status', 'aria-live': 'polite' });
    const saveButton = button('保存此区域', { iconName: 'save', disabled: true });
    const formApi = {
      get dirty() { return dirty; },
      draft,
      register(key, control, binding = textBinding()) {
        bindings.set(key, { control, binding });
        binding.write(control, draft[key]);
        const update = () => {
          draft[key] = binding.read(control);
          syncDirty();
        };
        control.addEventListener('input', update);
        control.addEventListener('change', update);
      },
      setStatus(message, statusState = '') {
        status.textContent = message;
        if (statusState) status.dataset.state = statusState;
        else delete status.dataset.state;
      },
    };
    const fields = renderFields(formApi);
    const secondaryActions = extraActions ? extraActions(formApi) : [];
    const section = h('section', { class: 'dk-settings-section', 'aria-labelledby': id + '-title' },
      sectionHeader(id + '-title', title, description),
      h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); save(); } },
        fields,
        h('div', { class: 'dk-settings-section__actions' },
          status,
          ...secondaryActions,
          saveButton)));

    function syncDirty() {
      dirty = keys.some((key) => !sameValue(draft[key], baseline[key]));
      saveButton.disabled = !dirty || saving;
      if (dirty) {
        formApi.setStatus('有未保存的更改', 'dirty');
      } else if (status.dataset.state === 'dirty') {
        formApi.setStatus('');
      }
    }

    async function save() {
      if (!dirty || saving) return;
      const validationMessage = validate ? validate(draft) : '';
      if (validationMessage) {
        formApi.setStatus(validationMessage, 'error');
        return;
      }
      const patch = {};
      keys.forEach((key) => {
        if (!sameValue(draft[key], baseline[key])) patch[key] = draft[key];
      });
      saving = true;
      saveButton.disabled = true;
      saveButton.setAttribute('aria-busy', 'true');
      bindings.forEach(({ control }) => { control.disabled = true; });
      secondaryActions.forEach((control) => { control.disabled = true; });
      formApi.setStatus('正在保存…', 'saving');
      try {
        const response = await api.put('/api/web/settings', patch);
        state.settings = response;
        // 仅同步当前 section，不重绘也不覆盖其他 section 的 draft。
        keys.forEach((key) => {
          baseline[key] = response[key];
          draft[key] = response[key];
          const registered = bindings.get(key);
          if (registered) registered.binding.write(registered.control, response[key]);
        });
        dirty = false;
        formApi.setStatus('已保存', 'success');
        toast(`${title}已保存`, 'success');
      } catch (error) {
        formApi.setStatus(error.message, 'error');
      } finally {
        saving = false;
        bindings.forEach(({ control }) => { control.disabled = false; });
        secondaryActions.forEach((control) => { control.disabled = false; });
        saveButton.removeAttribute('aria-busy');
        saveButton.disabled = !dirty;
      }
    }

    saveButton.addEventListener('click', save);
    return {
      element: section,
      get dirty() { return dirty; },
      setStatus: formApi.setStatus,
    };
  }

  function sectionHeader(id, title, description) {
    return h('div', { class: 'dk-settings-section__head' },
      h('div', { class: 'dk-settings-section__copy' },
        h('h2', { id, class: 'dk-section-title' }, title),
        h('p', { class: 'dk-section-meta' }, description)));
  }

  function textBinding() {
    return {
      read: (control) => control.value,
      write: (control, value) => { control.value = value == null ? '' : String(value); },
    };
  }

  function numberBinding() {
    return {
      read: (control) => Number(control.value),
      write: (control, value) => { control.value = value == null ? '' : String(value); },
    };
  }

  function checkboxBinding(fallback = false) {
    return {
      read: (control) => control.checked,
      // 后端没返回该键时用它的既定默认值，而不是一律当成未勾选——
      // 否则用户保存一次就会把默认开启的功能悄悄关掉
      write: (control, value) => {
        control.checked = value === undefined || value === null ? fallback : Boolean(value);
      },
    };
  }

  function sameValue(a, b) {
    return String(a) === String(b);
  }

  function integerBetween(value, min, max) {
    return Number.isInteger(Number(value)) && Number(value) >= min && Number(value) <= max;
  }

  function isHttpUrl(value) {
    try {
      const url = new URL(String(value || ''));
      return url.protocol === 'http:' || url.protocol === 'https:';
    } catch {
      return false;
    }
  }

  load();
  return () => { state.stopped = true; };
}
