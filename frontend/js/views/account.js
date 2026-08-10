// 我的账户（所有人都能进）：我是谁、我现在能不能生图、改密码、退出其他所有设备。
//
// 这一页上**没有、也不该有任何 API Key 输入框**。这不是漏做，理由见
// backend/app/routers/account.py 的文件头：成员的生图额度是管理员在后台替他开通的，
// 那把 Key 从头到尾不是他自己申请的。给他一个输入框，只会多出一条
// 「密钥在聊天记录里流转」的路径，或者他填错东西把自己的账号搞坏。
// 需要开通 / 换 Key，请管理员去「成员账号」页操作。
import { api, clearSession, getSessionEpoch, getUser, setSession } from '../api.js';
import { button, confirmDialog, field, h, icon, inlineAlert, skeleton, toast } from '../ui.js';

const MIN_PASSWORD_LEN = 8;

export function renderAccount(container) {
  const state = { account: null, stopped: false };
  const page = h('section', { class: 'dk-page dk-page--account', 'aria-labelledby': 'account-page-title' });
  const stack = h('div', { class: 'dk-account-stack', 'aria-busy': 'true' }, skeleton({ lines: 8, className: 'dk-loading' }));
  page.append(h('h1', { id: 'account-page-title', class: 'dk-visually-hidden' }, '我的账户'), stack);
  container.append(page);

  async function load() {
    try {
      const account = await api.get('/api/web/account');
      if (state.stopped) return;
      state.account = account;
      // 这里**不**回写 localStorage 里的身份：app.js 每次渲染前都会调一次
      // /api/web/auth/me 把它刷新好了。在这里再写一次只会让「会话计数」白白 +1，
      // 把页面上其他正在飞的请求判成「用户已经换人了」而被丢弃。
      render();
    } catch (error) {
      if (state.stopped) return;
      stack.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载账户信息' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
    } finally {
      stack.setAttribute('aria-busy', 'false');
    }
  }

  function render() {
    stack.replaceChildren(profileSection(), gatewaySection(), passwordSection(), devicesSection());
  }

  function sectionHeader(id, title, description) {
    return h('div', { class: 'dk-settings-section__head' },
      h('div', { class: 'dk-settings-section__copy' },
        h('h2', { id, class: 'dk-section-title' }, title),
        h('p', { class: 'dk-section-meta' }, description)));
  }

  function profileSection() {
    const account = state.account;
    return h('section', { class: 'dk-settings-section', 'aria-labelledby': 'account-profile-title' },
      sectionHeader('account-profile-title', '我的信息', '显示名和角色由管理员设定，需要修改请联系管理员。'),
      h('dl', { class: 'dk-account-facts' },
        h('div', {}, h('dt', {}, '显示名'), h('dd', {}, account.display_name || account.username)),
        h('div', {}, h('dt', {}, '登录用户名'), h('dd', {}, h('code', {}, account.username))),
        h('div', {}, h('dt', {}, '角色'), h('dd', {}, account.role === 'admin' ? '管理员' : '成员'))));
  }

  function gatewaySection() {
    const gateway = state.account.gateway || {};
    // **只看 can_generate，不要看 status**：模拟生图模式、或者「共用一把 Key」的
    // 计费方式下，status 是 not_configured 但照样能正常生图。
    // 拿 status 当开关会让用户白跑一趟去找管理员。
    const ok = Boolean(gateway.can_generate);
    const statusRoot = h('div', { class: 'dk-account-test-result', 'aria-live': 'polite' });

    const testProvider = button('测试生图连接', {
      variant: 'secondary',
      iconName: 'plug',
      onclick: () => runTest(testProvider, '/api/web/account/test_provider', '正在测试生图连接…（只查一次模型列表，不出图、不花钱）'),
    });
    const testText = button('测试写提示词的模型', {
      variant: 'secondary',
      iconName: 'wand-sparkles',
      onclick: () => runTest(testText, '/api/web/account/test_text_model', '正在测试文本模型…（只发一句话）'),
    });

    async function runTest(target, path, waitingText) {
      statusRoot.replaceChildren(inlineAlert(waitingText, 'info'));
      target.disabled = true;
      target.setAttribute('aria-busy', 'true');
      try {
        // 这两条接口永远返回 HTTP 200，成败看响应体的 ok，不能靠 catch 判断
        const result = await api.post(path);
        if (state.stopped) return;
        statusRoot.replaceChildren(inlineAlert(result.message, result.ok ? 'success' : 'error'));
      } catch (error) {
        if (state.stopped) return;
        statusRoot.replaceChildren(inlineAlert(error.message, 'error'));
      } finally {
        target.disabled = false;
        target.removeAttribute('aria-busy');
      }
    }

    return h('section', { class: 'dk-settings-section', 'aria-labelledby': 'account-gateway-title' },
      sectionHeader('account-gateway-title', '生图额度', '这里回答一个问题：我现在点「生成」会不会被拦住。'),
      h('div', { class: 'dk-account-gateway', 'data-ok': ok ? 'true' : 'false' },
        h('span', { class: 'dk-account-gateway__icon', 'aria-hidden': 'true' }, icon(ok ? 'circle-check' : 'circle-alert', { size: 22 })),
        h('div', {},
          h('strong', {}, ok ? '可以生图' : '暂时不能生图'),
          h('p', {}, ok
            ? '你的账号已经可以正常提交生成任务。'
            // 不能生图时后端给的 message 已经是中文人话，自带「找谁、做什么」，原样显示
            : (gateway.message || '你的账号还没有开通生图额度，请联系管理员在「成员账号」页配置。')))),
      statusRoot,
      h('div', { class: 'dk-settings-section__actions' }, testProvider, testText));
  }

  function passwordSection() {
    const oldPassword = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
    const newPassword = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const confirmPassword = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const status = h('div', { class: 'dk-settings-section__status', 'aria-live': 'polite' });
    const saveButton = button('修改密码', { iconName: 'lock' });

    function fail(message, input) {
      status.dataset.state = 'error';
      status.textContent = message;
      input.focus();
    }

    saveButton.addEventListener('click', async () => {
      status.textContent = '';
      delete status.dataset.state;
      if (!oldPassword.value) return fail('请填写当前密码。', oldPassword);
      if (newPassword.value.length < MIN_PASSWORD_LEN) return fail(`新密码至少需要 ${MIN_PASSWORD_LEN} 位。`, newPassword);
      if (newPassword.value !== confirmPassword.value) return fail('两次输入的新密码不一致。', confirmPassword);
      const sessionAtSubmit = getSessionEpoch();
      saveButton.disabled = true;
      saveButton.setAttribute('aria-busy', 'true');
      status.dataset.state = 'saving';
      status.textContent = '正在修改密码…';
      try {
        const result = await api.post('/api/web/auth/change_password', {
          old_password: oldPassword.value,
          new_password: newPassword.value,
        });
        if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
        if (result.token) setSession(result.token, { ...(getUser() || {}), must_change_password: false });
        oldPassword.value = '';
        newPassword.value = '';
        confirmPassword.value = '';
        status.dataset.state = 'success';
        status.textContent = '密码已修改。其他设备上的登录已经失效，这台设备不受影响。';
        toast('密码已修改', 'success');
      } catch (error) {
        if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
        status.dataset.state = 'error';
        status.textContent = error.message;
      } finally {
        saveButton.disabled = false;
        saveButton.removeAttribute('aria-busy');
      }
    });

    return h('section', { class: 'dk-settings-section', 'aria-labelledby': 'account-password-title' },
      sectionHeader('account-password-title', '修改密码', '改完之后，你在其他设备上的登录会立刻失效，当前这台不受影响。'),
      h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); saveButton.click(); } },
        h('div', { class: 'dk-field-grid' },
          field('当前密码', oldPassword, { required: true }),
          field('新密码', newPassword, { required: true, help: `至少 ${MIN_PASSWORD_LEN} 位，建议用一句好记的长句加数字。` }),
          field('确认新密码', confirmPassword, { required: true })),
        h('div', { class: 'dk-settings-section__actions' }, status, saveButton)));
  }

  function devicesSection() {
    const status = h('div', { class: 'dk-settings-section__status', 'aria-live': 'polite' });
    // 按钮上写的是「其他」而不是「所有」，因为后端就是这么做的：当前这台会换发新凭证、
    // 继续留在登录状态（见 backend/app/routers/account.py 的 logout_all）。
    // 写成「退出所有设备」而人还留在页面上，用户会以为按钮没生效、于是反复点。
    const logoutAll = button('退出其他所有设备', {
      variant: 'danger',
      iconName: 'log-out',
      onclick: async () => {
        // 二次确认：这个动作会把别处正在做图的会话当场踢掉，而且撤不回来（只能各自重新登录）。
        // 注意：confirmDialog 的正文是纯文本（放进一个 <p> 里），换行和 ** 之类的记号
        // 都不会生效，只能写成一段连贯的话。
        const confirmed = await confirmDialog(
          '会把你在手机、平板、别人电脑等其他所有地方的登录同时踢下线，'
          + '别人手上已经拿到的图片链接也会立刻打不开；别人正在做的图会被中断，这一步撤不回来。'
          + '你正在用的这台不受影响，不用重新登录。'
          + '如果是怀疑账号被别人用了，踢完之后请顺手把密码也改掉，'
          + '否则对方知道密码，还能再登进来一次。',
          { danger: true, okText: '退出其他所有设备', title: '确认让其他设备下线' },
        );
        if (!confirmed || state.stopped) return;
        const sessionAtSubmit = getSessionEpoch();
        logoutAll.disabled = true;
        logoutAll.setAttribute('aria-busy', 'true');
        status.dataset.state = 'saving';
        status.textContent = '正在让其他设备下线…';
        try {
          const result = await api.post('/api/web/account/logout_all');
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          // 后端把 token_version 加了一，手上这张旧令牌已经作废，必须立刻换成新的，
          // 否则下一个请求就是 401，自己把自己踢回登录页。
          if (result && result.token) {
            // 只换令牌，身份信息原样留着（这个动作不改显示名/角色，也不涉及改密状态；
            // app.js 每次渲染前都会调一次 /me 把它刷新好）。
            setSession(result.token, getUser() || {});
            status.dataset.state = 'success';
            status.textContent = '其他设备上的登录已经全部失效，你正在用的这台不受影响。'
              + '如果怀疑账号被别人用了，请顺手用上面的「修改密码」换一个密码。';
            toast('其他设备已全部退出', 'success');
          } else {
            // 后端没给新令牌（理论上不会发生）。这时手上的令牌已经作废了，
            // 与其让用户在页面上一个接一个地撞 401，不如老老实实回登录页。
            clearSession();
            toast('其他设备已退出，请重新登录', 'success');
            window.dispatchEvent(new Event('dk-unauthorized'));
          }
        } catch (error) {
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          status.dataset.state = 'error';
          status.textContent = error.message;
        } finally {
          logoutAll.disabled = false;
          logoutAll.removeAttribute('aria-busy');
        }
      },
    });

    return h('section', { class: 'dk-settings-section', 'aria-labelledby': 'account-devices-title' },
      sectionHeader('account-devices-title', '登录设备',
        '右上角的「退出登录」只退出这一台。怀疑账号被别人用了，就点下面这个。'),
      h('div', { class: 'dk-panel-stack' },
        inlineAlert('会让你在手机、电脑等其他所有地方的登录同时失效，'
          + '别人手上已经拿到的图片链接也会立刻打不开；你正在用的这台不受影响，不用重新登录。'
          + '别人正在做的图会被中断，这一步撤不回来。', 'warning'),
        h('div', { class: 'dk-settings-section__actions' }, status, logoutAll)));
  }

  load();
  return () => { state.stopped = true; };
}
