// 系统设置：生图服务配置 / 系统参数 / 修改密码
import { api, getUser, setSession } from '../api.js';
import { field, h, toast } from '../ui.js';

export function renderSettings(container) {
  let settings = null;

  const providerCard = h('div', { class: 'card' });
  const systemCard = h('div', { class: 'card' });
  const passwordCard = h('div', { class: 'card' });
  container.append(providerCard, systemCard, passwordCard);

  async function load() {
    try { settings = await api.get('/api/web/settings'); }
    catch (e) { return toast(e.message, 'error'); }
    renderProvider(); renderSystem(); renderPassword();
  }

  function renderProvider() {
    providerCard.innerHTML = '';
    const draft = {};
    const providerSel = h('select', { class: 'select', onchange: e => { draft.provider = e.target.value; hint.style.display = e.target.value === 'mock' ? 'block' : 'none'; } },
      h('option', { value: 'mock', selected: settings.provider === 'mock' }, '模拟生图（免费，用于验证流程）'),
      h('option', { value: 'openai', selected: settings.provider === 'openai' }, 'OpenAI 兼容接口（官方 / 国内中转平台）'));
    const hint = h('div', {
      class: 'login-tip', style: { display: settings.provider === 'mock' ? 'block' : 'none', marginBottom: '14px' },
    }, '当前是模拟模式：生成的是占位图，不花钱。拿到中转平台的 API Key 后，切换为「OpenAI 兼容接口」并填好下面三项即可出真图。');
    const baseInput = h('input', { class: 'input', value: settings.openai_base_url || '', placeholder: '例如 https://api.openai.com 或中转平台地址', oninput: e => draft.openai_base_url = e.target.value });
    const keyInput = h('input', { class: 'input', type: 'password', value: settings.openai_api_key || '', placeholder: 'sk-…', oninput: e => draft.openai_api_key = e.target.value });
    const modelInput = h('input', { class: 'input', value: settings.image_model || '', placeholder: 'gpt-image-1', oninput: e => draft.image_model = e.target.value });

    const testResult = h('span', { style: { fontSize: '13px', marginLeft: '10px' } });
    const saveBtn = h('button', {
      class: 'btn', onclick: async () => {
        try {
          settings = await api.put('/api/web/settings', draft);
          toast('已保存', 'success'); renderProvider();
        } catch (e) { toast(e.message, 'error'); }
      },
    }, '保存');
    const testBtn = h('button', {
      class: 'btn ghost', style: { marginLeft: '10px' }, onclick: async () => {
        testResult.textContent = '测试中…'; testResult.style.color = 'var(--text-3)';
        try {
          const r = await api.post('/api/web/settings/test_provider');
          testResult.textContent = (r.ok ? '✅ ' : '❌ ') + r.message;
          testResult.style.color = r.ok ? 'var(--success)' : 'var(--danger)';
        } catch (e) { testResult.textContent = '❌ ' + e.message; testResult.style.color = 'var(--danger)'; }
      },
    }, '测试连接');

    providerCard.append(
      h('div', { class: 'card-title' }, '生图服务',
        h('span', { class: 'hint' }, '对接 ChatGPT 生图 API（支持国内中转平台）')),
      field('服务模式', providerSel), hint,
      field('API 地址（Base URL）', baseInput, { help: '中转平台给你的接口地址，末尾带不带 /v1 都可以' }),
      field('API Key', keyInput, { help: '出于安全考虑显示为掩码，重新输入即可更换' }),
      field('生图模型', modelInput, { help: '默认 gpt-image-1；中转平台如有指定模型名，按其文档填写' }),
      h('div', { style: { display: 'flex', alignItems: 'center' } }, saveBtn, testBtn, testResult));
  }

  function renderSystem() {
    systemCard.innerHTML = '';
    const draft = {};
    const publicInput = h('input', { class: 'input', value: settings.public_base_url || '', oninput: e => draft.public_base_url = e.target.value });
    const concInput = h('input', { class: 'input', type: 'number', min: 1, max: 8, value: settings.worker_concurrency, oninput: e => draft.worker_concurrency = e.target.value });
    const attemptsInput = h('input', { class: 'input', type: 'number', min: 1, max: 5, value: settings.max_attempts, oninput: e => draft.max_attempts = e.target.value });
    const timeoutInput = h('input', { class: 'input', type: 'number', min: 30, max: 900, value: settings.request_timeout, oninput: e => draft.request_timeout = e.target.value });
    const allowInternal = h('input', { type: 'checkbox', checked: settings.allow_internal_targets, onchange: e => draft.allow_internal_targets = e.target.checked });

    systemCard.append(
      h('div', { class: 'card-title' }, '系统参数'),
      h('div', { class: 'row' },
        field('对外访问地址', publicInput, { help: '对外 API 和回调里的图片 URL 用它拼接；部署到服务器后改成服务器地址' }),
        field('并发生成数', concInput, { help: '同时执行的生成任务数，修改后需重启服务生效' })),
      h('div', { class: 'row' },
        field('失败重试次数', attemptsInput, { help: '生图失败时自动重试的总次数' }),
        field('生图超时（秒）', timeoutInput, { help: '等待生图接口返回的最长时间' })),
      field('允许取图/回调到内网地址',
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, allowInternal,
          h('span', { style: { fontSize: '13px', color: 'var(--text-2)' } }, '内部使用请保持勾选；对外开放（公网 SaaS）时取消勾选以拦截内网 SSRF'),
        ),
        { help: '无论是否勾选，云服务器元数据地址（169.254.x）等危险目标始终被拦截' }),
      h('button', {
        class: 'btn', onclick: async () => {
          try { settings = await api.put('/api/web/settings', draft); toast('已保存', 'success'); renderSystem(); }
          catch (e) { toast(e.message, 'error'); }
        },
      }, '保存'));
  }

  function renderPassword() {
    passwordCard.innerHTML = '';
    const oldInput = h('input', { class: 'input', type: 'password' });
    const newInput = h('input', { class: 'input', type: 'password', placeholder: '至少 8 位' });
    const new2Input = h('input', { class: 'input', type: 'password' });
    passwordCard.append(
      h('div', { class: 'card-title' }, '修改登录密码'),
      h('div', { class: 'row' },
        field('原密码', oldInput), field('新密码', newInput), field('确认新密码', new2Input)),
      h('button', {
        class: 'btn', onclick: async () => {
          if (!newInput.value || newInput.value.length < 8) return toast('新密码至少 8 位', 'error');
          if (newInput.value !== new2Input.value) return toast('两次输入的新密码不一致', 'error');
          try {
            const r = await api.post('/api/web/auth/change_password', { old_password: oldInput.value, new_password: newInput.value });
            // 改密会使旧令牌失效，用返回的新令牌续上当前会话，避免自己被登出
            if (r.token) setSession(r.token, getUser() || {});
            toast('密码已修改', 'success');
            oldInput.value = newInput.value = new2Input.value = '';
          } catch (e) { toast(e.message, 'error'); }
        },
      }, '修改密码'));
  }

  load();
}
