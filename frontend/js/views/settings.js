// 系统设置：四个独立 section，各自管理 draft / dirty / saving / result
import { api, getSessionEpoch, getUser, setSession } from '../api.js';
import { button, field, fmtTime, h, inlineAlert, skeleton, toast } from '../ui.js';

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
      createAccessSection(),
      // 这一节在后端还没有自动开通功能时返回 null（见函数内注释），
      // 所以这里要把 null 过滤掉，否则 stack.append 会抛异常、整页设置打不开。
      createProvisioningSection(),
      createSelfRegisterSection(),
      createSmsSection(),
      createPasswordSection(),
    ].filter(Boolean);
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
              // 后端为了不把代理密码回吐到页面上，会把 user:pass 那一段打成 ***（见
              // settings_service.masked）。管理员如果在这串上直接改端口再保存，
              // 那三个星号会被当成真的密码存进去，表现是「灵感库同步突然连不上」，
              // 而页面上什么错都看不出来。所以这里必须提醒他整串重填。
              + '带用户名密码的代理，密码不会显示（显示成 ***）；要改的话请把整串重新填一遍，'
              + '只改其中一段会把密码存成三个星号本身。'
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

  /** 图片访问与多用户。
   *
   * 这一节管的是「谁能看到图」和「生图的钱记在谁头上」。
   * 尤其是「生图费用怎么算」：不在这里放一个开关的话，管理员在「成员账号」页
   * 给每个人配好了 Key，却永远没有地方把它启用，界面上还一直提示他
   *「要等在系统设置里改成每人一把」——那句提示就变成了一条死路。
   */
  function createAccessSection() {
    return createSettingsController({
      id: 'access-settings',
      title: '图片访问与多用户',
      description: '控制图片链接要不要凭证才能打开，以及生图费用是大家共用一把 Key 还是各花各的。',
      keys: ['gateway_mode', 'files_signed_only', 'web_file_cookie_days',
        'erp_file_link_ttl_hours', 'allowed_origins'],
      renderFields: (form) => {
        const gatewayMode = h('select', { class: 'select' },
          h('option', { value: 'shared' }, '共用一把 Key（所有人的生图费用记在一起）'),
          h('option', { value: 'per_user' }, '每人一把 Key（各花各的，需要先在「成员账号」页给每个人配 Key）'));
        const signedOnly = h('input', { type: 'checkbox' });
        const cookieDays = h('input', { class: 'input', type: 'number', min: 7, max: 30, inputmode: 'numeric' });
        const erpTtl = h('input', { class: 'input', type: 'number', min: 1, max: 8760, inputmode: 'numeric' });
        const origins = h('input', { class: 'input', type: 'text', autocomplete: 'off', placeholder: '*' });
        form.register('gateway_mode', gatewayMode);
        form.register('files_signed_only', signedOnly, checkboxBinding(true));
        form.register('web_file_cookie_days', cookieDays, numberBinding());
        form.register('erp_file_link_ttl_hours', erpTtl, numberBinding());
        form.register('allowed_origins', origins);

        // 只读的观察窗口：本次启动以来有多少次「没带任何凭证」的取图请求。
        // 这是判断「敢不敢临时关掉图片访问限制」的唯一客观依据，不用去翻日志。
        const stats = state.settings.files_unsigned_access || {};
        const total = Number(stats.total || 0);
        const blocked = Number(stats.blocked || 0);
        const lastAt = Number(stats.last_at || 0);
        const statsText = total
          ? `自本次启动以来，有 ${total} 次没有凭证的图片访问（其中 ${blocked} 次已被拒绝）。`
            + `最近一次：${lastAt ? fmtEpoch(lastAt) : '从未发生'}。`
          : '自本次启动以来，没有出现过不带凭证的图片访问。';

        return h('div', { class: 'dk-panel-stack' },
          field('生图费用怎么算', gatewayMode, {
            help: '选「每人一把」之后，没有配 Key 的成员点生成会被直接拦住，并提示他找管理员开通。'
              + '改成「共用一把」则全体回到系统设置里上面那把全局 Key。',
          }),
          field('图片链接需要凭证才能打开',
            h('label', { class: 'dk-checkbox-row' }, signedOnly,
              h('span', { class: 'dk-checkbox-row__copy' }, '打开（推荐）',
                h('small', { class: 'dk-checkbox-row__description' },
                  '关掉之后，没有登录凭证的人也能凭地址直接打开图片，只用于临时兼容对接方手里的老链接。'))),
            { help: '网页端靠登录后自动发放的凭证取图，对接方靠链接里的签名参数，正常使用都不受影响。' }),
          h('div', { class: 'dk-field-grid' },
            field('网页取图凭证有效期（天）', cookieDays, {
              help: '范围 7–30。到期后重新登录一次即可，用户一般察觉不到。',
            }),
            field('对接方图片链接有效期（小时）', erpTtl, {
              help: '范围 1–8760（8760 小时 = 一年）。默认 168 小时，也就是 7 天。',
            })),
          field('允许跨站调用的地址（allowed_origins）', origins, {
            help: '多个用英文逗号隔开，不限制填 *。每一条都要带 http:// 或 https:// 且不带路径。'
              + '注意：这一项改完需要重启服务才生效——页面提示保存成功也不会立刻起作用，'
              + '这是正常的，不用反复保存。',
          }),
          inlineAlert(statsText, total ? 'warning' : 'info', { title: '无凭证访问统计' }),
          h('p', { class: 'dk-section-meta' }, '这个数字在服务重启后归零，它只是一个观察窗口。'));
      },
      validate: (draft) => {
        if (!['shared', 'per_user'].includes(draft.gateway_mode)) return '生图费用方式只能选「共用一把」或「每人一把」。';
        if (typeof draft.files_signed_only !== 'boolean') return '图片凭证开关的取值不对，请重新勾选一次。';
        if (!integerBetween(draft.web_file_cookie_days, 7, 30)) return '网页取图凭证有效期必须是 7–30 天的整数。';
        if (!integerBetween(draft.erp_file_link_ttl_hours, 1, 8760)) return '对接方图片链接有效期必须是 1–8760 小时的整数。';
        const origins = String(draft.allowed_origins || '').trim();
        if (origins && origins !== '*') {
          for (const item of origins.split(',')) {
            const one = item.trim();
            if (!one) continue;
            if (!/^https?:\/\/[^/]+\/?$/.test(one)) {
              return `「${one}」不是合法的地址：要以 http:// 或 https:// 开头，且不能带路径，例如 https://erp.example.com。`;
            }
          }
        }
        return '';
      },
    });
  }

  function fmtEpoch(seconds) {
    return fmtTime(new Date(seconds * 1000).toISOString());
  }

  /* ── 网关自动开通 ─────────────────────────────────────────────────────
   *
   * 这一节管的是「新成员建号时，系统要不要自己跑到图片网关那边替他开个账号、
   * 领一把 Key」。做这件事的全部理由就是省掉管理员在「成员账号」页手工粘 Key
   * 那一步——**它永远只是省事，不是唯一通路**。所以这一节的文案要反复讲清楚：
   * 开不通不影响任何人登录和使用，只是生图要等你手工发 Key。
   *
   * 界面上永远不出现的东西（后端接口也压根不返回）：
   *   - 成员在网关那边的登录密码（系统自己生成的，开通成功后就删了）
   *   - 任何一把 Key 的内容，包括末 4 位
   *   - E_LOGIN_2FA 这类分类码
   */

  /** 后端的时间戳有两种：带 Z 的（users.py 的 _iso 会补）和不带 Z 的裸 UTC
   * （provisioning.summary 里的 halted_at 就是裸的）。不带 Z 时浏览器会按**本地时区**
   * 解释，中国时区直接差 8 小时——「刚刚暂停的」会显示成 8 小时前，
   * 管理员会以为这是条陈年旧账，不当回事。所以这里统一补一个 Z 再交给 fmtTime。
   */
  function fmtServerTime(iso) {
    const text = String(iso || '');
    if (!text) return '';
    const normalized = /(Z|[+-]\d{2}:?\d{2})$/.test(text) ? text : `${text}Z`;
    return fmtTime(normalized);
  }

  // 总结论那一行的标题。单项的等级文字用后端给的 level_text
  //（「正常 / 注意 / 有问题 / 未验证」），不在前端另写一份，免得两边说法不一致。
  const OVERALL_TEXT = {
    green: '可以自动开通',
    yellow: '能用，但有几处要注意',
    red: '现在开不通，需要先处理',
    // unknown 只出现在单项上：库里一个已开通的成员都没有、手上没有登录令牌，
    // 都会是这个。**必须照实写「未验证」**，画成绿灯会让管理员以为查过了、
    // 其实根本没查。
    unknown: '未验证',
  };

  function createProvisioningSection() {
    // 后端还没上这套设置项（老版本）时，整节不出现。
    // 与其画一个点了必报错的面板，不如干脆不给——运营看到按钮就会去点。
    if (!state.settings || !('sub2api_base_url' in state.settings)) return null;

    const summaryRoot = h('div', { class: 'dk-provision-summary', 'aria-live': 'polite' });
    const checkRoot = h('div', { class: 'dk-provision-check', 'aria-live': 'polite' });
    let testButton;

    const controller = createSettingsController({
      id: 'provisioning-settings',
      title: '网关自动开通',
      description: '新成员建号时，系统自动到图片网关那边替他开一个账号、领一把自己的 Key，'
        + '省掉你手工发 Key 这一步。开不通不影响任何人登录和使用，只是生图要等你手工发。',
      keys: ['sub2api_auto_provision', 'sub2api_base_url', 'sub2api_admin_key',
        'sub2api_group_id', 'sub2api_email_domain', 'sub2api_keep_password',
        'sub2api_max_attempts'],
      renderFields: (form) => {
        const enabled = h('input', { type: 'checkbox' });
        const baseUrl = h('input', { class: 'input', type: 'url', autocomplete: 'off', placeholder: 'http://192.168.31.235:3000' });
        // 管理员 Key 沿用项目里已有的打码约定：后端回吐的是 8 个星号（+ 末 4 位），
        // 原样提交后端会认出这是占位符、判定为「没改」，不会覆盖原值。
        // 所以这里是 type=password 而不是普通输入框——不是怕管理员看，
        // 是怕他身后站着人，以及怕浏览器把它记进自动填充。
        const adminKey = h('input', { class: 'input', type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: '粘贴网关后台生成的管理员 Key' });
        const groupId = h('input', { class: 'input', type: 'text', inputmode: 'numeric', autocomplete: 'off', placeholder: '例如 1' });
        const emailDomain = h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', placeholder: 'designkit.local' });
        const keepPassword = h('input', { type: 'checkbox' });
        const maxAttempts = h('input', { class: 'input', type: 'number', min: 1, max: 10, inputmode: 'numeric' });

        form.register('sub2api_auto_provision', enabled, checkboxBinding(false));
        form.register('sub2api_base_url', baseUrl);
        form.register('sub2api_admin_key', adminKey);
        form.register('sub2api_group_id', groupId);
        form.register('sub2api_email_domain', emailDomain);
        form.register('sub2api_keep_password', keepPassword, checkboxBinding(false));
        form.register('sub2api_max_attempts', maxAttempts, numberBinding());

        return h('div', { class: 'dk-panel-stack' },
          summaryRoot,
          field('自动给新成员开通',
            h('label', { class: 'dk-checkbox-row' }, enabled,
              h('span', { class: 'dk-checkbox-row__copy' }, '打开（建议先把下面四项填好再打开）',
                h('small', { class: 'dk-checkbox-row__description' },
                  '关着的时候，新成员建号后照常能登录、能进工作台，只是生图前要你到'
                  + '「成员账号」页给他填一把 Key。打开之后系统会自己去办，办不成还是回到手工发 Key。'))),
            { help: '这个开关只影响「以后新建的成员」和「还没开通成功的成员」，已经能生图的人不受任何影响。' }),
          h('div', { class: 'dk-field-grid' },
            field('网关地址', baseUrl, {
              help: '就是图片网关管理后台的地址，形如 http://192.168.31.235:3000。'
                + '注意它和上面「生图服务」那一节填的接口地址不是同一个：那个是发图的地址（带 /v1），这个是后台的地址。',
            }),
            field('网关管理员 Key', adminKey, {
              help: '在网关后台自己生成一把管理员 Key 贴进来。已保存的值只显示星号，不改它就不会被覆盖。'
                + '这把 Key 只用来建账号和查状态，系统不会拿它去充值或删任何东西。',
            }),
            field('目标分组 id', groupId, {
              help: '填一个数字，就是网关后台里那个分组的编号。填错的表现是成员开通到最后一步失败，'
                + '提示「没绑分组」——那时回来改这里就行。',
            }),
            field('成员邮箱后缀', emailDomain, {
              help: '系统会用「dk + 成员编号 + @这个后缀」在网关那边建账号，例如 dk7@designkit.local。'
                + '这个邮箱不用真实存在，也不会收信；填一个你自己认得出来的就行，不要用 .invalid 结尾（网关把它当保留后缀）。',
            })),
          field('保管成员在网关的登录密码',
            h('label', { class: 'dk-checkbox-row' }, keepPassword,
              h('span', { class: 'dk-checkbox-row__copy' }, '不保管（默认，更安全）时请保持关闭',
                h('small', { class: 'dk-checkbox-row__description' },
                  '关着：开通成功后系统立刻把那个密码删掉，以后只留着一把 Key。'
                  + '代价是这个成员将来要换一把新 Key 的话，系统自己办不了，只能你手工填。'
                  + '开着：密码一直存着（加密存放），将来能自动重新建 Key，但系统里就长期躺着一堆能登录网关的凭据。'))),
            { help: '不管开还是关，界面上都永远看不到这个密码——任何接口都不返回它，这不是藏起来，是压根没有这个功能。' }),
          field('最多自动重试几次', maxAttempts, {
            help: '范围 1–10。连着失败这么多次之后，这个成员就不再自动重试了，'
              + '会出现在「成员账号」页并标成「需要手工处理」，等你去点「重新开通」或手工填一把 Key。',
          }),
          checkRoot);
      },
      validate: (draft) => {
        const on = Boolean(draft.sub2api_auto_provision);
        const url = String(draft.sub2api_base_url || '').trim();
        const groupId = String(draft.sub2api_group_id || '').trim();
        const domain = String(draft.sub2api_email_domain || '').trim();
        if (url && !isHttpUrl(url)) return '网关地址要以 http:// 或 https:// 开头，例如 http://192.168.31.235:3000。';
        if (groupId && !/^\d+$/.test(groupId)) return '目标分组 id 只能填数字。';
        if (domain) {
          // 这三条规则要和后端 settings_router 的 _EMAIL_DOMAIN_RE /
          // _RESERVED_EMAIL_DOMAIN_SUFFIXES 逐条对齐。前端先拦一道只是为了当场
          // 给出中文提示，说了算的仍然是后端；但两边不一致的话，管理员会看到
          //「前端说可以、点保存又被退回来」，那比不校验还糟。
          if (domain.length > 100 || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/.test(domain)) {
            return '成员邮箱后缀只能用小写字母、数字、减号和点，中间要带一个点，例如 designkit.local。';
          }
          // 网关自己占了几个 *-connect.invalid 后缀，撞上会被建号策略直接拒掉，
          // 而报错只说「邮箱不被允许」，完全看不出是后缀的问题；
          // .localhost / .example / .test 是 RFC 保留给文档和测试的，同理。
          const reserved = ['.invalid', '.localhost', '.example', '.test'].find((s) => domain.endsWith(s));
          if (reserved) return `成员邮箱后缀不能用 ${reserved} 结尾，这是保留后缀，网关会拒绝建号。换成 designkit.local 这种就行。`;
        }
        if (!integerBetween(draft.sub2api_max_attempts, 1, 10)) return '最多自动重试次数要填 1–10 之间的整数。';
        // 开关打开时才强制要求填齐：没打开的时候允许先存半截，
        // 否则管理员想先把地址存下来、明天再拿 Key 都做不到。
        if (on) {
          if (!isHttpUrl(url)) return '要打开自动开通，先把网关地址填好（以 http:// 或 https:// 开头）。';
          if (!String(draft.sub2api_admin_key || '').trim()) return '要打开自动开通，先把网关管理员 Key 填好。';
          if (!groupId) return '要打开自动开通，先把目标分组 id 填好（一个数字）。';
          if (!domain) return '要打开自动开通，先把成员邮箱后缀填好，例如 designkit.local。';
        }
        return '';
      },
      extraActions: (form) => {
        testButton = button('测试能不能自动开通', {
          variant: 'secondary',
          iconName: 'plug',
          onclick: async () => {
            if (form.dirty) {
              form.setStatus('请先保存当前更改，测试只使用已保存的配置。', 'warning');
              return;
            }
            testButton.disabled = true;
            testButton.setAttribute('aria-busy', 'true');
            checkRoot.replaceChildren(inlineAlert('正在检查…（只查看网关那边的设置，不建账号、不发 Key、不花钱）', 'info'));
            try {
              // 这条接口和「测试连接」一样永远返回 HTTP 200，成败看响应体，
              // 不能靠 catch 判断——真挂了才会走到 catch。
              const result = await api.post('/api/web/settings/test_provisioning');
              if (state.stopped) return;
              renderCheck(result);
              // 自检发现「前提条件没了」时后端会当场把总开关关掉（paused_now）。
              // 页面上那个勾还停在打开的位置，管理员保存一次别的字段就会把它又打开，
              // 于是 worker 继续撞墙。所以拿返回值把开关和基线一起对回去。
              controller.applyServerValues({ sub2api_auto_provision: result.auto_provision });
              if (result.summary) renderSummary(result.summary);
              else loadSummary();
            } catch (error) {
              if (state.stopped) return;
              checkRoot.replaceChildren(inlineAlert(error.message, 'error', { title: '检查没跑起来' }));
            } finally {
              testButton.disabled = false;
              testButton.removeAttribute('aria-busy');
            }
          },
        });
        return [testButton];
      },
    });

    // ── 面板顶部：全局暂停红条 + 各状态人数 ──
    function renderSummary(data) {
      if (state.stopped) return;
      const counts = (data && data.counts) || {};
      const stuck = (data && data.stuck) || [];
      const nodes = [];
      if (data && data.halted) {
        nodes.push(h('div', { class: 'dk-panel-stack' },
          inlineAlert(
            (data.halt_reason || '自检发现网关那边有前提条件不满足。')
            + (data.halted_at ? `（暂停时间：${fmtServerTime(data.halted_at)}）` : ''),
            'error',
            { title: '自动开通已暂停，新成员现在开不出来' },
          ),
          h('div', { class: 'dk-inline-actions' },
            // 名字要和后端提示里写的逐字一致：自检结论里写着
            //「请点「我已处理，恢复自动开通」」，这里叫别的名字就等于指了个假路。
            button('我已处理，恢复自动开通', {
              variant: 'secondary',
              iconName: 'refresh-cw',
              onclick: resumeHalt,
            }))));
      }
      // 口径要和「成员账号」页那一列逐字对上，否则两个页面的数字对不上，
      // 管理员会以为系统在骗他。
      // stuck 的定义（见后端 provisioning.summary）：开通失败的 + 转了手工但还没填 Key 的。
      // 所以「手工配置」要把 stuck 里那些 manual 的人扣掉，不能直接用 counts.manual。
      const active = Number(counts.active || 0);
      const working = Number(counts.pending || 0) + Number(counts.user_created || 0) + Number(counts.key_issued || 0);
      const manualStuck = stuck.filter((item) => item.state === 'manual').length;
      const manual = Math.max(0, Number(counts.manual || 0) - manualStuck);
      nodes.push(h('p', { class: 'dk-section-meta' },
        `目前：已开通 ${active} 人 · 开通中 ${working} 人 · 手工配置 ${manual} 人 · 需要手工处理 ${stuck.length} 人。`
        // 按钮在「成员账号」页叫「重新开通」，这里就得叫同一个名字。
        // 指路指错名字，非技术用户会在页面上反复找一个不存在的按钮。
        + (stuck.length ? '需要你处理的那几个人，在左侧「成员账号」页会标出来，那里可以直接点「重新开通」或手工填一把 Key。' : '')));
      // config_problem 是后端算好的一句人话（「还没填目标分组 id」这种）。
      // 它比自检更早能告诉管理员缺什么——不用等他点按钮。
      if (data && data.config_problem) {
        nodes.push(inlineAlert(`${data.config_problem}。填好并保存之后，自动开通才会真的开始干活。`, 'warning'));
      }
      summaryRoot.replaceChildren(...nodes);
    }

    async function loadSummary() {
      try {
        const data = await api.get('/api/web/settings/provisioning');
        if (state.stopped) return;
        renderSummary(data);
        // 60 秒内跑过自检的话，后端会把那次结果一起带回来。直接画出来，
        // 省得管理员刷一次页面就以为「还没查过」，然后又点一次按钮。
        if (data.selfcheck) renderCheck(data.selfcheck);
      } catch {
        // 概览拿不到不影响改设置，安静跳过：这里蹦一个红条只会吓到人，
        // 而他此刻要做的事（填地址、填 Key）一件都不受影响。
        if (!state.stopped) summaryRoot.replaceChildren();
      }
    }

    async function resumeHalt() {
      try {
        const data = await api.post('/api/web/settings/provisioning/resume');
        if (state.stopped) return;
        renderSummary(data);
        // 这个接口除了解暂停，还会把总开关重新打开（两处都关着是暂停时的常态）。
        // 不把页面上那个勾同步回去的话，管理员下次保存会又把它关掉，
        // 表现就是「刚恢复完，一保存又停了」。
        controller.applyServerValues({ sub2api_auto_provision: true });
        toast(data.message || '自动开通已恢复', 'success');
      } catch (error) {
        // 配置没填齐时后端会 422 并说清缺什么，原样转给管理员
        if (!state.stopped) toast(error.message, 'error');
      }
    }

    /** 自检结果：三色灯 + 逐项结论 + 每项从网关读回来的实际取值。
     *
     * 字段全部来自后端（settings_router 的 _item / _run_selfcheck），前端不再自己
     * 从 detail 里拼句子——同一个字段名在不同检查项里含义不一样（「公开设置」的
     * version 是网关版本，「合规确认」的 version 是合规承诺版本），在前端拼必然拼错。
     * 后端已经把每项的 actual / expected 写成人话了，照抄就好。
     *
     * 另外：detail 里那些名字带 key / token / password / email 的字段后端已经滤掉了
     *（见 _SENSITIVE_DETAIL_HINTS）。这里也**不显示 detail**，只显示 actual / expected，
     * 双保险——将来后端漏了一个字段，界面这边也不会把它捅出去。
     */
    function renderCheck(result) {
      const level = String(result.level || 'unknown');
      const probes = Array.isArray(result.probes) ? result.probes : [];
      const actions = Array.isArray(result.actions) ? result.actions : [];

      checkRoot.replaceChildren(
        h('div', { class: 'dk-provision-result', 'data-level': level },
          h('div', { class: 'dk-provision-result__head' },
            h('span', { class: 'dk-provision-lamp', 'data-level': level, 'aria-hidden': 'true' }),
            h('strong', {}, OVERALL_TEXT[level] || '检查完成'),
            h('span', { class: 'dk-provision-result__time' },
              (result.checked_at ? `检查时间 ${fmtServerTime(result.checked_at)}` : '')
              // cached=true 表示这是 60 秒内那次的结果（打开页面时带回来的），
              // 不说清楚的话管理员会以为是刚刚重新查的。
              + (result.cached ? '（上一次的结果）' : ''))),
          result.message ? h('p', { class: 'dk-provision-result__message' }, result.message) : null,
          // 后端在发现「前提条件没了」时会当场暂停自动开通。这件事比任何一项检查
          // 结论都重要，单独用红条讲，别让它混在列表里被划过去。
          result.paused_now
            ? inlineAlert('为了不让所有新成员一直白撞墙，系统已经自动把「自动给新成员开通」暂时关掉了。'
              + '按下面说的处理完，再点上面的「我已处理，恢复自动开通」。', 'error',
            { title: '自动开通已被暂停' })
            : null,
          // 自检查的是「网关那边行不行」，跟总开关是两件事。全绿但开关关着的时候
          // 只写一句「可以自动开通」，管理员会以为已经在办了，然后等一个永远不来的结果。
          // 用**后端这次回的**值判断，不是页面上那个还没保存的勾。
          result.configured && !result.auto_provision && !result.paused_now
            ? inlineAlert('网关这边没问题，但上面那个「自动给新成员开通」的开关现在是关着的，'
              + '系统不会真的去开通。要用的话把它打开并保存。', 'warning')
            : null,
          actions.length
            ? h('div', { class: 'dk-provision-facts' },
              h('h3', {}, '要你去做的事'),
              h('ul', {}, actions.map((line) => h('li', {}, line))))
            : null,
          probes.length
            ? h('ul', { class: 'dk-provision-probes' }, probes.map((probe) => h('li', { 'data-level': probe.level || 'unknown' },
              h('span', { class: 'dk-provision-lamp', 'data-level': probe.level || 'unknown', 'aria-hidden': 'true' }),
              h('div', {},
                h('strong', {}, probe.name || '检查项'),
                // 等级文字照后端给的写。「未验证」必须原样出现——它不是绿也不是红，
                // 是「这次根本没查」，画成绿会让管理员以为查过了。
                h('span', { class: 'dk-provision-probes__tag', 'data-level': probe.level || 'unknown' },
                  probe.level_text || OVERALL_TEXT[probe.level] || ''),
                h('p', {}, probe.summary || ''),
                // 「实际」是刚从这台网关上读回来的真值，「期望」是该长什么样。
                // 两行并排，管理员不用懂技术也能自己比出哪里不对。
                probe.actual === undefined || probe.actual === null || probe.actual === ''
                  ? null
                  : h('p', { class: 'dk-provision-probes__fact' }, `实际：${probe.actual}`),
                probe.expected
                  ? h('p', { class: 'dk-provision-probes__fact' }, `应该是：${probe.expected}`)
                  : null))))
            : inlineAlert('这台服务的自检接口没有返回检查明细，只能给出上面这个结论。', 'info')),
      );
    }

    loadSummary();
    return controller;
  }

  /** 开放注册的三条硬前置条件，按当前草稿算一遍还差哪几条。
   *
   * **邀请码注册和手机号注册共用这一份**（后端 _open_register_blockers 也是共用的）。
   * 文案要和后端那三条一一对应——两边写岔了的表现是「界面显示全绿、保存却被拒」，
   * 那比不显示还糟：她会以为是系统出了故障，而不是自己还差一步。
   */
  function openRegisterBlockers(draft) {
    const list = [];
    if (draft.allow_internal_targets) {
      list.push('「安全与网络」那一节的**允许访问内网图片和回调地址**还开着，请先关掉。'
        + '开着它，注册进来的陌生人可以让本系统替他去访问你内网里的任意地址——'
        + '包括生图网关的管理端口，那上面能建号、改余额、读别人的 Key。');
    }
    const url = String(draft.public_base_url || '').trim();
    if (!isHttpUrl(url) || /^https?:\/\/(127\.0\.0\.1|localhost)(:|\/|$)/i.test(url)) {
      list.push('「安全与网络」那一节的**对外访问地址**还不能用。'
        + '新用户拿到的图片链接和回调地址都按它拼，填成 127.0.0.1 的话别人打不开，'
        + '而系统这边不会有任何报错。');
    }
    if (!draft.files_signed_only) {
      list.push('「图片访问与多用户」那一节的**图片必须带凭证才能访问**是关的，请打开。'
        + '关着它，只要知道图片地址就能下载任何人的商品图——'
        + '内部几个人用还能凑合，谁都能注册之后就等于谁都能进来翻别人的图。');
    }
    return list;
  }

  /** 自助注册（邀请码）。
   *
   * 这一节存在的唯一理由，就是给管理员一个「把自助注册打开」的地方。
   * 后端的开关（self_register_enabled）、双向硬前置闸门、注册接口、邀请码接口
   * 早就做好了，但界面上一直没有开关——结果是「后端能开、管理员没地方开」，
   * 她在设置页翻遍了也找不到，只能得出「这功能没做」的结论。
   * 这是一次真实发生过的事故，别把这一节删掉。
   *
   * 开关旁边那份「前提条件」清单是刻意做的：后端拒绝时会返回一大段说明，
   * 但那要等她点了保存、被拒之后才看得到。先摆出来，她就不用先撞一次墙。
   * 清单的判断条件必须和后端 _self_register_blockers 保持一致——
   * 两边写岔了的表现是「界面显示全绿、保存却被拒」，比不显示还糟。
   */
  function createSelfRegisterSection() {
    // 后端还没有这个功能时（老镜像）整节不渲染，理由同 createProvisioningSection：
    // 画一个点了必报错的开关，比不画更让人困惑。
    if (!state.settings || !('self_register_enabled' in state.settings)) return null;

    const gateRoot = h('div', { class: 'dk-panel-stack', 'aria-live': 'polite' });

    // 三条前提是**两条注册路共用**的，所以清单抽在外面（openRegisterBlockers），
    // 手机号那一节用的是同一份。两处各抄一份的话，改了一处漏一处，
    // 表现是同一台机器上两节的清单互相矛盾。
    const blockers = openRegisterBlockers;

    // 「重新检查」按钮不是可有可无的装饰：这三条前提分别住在本页**另外两节**里，
    // 而每一节都是独立保存的（createSettingsController 保存后只同步自己那几个键，
    // 不重绘别的 section）。所以她在上面改完并保存之后，这里的清单不会自己变。
    // 没有这个按钮的话，她会看着一份过期的「还差 3 条」去怀疑自己没改对。
    function renderGate() {
      const draft = state.settings || {};
      const list = blockers(draft);
      const recheck = button('重新检查', {
        variant: 'ghost', iconName: 'refresh-cw', onclick: renderGate,
      });
      if (!list.length) {
        gateRoot.replaceChildren(inlineAlert(
          '三项前提都满足了，可以打开自助注册。打开之后记得到左侧「邀请码」页发码——'
          + '没有码谁也注册不进来。', 'success', { title: '前提条件已满足' }), recheck);
        return;
      }
      gateRoot.replaceChildren(inlineAlert(
        '还差下面 ' + list.length + ' 条，不满足的话开关保存不了：\n\n'
        + list.map((x, i) => (i + 1) + '. ' + x.replace(/\*\*/g, '')).join('\n\n')
        + '\n\n这几项都在本页上面两节里。改完那两节各自的「保存此区域」之后，'
        + '点一下下面的「重新检查」，这份清单才会更新。',
        'warning', { title: '打开之前要先满足三个前提' }), recheck);
    }

    const controller = createSettingsController({
      id: 'self-register-settings',
      title: '自助注册（邀请码）',
      description: '让新人自己拿邀请码注册，不用你一个个建号。注意它和上面「网关自动开通」'
        + '不是一回事：那个管的是「你建号之后系统替他去网关领 Key」，这个管的是「他能不能自己建号」。',
      keys: ['self_register_enabled', 'self_register_ip_per_hour', 'self_register_daily_limit',
        'invite_code_default_max_uses', 'invite_code_default_valid_days'],
      renderFields: (form) => {
        const enabled = h('input', { type: 'checkbox' });
        const ipPerHour = h('input', { class: 'input', type: 'number', min: 1, max: 1000, inputmode: 'numeric' });
        const dailyLimit = h('input', { class: 'input', type: 'number', min: 1, max: 10000, inputmode: 'numeric' });
        const defaultUses = h('input', { class: 'input', type: 'number', min: 1, max: 1000, inputmode: 'numeric' });
        const defaultDays = h('input', { class: 'input', type: 'number', min: 0, max: 365, inputmode: 'numeric' });

        form.register('self_register_enabled', enabled, checkboxBinding(false));
        form.register('self_register_ip_per_hour', ipPerHour, numberBinding());
        form.register('self_register_daily_limit', dailyLimit, numberBinding());
        form.register('invite_code_default_max_uses', defaultUses, numberBinding());
        form.register('invite_code_default_valid_days', defaultDays, numberBinding());

        renderGate();

        return h('div', { class: 'dk-panel-stack' },
          gateRoot,
          field('开放自助注册',
            h('label', { class: 'dk-checkbox-row' }, enabled,
              h('span', { class: 'dk-checkbox-row__copy' }, '打开（打开前先看上面那三条）',
                h('small', { class: 'dk-checkbox-row__description' },
                  '打开之后，登录页会多出一个「我有邀请码，去注册」的入口。'
                  + '没有邀请码的人注册不进来，所以你随时可以只发给想让他进来的人。'))),
            { help: '关掉之后注册入口立刻消失，已经注册进来的账号不受影响。' }),
          h('div', { class: 'dk-field-grid' },
            field('每个 IP 每小时最多注册几次', ipPerHour, {
              help: '防止有人写脚本刷注册。注册会连锁去网关建账号，被刷的话你的网关后台会被塞满垃圾账号。',
            }),
            field('全站每天最多注册几人', dailyLimit, {
              help: '再加一道总闸。填小一点更稳，不够了随时能改大。',
            }),
            field('新建邀请码：默认可用几次', defaultUses, {
              help: '在「邀请码」页建码时的默认值，建的时候还能单独改。填 1 就是一码一人。',
            }),
            field('新建邀请码：默认几天后过期', defaultDays, {
              help: '同上，是建码时的默认值。填 0 表示永不过期（不建议，发出去就收不回来了）。',
            })));
      },
    });
    return controller;
  }

  /* ── 短信服务（手机号 + 验证码注册）─────────────────────────────────
   *
   * 这一节和上面「网关自动开通」是同一个套路：前提条件清单摆在最上面、
   * 底下一个「试发一条」按钮。原因也一样——这两件事都要连一台外面的服务器，
   * 而「配置填好了」和「真的能用」是两回事，中间隔着签名审核、模板审核、
   * 账户余额、系统时钟这几样，光看设置页永远看不出来。
   *
   * 这一节独有的两条纪律：
   *
   * 一、**调试模式必须显著标出来。** 处在调试模式时，验证码根本不会发到手机上，
   *    而是直接显示在注册页面上。不标的话，管理员哪天忘了切回阿里云，
   *    她会一直以为短信发出去了，用户那边永远收不到——双方都查不出问题在哪。
   *    横幅的文案一律用后端给的（mode_text / notice / warning），不在这里另写。
   *
   * 二、**AccessKey 一律由管理员自己填。** 界面上不预填、代码里不写、
   *    测试数据里也没有。Secret 存进去之后只回一串星号，原样提交表示「没改」。
   */
  function createSmsSection() {
    // 后端还没上这套设置项（老镜像）时整节不出现，理由同上面几节：
    // 画一个点了必报错的面板，比不画更让人困惑。
    if (!state.settings || !('sms_provider' in state.settings)) return null;

    const statusRoot = h('div', { class: 'dk-panel-stack', 'aria-live': 'polite' });
    const gateRoot = h('div', { class: 'dk-panel-stack', 'aria-live': 'polite' });
    const testRoot = h('div', { class: 'dk-panel-stack', 'aria-live': 'polite' });
    const testPhone = h('input', {
      class: 'input',
      type: 'tel',
      inputmode: 'numeric',
      autocomplete: 'off',
      maxlength: '13',
      placeholder: '你自己的手机号',
    });
    let testButton;
    // 这一次打开设置页之后有没有真的点过「试发一条」。
    // 有的话，下面 renderStatus 就**不许**再拿服务端那份 last_test 覆盖结果区——
    // 覆盖的表现是：刚点完试发，结果后面立刻多出一句「（上一次试发的结果）」，
    // 管理员会以为自己这一次根本没发出去，于是又点一次（真实模式下就是又一条短信的钱）。
    let hasFreshTest = false;

    /** 上半张脸：现在是什么模式、有没有严重问题、还缺哪几项配置。
     * 全部照抄后端 sms_status 里的句子——它比前端更清楚「填了没有、解不解得开」。 */
    function renderStatus(status) {
      if (state.stopped) return;
      const data = status || {};
      const nodes = [];
      if (data.warning) {
        // warning 比 notice 严重（「注册开着却还停在调试模式」这种），
        // 用红条，别让它混在普通说明里被划过去。
        nodes.push(inlineAlert(data.warning, 'error', { title: '这个组合有问题，请先处理' }));
      }
      if (data.mode_text) {
        nodes.push(inlineAlert(data.mode_text, data.debug ? 'warning' : 'info',
          { title: data.debug ? '当前：调试模式（不发短信、不花钱）' : '当前：真实模式（阿里云）' }));
      }
      if (data.debug && data.notice) {
        // 这句话和注册页上显示给用户的那句是**同一份原文**，
        // 管理员在这里看到的和用户看到的完全一致，出问题时好对账。
        nodes.push(h('p', { class: 'dk-section-meta' }, `注册页上会显示这句话：${data.notice}`));
      }
      const problems = Array.isArray(data.problems) ? data.problems : [];
      if (problems.length) {
        // 调试模式下这几项本来就用不上，所以话要说成「切过去之前要填」，
        // 而不是「你少填了东西」——后者会让管理员以为现在就有故障，
        // 于是跑去填一份她这个阶段根本不需要的阿里云配置。
        nodes.push(inlineAlert(
          (data.debug
            ? '现在是调试模式，用不到这些。等你要真的发短信、把上面的通道切到阿里云时，'
              + '下面这几项必须先填齐（没填齐也切不过去）：\n\n'
            : '还差下面这几项，补齐之前一条短信也发不出去：\n\n')
          + problems.map((x, i) => `${i + 1}. ${x}`).join('\n'),
          'warning', { title: data.debug ? '要真的发短信的话，还差这几项' : '阿里云的配置没填齐' }));
      }
      const limits = data.limits || {};
      if (limits.resend_cooldown_seconds) {
        // 显示的是**实际生效值**（后端已经夹过范围），不是设置表里的原始值：
        // 两者不一致时（老库遗留、有人直接改了数据库），显示原始值会让管理员
        // 按一个根本没生效的数字去排查。
        nodes.push(h('p', { class: 'dk-section-meta' },
          `现在实际生效的限制：同一个号 ${limits.resend_cooldown_seconds} 秒才能再发一条、`
          + `每天最多 ${limits.phone_daily_limit} 条；同一个来源每小时最多 ${limits.ip_hourly_limit} 条；`
          + `全站每天最多 ${limits.global_daily_limit} 条。验证码 ${limits.code_ttl_seconds} 秒内有效，`
          + `最多能填错 ${limits.code_max_attempts} 次。`));
      }
      if (data.last_test && !hasFreshTest) renderTestResult(data.last_test, true);
      statusRoot.replaceChildren(...nodes);
    }

    /** 打开手机号注册之前要满足的前提。和「自助注册（邀请码）」那一节共用前三条，
     * 手机号这条路自己多一条：短信通道得是能用的。 */
    function renderGate() {
      const draft = state.settings || {};
      const list = openRegisterBlockers(draft);
      const smsStatus = draft.sms_status || {};
      if (!smsStatus.debug && !smsStatus.ready) {
        list.push('短信通道选的是阿里云，但**配置没填齐**（缺哪几项见上面那一条）。'
          + '这样打开手机号注册，每个人点「获取验证码」都会失败，'
          + '而他自己完全看不出是我们这边没配好。');
      }
      const recheck = button('重新检查', {
        variant: 'ghost', iconName: 'refresh-cw', onclick: renderGate,
      });
      if (!list.length) {
        gateRoot.replaceChildren(inlineAlert(
          '前提都满足了，可以打开手机号注册。'
          + '打开之前请再确认一次下面那四个限速数字——每一条验证码短信都是真花钱的。',
          'success', { title: '前提条件已满足' }), recheck);
        return;
      }
      gateRoot.replaceChildren(inlineAlert(
        '还差下面 ' + list.length + ' 条，不满足的话开关保存不了：\n\n'
        + list.map((x, i) => (i + 1) + '. ' + x.replace(/\*\*/g, '')).join('\n\n')
        + '\n\n和「自助注册（邀请码）」共用的那几条在本页上面几节里，'
        + '各自「保存此区域」之后回来点一下「重新检查」。',
        'warning', { title: '打开之前要先满足这几个前提' }), recheck);
    }

    /** 一次试发的结论。字段全部来自后端（见 settings_router 的 test_sms 文档）。 */
    function renderTestResult(result, cached) {
      const level = String(result.level || '');
      const nodes = [inlineAlert(
        `${result.message || ''}${cached ? '（上一次试发的结果）' : ''}`,
        level === 'green' ? 'success' : 'error',
        { title: result.ok ? '已经交给阿里云发送' : '这次没发出去' })];
      if (result.next_step) nodes.push(h('p', { class: 'dk-section-meta' }, `下一步：${result.next_step}`));
      if (result.note) nodes.push(h('p', { class: 'dk-section-meta' }, result.note));
      // 错误码 / RequestId / 回执号是管理员找阿里云客服时必须提供的东西，
      // 一定要显示出来，而且要能选中复制（普通文本即可）。
      const facts = [
        result.phone ? `发往：${result.phone}` : '',
        result.error_code ? `错误码：${result.error_code}` : '',
        result.request_id ? `RequestId：${result.request_id}` : '',
        result.provider_msg_id ? `回执号：${result.provider_msg_id}` : '',
        result.at ? `时间：${fmtServerTime(result.at)}` : '',
      ].filter(Boolean);
      if (facts.length) nodes.push(h('p', { class: 'dk-section-meta' }, facts.join(' · ')));
      testRoot.replaceChildren(...nodes);
    }

    const controller = createSettingsController({
      id: 'sms-settings',
      title: '短信服务（手机号注册）',
      description: '让新人用手机号 + 短信验证码自己注册。默认是「调试模式」——不发真短信、不花钱，'
        + '验证码直接显示在注册页上，用来先把整条流程走通。要真的发短信，才需要填下面的阿里云配置。',
      keys: ['sms_provider', 'sms_aliyun_access_key_id', 'sms_aliyun_access_key_secret',
        'sms_aliyun_sign_name', 'sms_aliyun_template_code', 'sms_aliyun_endpoint',
        'phone_register_enabled', 'phone_register_require_invite',
        'sms_code_ttl_seconds', 'sms_code_max_attempts',
        'sms_code_resend_cooldown_seconds', 'sms_code_phone_daily_limit',
        'sms_code_ip_hourly_limit', 'sms_code_global_daily_limit'],
      renderFields: (form) => {
        const provider = h('select', { class: 'dk-control input' },
          h('option', { value: 'debug' }, '调试模式（不发短信、不花钱）'),
          h('option', { value: 'aliyun' }, '阿里云短信（真的发、真的花钱）'));
        const accessKeyId = h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', placeholder: 'LTAI 开头的那一串' });
        // Secret 沿用项目里已有的打码约定：后端回吐的是一串星号，原样提交
        // 后端会认出这是占位符、判定为「没改」，不会覆盖原值。
        const accessKeySecret = h('input', { class: 'input', type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: '粘贴阿里云的 AccessKey Secret' });
        const signName = h('input', { class: 'input', type: 'text', autocomplete: 'off', placeholder: '例如 某某电商' });
        const templateCode = h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', placeholder: '例如 SMS_123456789' });
        const endpoint = h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', placeholder: 'dysmsapi.aliyuncs.com' });
        const registerEnabled = h('input', { type: 'checkbox' });
        const requireInvite = h('input', { type: 'checkbox' });
        const ttl = h('input', { class: 'input', type: 'number', min: 60, max: 1800, inputmode: 'numeric' });
        const maxAttempts = h('input', { class: 'input', type: 'number', min: 1, max: 10, inputmode: 'numeric' });
        const cooldown = h('input', { class: 'input', type: 'number', min: 60, max: 600, inputmode: 'numeric' });
        const phoneDaily = h('input', { class: 'input', type: 'number', min: 1, max: 20, inputmode: 'numeric' });
        const ipHourly = h('input', { class: 'input', type: 'number', min: 1, max: 200, inputmode: 'numeric' });
        const globalDaily = h('input', { class: 'input', type: 'number', min: 1, max: 2000, inputmode: 'numeric' });

        form.register('sms_provider', provider);
        form.register('sms_aliyun_access_key_id', accessKeyId);
        form.register('sms_aliyun_access_key_secret', accessKeySecret);
        form.register('sms_aliyun_sign_name', signName);
        form.register('sms_aliyun_template_code', templateCode);
        form.register('sms_aliyun_endpoint', endpoint);
        form.register('phone_register_enabled', registerEnabled, checkboxBinding(false));
        form.register('phone_register_require_invite', requireInvite, checkboxBinding(true));
        form.register('sms_code_ttl_seconds', ttl, numberBinding());
        form.register('sms_code_max_attempts', maxAttempts, numberBinding());
        form.register('sms_code_resend_cooldown_seconds', cooldown, numberBinding());
        form.register('sms_code_phone_daily_limit', phoneDaily, numberBinding());
        form.register('sms_code_ip_hourly_limit', ipHourly, numberBinding());
        form.register('sms_code_global_daily_limit', globalDaily, numberBinding());

        renderStatus(state.settings.sms_status);
        renderGate();

        return h('div', { class: 'dk-panel-stack' },
          statusRoot,
          field('短信通道', provider, {
            help: '默认「调试模式」：不连任何外部服务，验证码直接显示在注册页上，'
              + '适合先在内网把流程走一遍。切到阿里云之前，下面四项必须填齐，否则存不进去。',
          }),
          h('div', { class: 'dk-field-grid' },
            field('AccessKey Id', accessKeyId, {
              help: '在阿里云控制台建一个 RAM 用户、只给它发短信的权限，然后把这一对贴过来。'
                + '这一项相当于用户名，所以不打码——你要能看出自己填的是哪一把。',
            }),
            field('AccessKey Secret', accessKeySecret, {
              help: '这一项是密码，存进去之后只显示星号。不改它就不会被覆盖。'
                + '⚠ 它是阿里云账号级别的凭据，只有你自己该知道，别贴到聊天里。',
            }),
            field('短信签名', signName, {
              help: '阿里云控制台里审核通过的那个签名，必须一字不差（比如「某某电商」）。'
                + '差一个字就是发送失败，而失败原因是一串英文错误码。',
            }),
            field('模板 CODE', templateCode, {
              help: '形如 SMS_123456789。模板正文里必须带 ${code} 这个变量，'
                + '否则发出去的是一条没有验证码的短信。',
            })),
          field('短信接口域名', endpoint, {
            help: '正常情况下不要改，留空会自动用 dysmsapi.aliyuncs.com。',
          }),
          gateRoot,
          field('开放手机号注册',
            h('label', { class: 'dk-checkbox-row' }, registerEnabled,
              h('span', { class: 'dk-checkbox-row__copy' }, '打开（打开前先看上面那份清单）',
                h('small', { class: 'dk-checkbox-row__description' },
                  '打开之后，登录页和注册页会多出「用手机号注册」这条路。'
                  + '它和「自助注册（邀请码）」是并列的两条路，互不影响，可以只开一条。'))),
            { help: '关掉之后入口立刻消失，已经注册进来的人不受影响，照样能用手机号登录。' }),
          field('注册时还要填邀请码',
            h('label', { class: 'dk-checkbox-row' }, requireInvite,
              h('span', { class: 'dk-checkbox-row__copy' }, '要求填（默认，也更稳）',
                // 界面不渲染 Markdown，所以强调一律用【】，别写星号——
                // 写了会原样显示成星号（这一条在后端 test_send_warning 那里也写着）。
                h('small', { class: 'dk-checkbox-row__description' },
                  '⚠ 关掉之后，【任何人只要有一个能收短信的手机号就能注册进来】，没有任何名单限制。'
                  + '真要对外开放时才关，关之前先把下面四个限速数字和「自助注册」那一节的'
                  + '「全站每天最多注册几人」都调到你能承受的量——每个新账号都要去网关开通、都要花钱。'))),
            { help: '开着的时候，手机号只负责证明「这个号是你的」，能不能进来仍然由邀请码说了算。' }),
          h('div', { class: 'dk-field-grid' },
            field('验证码几秒内有效', ttl, {
              help: '范围 60–1800。默认 300 秒（5 分钟）：短信到达一般十几秒，5 分钟够一个人切回来填；'
                + '再长就是给「慢慢猜 6 位数」留时间。',
            }),
            field('一条码最多填错几次', maxAttempts, {
              help: '范围 1–10。默认 5 次：真人填错一两次很常见，但放得越宽，猜码越划算。',
            })),
          h('div', { class: 'dk-field-grid' },
            field('同一个号隔多少秒才能再发', cooldown, {
              help: '范围 60–600。默认 60 秒。注册页上的「重新获取」倒计时就是按它走的。'
                + '阿里云自己也限「同一个号 1 分钟 1 条」，填得比 60 小没有意义。',
            }),
            field('同一个号一天最多几条', phoneDaily, {
              help: '范围 1–20。默认 10 条。它挡的是「拿一个号当靶子反复发」——'
                + '机主一投诉，阿里云会直接封掉你的签名，那比多花几毛钱严重得多。',
            }),
            field('同一个来源每小时最多几条', ipHourly, {
              help: '范围 1–200。默认 20 条，留了「同一间办公室多人共用一个出口」的余地。',
            }),
            field('全站每天最多几条', globalDaily, {
              help: '范围 1–2000。默认 200 条，这是最后一道保险丝：万一前两道被绕过、'
                + '或者哪个数字填错了，一天的损失也就止于这个数（国内短信约 4 分钱一条），'
                + '第二天零点自动恢复。',
            })),
          // ⚠ 这句话必须紧挨着试发按钮：它是本页唯一一个**点一下就真的花钱**的
          // 按钮，而且**调试模式下也照发不误**——试发的意义就是验证阿里云那条
          // 链路通不通，而管理员需要它的时候，多半正是「还没切过去」的时候。
          h('div', { class: 'dk-sms-test' },
            h('p', { class: 'dk-section-meta dk-sms-test__warning' },
              state.settings?.sms_status?.test_send_warning
              || '点一下会真的发出一条短信并产生费用（约 4 分钱）。'),
            field('试发到这个手机号', testPhone, {
              help: '填你自己手上这台手机，发完看它有没有真的收到。'
                + '「阿里云受理了」不等于「手机收到了」——中间还隔着运营商。',
            })),
          testRoot);
      },
      validate: (draft) => {
        if (!integerBetween(draft.sms_code_ttl_seconds, 60, 1800)) return '「验证码几秒内有效」要填 60 到 1800 之间的整数。';
        if (!integerBetween(draft.sms_code_max_attempts, 1, 10)) return '「一条码最多填错几次」要填 1 到 10 之间的整数。';
        if (!integerBetween(draft.sms_code_resend_cooldown_seconds, 60, 600)) return '「同一个号隔多少秒才能再发」要填 60 到 600 之间的整数。';
        if (!integerBetween(draft.sms_code_phone_daily_limit, 1, 20)) return '「同一个号一天最多几条」要填 1 到 20 之间的整数。';
        if (!integerBetween(draft.sms_code_ip_hourly_limit, 1, 200)) return '「同一个来源每小时最多几条」要填 1 到 200 之间的整数。';
        if (!integerBetween(draft.sms_code_global_daily_limit, 1, 2000)) return '「全站每天最多几条」要填 1 到 2000 之间的整数。';
        // 切到阿里云要先填齐四项。后端也拦这一条，前端先说一遍是为了当场给出
        // 中文提示，而不是让她点了保存再被退回来。
        if (String(draft.sms_provider || '') === 'aliyun') {
          const missing = [
            String(draft.sms_aliyun_access_key_id || '').trim() ? '' : 'AccessKey Id',
            String(draft.sms_aliyun_access_key_secret || '').trim() ? '' : 'AccessKey Secret',
            String(draft.sms_aliyun_sign_name || '').trim() ? '' : '短信签名',
            String(draft.sms_aliyun_template_code || '').trim() ? '' : '模板 CODE',
          ].filter(Boolean);
          if (missing.length) return `要切到阿里云，先把这几项填好：${missing.join('、')}。`;
        }
        return '';
      },
      onSaved: (response) => {
        renderStatus(response.sms_status);
        renderGate();
      },
      extraActions: (form) => {
        testButton = button('试发一条', {
          variant: 'secondary',
          // 图标名必须在 vendor/lucide/icons.js 的子集里有，否则静默变成问号圆圈。
          iconName: 'upload',
          onclick: async () => {
            if (form.dirty) {
              form.setStatus('请先保存当前更改，试发只会使用已经保存的配置。', 'warning');
              return;
            }
            const phone = testPhone.value.trim();
            if (!phone) {
              form.setStatus('请先在按钮左边填一个你自己的手机号，短信会真的发到那台手机上。', 'warning');
              testPhone.focus();
              return;
            }
            testButton.disabled = true;
            testButton.setAttribute('aria-busy', 'true');
            testRoot.replaceChildren(inlineAlert('正在发送…', 'info'));
            try {
              // 这条接口和「测试连接」一样：发送成败一律 HTTP 200，看响应体判断。
              // 走到 catch 说明是号码填错（422）或被限速挡下（429），
              // 那时**一条短信都没发出去**，两种情况的说法完全不同。
              const result = await api.post('/api/web/settings/test_sms', { phone });
              if (state.stopped) return;
              hasFreshTest = true;
              renderTestResult(result, false);
              if (result.status) renderStatus(result.status);
            } catch (error) {
              if (state.stopped) return;
              testRoot.replaceChildren(inlineAlert(error.message, 'error', { title: '这一条没发出去' }));
            } finally {
              testButton.disabled = false;
              testButton.removeAttribute('aria-busy');
            }
          },
        });
        // 只把按钮交给 createSettingsController：它会在保存期间把这些控件
        // 一起禁用掉（存到一半又点试发是没有意义的）。手机号那一栏和警告语
        // 画在表单里，紧挨着结果区。
        return [testButton];
      },
    });
    return controller;
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

  /** onSaved：保存成功之后拿服务端的完整响应再做一次自己的事。
   *
   * 「短信服务」那一节需要它：那一节顶上的状态面板（现在是不是调试模式、
   * 还缺哪几项配置、限速的实际生效值）来自响应里的 sms_status，而那是**后端算的**，
   * 不是表单里的值。不重画的话，管理员刚把通道切到阿里云、保存成功，
   * 顶上那条横幅还写着「现在是调试模式」——她会以为没存进去，于是再存一遍。
   */
  function createSettingsController({ id, title, description, keys, renderFields, validate, extraActions, onSaved }) {
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
        onSaved?.(response);
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
      /** 服务端自己改了某几项设置时，把控件、baseline、draft 一起对回去。
       *
       * 「网关自动开通」那一节需要它：自检发现前提条件没了会当场关掉总开关，
       * 点「恢复自动开通」又会重新打开——这两件事都发生在后端。
       * 只改控件不改 baseline/draft 的话，页面会以为「用户手动改了这个勾」，
       * 管理员随手保存一次就把服务端刚做的决定又推翻了。
       */
      applyServerValues(values) {
        Object.entries(values || {}).forEach(([key, value]) => {
          if (!keys.includes(key) || value === undefined) return;
          baseline[key] = value;
          draft[key] = value;
          state.settings[key] = value;
          const registered = bindings.get(key);
          if (registered) registered.binding.write(registered.control, value);
        });
        syncDirty();
      },
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
