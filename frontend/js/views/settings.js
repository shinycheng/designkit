// 系统设置：四个独立 section，各自管理 draft / dirty / saving / result
import { api, getSessionEpoch, getUser, setSession } from '../api.js';
import { button, field, h, inlineAlert, skeleton, toast } from '../ui.js';

export function renderSettings(container) {
  const state = { settings: null, stopped: false, controllers: [] };
  const page = h('section', { class: 'dk-page dk-page--settings', 'aria-labelledby': 'settings-page-title' });
  const stack = h('div', { class: 'dk-settings-stack', 'aria-busy': 'true' }, skeleton({ lines: 10, className: 'dk-loading' }));
  page.append(h('h1', { id: 'settings-page-title', class: 'dk-visually-hidden' }, '系统设置'), stack);
  container.append(page);

  async function load() {
    try {
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
      keys: ['provider', 'openai_base_url', 'openai_api_key', 'image_model'],
      renderFields: (form) => {
        const provider = h('select', { class: 'select' },
          h('option', { value: 'mock' }, '模拟生图（验证流程）'),
          h('option', { value: 'openai' }, 'OpenAI 兼容接口'));
        const baseUrl = h('input', { class: 'input', type: 'url', placeholder: 'https://api.openai.com' });
        const apiKey = h('input', { class: 'input', type: 'password', autocomplete: 'off', placeholder: 'sk-…' });
        const model = h('input', { class: 'input', placeholder: 'gpt-image-1' });
        form.register('provider', provider);
        form.register('openai_base_url', baseUrl);
        form.register('openai_api_key', apiKey);
        form.register('image_model', model);
        mockNotice = inlineAlert('模拟模式会返回占位图，适合验证上传、队列和 ERP 回调流程，不会产生模型费用。', 'info');
        const syncModeNotice = () => { mockNotice.hidden = provider.value !== 'mock'; };
        provider.addEventListener('change', syncModeNotice);
        queueMicrotask(syncModeNotice);
        return h('div', { class: 'dk-panel-stack' },
          field('服务模式', provider),
          mockNotice,
          h('div', { class: 'dk-field-grid' },
            field('API 地址（Base URL）', baseUrl, { help: '可填 OpenAI 官方或兼容中转服务的地址。' }),
            field('生图模型', model, { help: '默认为 gpt-image-1；中转平台如有要求，使用其指定模型名。' })),
          field('API Key', apiKey, { help: '已保存的值仅显示掩码；不修改即不会覆盖原密钥。' }));
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
        return [testButton];
      },
    });
    return controller;
  }

  function createRuntimeSection() {
    return createSettingsController({
      id: 'runtime-settings',
      title: '运行参数',
      description: '调整生成并发、重试、超时和新任务的默认输出。',
      keys: ['worker_concurrency', 'max_attempts', 'request_timeout', 'default_size', 'default_n'],
      renderFields: (form) => {
        const concurrency = h('input', { class: 'input', type: 'number', min: 1, max: 8, inputmode: 'numeric' });
        const attempts = h('input', { class: 'input', type: 'number', min: 1, max: 5, inputmode: 'numeric' });
        const timeout = h('input', { class: 'input', type: 'number', min: 30, max: 900, inputmode: 'numeric' });
        const defaultSize = h('select', { class: 'select' },
          h('option', { value: '1024x1024' }, '方形 · 1024 × 1024'),
          h('option', { value: '1536x1024' }, '横幅 · 1536 × 1024'),
          h('option', { value: '1024x1536' }, '竖幅 · 1024 × 1536'),
          h('option', { value: 'auto' }, '自动'));
        const defaultCount = h('input', { class: 'input', type: 'number', min: 1, max: 4, inputmode: 'numeric' });
        form.register('worker_concurrency', concurrency, numberBinding());
        form.register('max_attempts', attempts, numberBinding());
        form.register('request_timeout', timeout, numberBinding());
        form.register('default_size', defaultSize);
        form.register('default_n', defaultCount, numberBinding());
        return h('div', { class: 'dk-panel-stack' },
          inlineAlert('修改并发生成数后需要重启服务才能对 worker 生效。', 'info', { title: '需要重启' }),
          h('div', { class: 'dk-field-grid' },
            field('并发生成数', concurrency, { help: '同时执行的生成任务数，范围 1–8。' }),
            field('失败尝试次数', attempts, { help: '包含首次请求，范围 1–5。' }),
            field('生图超时', timeout, { help: '单次请求最长等待秒数，范围 30–900。' }),
            field('默认输出尺寸', defaultSize),
            field('默认生成张数', defaultCount, { help: '新任务的预设值，范围 1–4。' })));
      },
      validate: (draft) => {
        if (!integerBetween(draft.worker_concurrency, 1, 8)) return '并发生成数必须为 1–8 的整数。';
        if (!integerBetween(draft.max_attempts, 1, 5)) return '失败尝试次数必须为 1–5 的整数。';
        if (!integerBetween(draft.request_timeout, 30, 900)) return '生图超时必须为 30–900 秒。';
        if (!['1024x1024', '1536x1024', '1024x1536', 'auto'].includes(draft.default_size)) return '默认尺寸不受支持。';
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
        form.register('allow_internal_targets', allowInternal, checkboxBinding());
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

  function checkboxBinding() {
    return {
      read: (control) => control.checked,
      write: (control, value) => { control.checked = Boolean(value); },
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
