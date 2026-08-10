// 成员账号（只有管理员看得到）：列表 + 建号 + 停用/启用 + 重置密码 + 配生图 Key
//
// 这一页有一条**永远不能破的规矩**：界面上不显示成员网关 Key 的任何一部分，
// 连末 4 位都不显示，只显示「已配置 / 未配置」。
// 原因写在 backend/app/models.py 的 UserGatewayAccount 注释里：designkit、网关后台、
// 上游账单三处各有一份「后 4 位」，凑一起就能把「谁 / 哪把 Key / 花了多少钱」串成一条线。
// 后端接口本身也不返回 Key（backend/app/routers/users.py 文件头），
// 所以这里就算想显示也拿不到——两边是一致的，不要试图给它加一个「查看」按钮。
import { api, getSessionEpoch } from '../api.js';
import {
  button,
  confirmDialog,
  copyText,
  emptyState,
  field,
  fmtTime,
  h,
  icon,
  iconButton,
  inlineAlert,
  modal,
  skeleton,
  statusBadge,
  toast,
} from '../ui.js';

// 用户名规则与后端 backend/app/routers/users.py 的 _USERNAME_RE 完全一致。
// 前端先拦一道纯粹是为了当场给出中文提示，不是安全措施——真正说了算的是后端。
const USERNAME_RE = /^[A-Za-z0-9._-]{3,32}$/;
const MIN_PASSWORD_LEN = 8;
// 已配置的 Key 在输入框里显示成 8 个星号。管理员不动它直接提交时，
// 后端（users.py 的 _looks_masked）会认出这是占位符，判定为「没改」，不会覆盖原 Key。
const MASK = '********';

// 网关账号状态 → 中文。取值见 backend/app/models.py 的 GATEWAY_ACCOUNT_STATES。
// 只有 active 才真的能生图（settings_service.resolve_for_user 只认这一个）。
const GATEWAY_STATE_TEXT = {
  active: '可用',
  key_issued: '已拿到 Key，还没自检通过',
  manual: '手工填入',
  disabled: '已停用',
  failed: '开通失败',
  pending: '开通中',
};

function passwordControl({ autocomplete = 'new-password', placeholder = '' } = {}) {
  const input = h('input', { class: 'dk-control input', type: 'password', autocomplete, placeholder });
  const toggle = iconButton('eye', '显示密码', {
    className: 'dk-password-toggle',
    onclick: () => {
      const visible = input.type === 'text';
      input.type = visible ? 'password' : 'text';
      toggle.replaceChildren(icon(visible ? 'eye' : 'eye-off'));
      toggle.setAttribute('aria-label', visible ? '显示密码' : '隐藏密码');
      toggle.title = visible ? '显示密码' : '隐藏密码';
      input.focus();
    },
  });
  return { input, wrap: h('div', { class: 'dk-password-control' }, input, toggle) };
}

export function renderUsers(container, currentUser) {
  const state = {
    users: [],
    // 计费方式：shared（共用一把 Key）/ per_user（每人一把）。
    // 只影响页面上的提示文案，拿不到就当拿不到，不影响任何操作。
    gatewayMode: '',
    stopped: false,
    pending: new Set(),
  };

  const page = h('section', { class: 'dk-page dk-page--users', 'aria-labelledby': 'users-page-title' });
  // 「停用连带停掉了他名下 N 把 API Key」这类说明放在这里，而不是放进 listSection：
  // listSection 每次 renderList() 都是 replaceChildren 整体重画，写进去的提示
  // 会被紧接着的那次重画抹掉（toggleUser 的 finally 里就有一次）。
  const noticeRoot = h('div', { 'aria-live': 'polite' });
  const listSection = h('section', { class: 'dk-section', 'aria-labelledby': 'user-list-title', 'aria-busy': 'true' },
    skeleton({ lines: 6, className: 'dk-loading' }));
  page.append(h('h1', { id: 'users-page-title', class: 'dk-visually-hidden' }, '成员账号'), noticeRoot, listSection);
  container.append(page);

  async function load() {
    if (state.stopped) return;
    // 整页重新拉数据 = 一次新的开始，上一次停用/启用留下的说明该清掉了
    //（建完新成员会走到这里；那时还挂着「XX 已停用」会让人以为说的是新建的这个）。
    noticeRoot.replaceChildren();
    listSection.setAttribute('aria-busy', 'true');
    try {
      const [users, settings] = await Promise.all([
        api.get('/api/web/users'),
        // 计费方式只是拿来写提示语的，拿不到就算了，不能因此让整页打不开
        api.get('/api/web/settings').catch(() => null),
      ]);
      if (state.stopped) return;
      state.users = users;
      state.gatewayMode = settings ? String(settings.gateway_mode || '') : '';
      renderList();
    } catch (error) {
      if (state.stopped) return;
      listSection.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载成员列表' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
    } finally {
      listSection.setAttribute('aria-busy', 'false');
    }
  }

  function modeNotice() {
    if (state.gatewayMode === 'per_user') {
      return inlineAlert('当前计费方式是「每人一把 Key」：每个成员用自己的 Key 生图，费用各算各的。'
        + '没有配 Key 的成员点生成会被直接拦住。', 'info');
    }
    if (state.gatewayMode === 'shared') {
      return inlineAlert('当前计费方式是「共用一把 Key」：所有人都在用系统设置里那一把全局 Key，'
        + '费用记在一起。在这里给成员配的个人 Key 要等计费方式改成「每人一把」后才会生效。', 'info');
    }
    return null;
  }

  function renderList() {
    listSection.replaceChildren(
      h('div', { class: 'dk-section-head' },
        h('div', {},
          h('h2', { id: 'user-list-title', class: 'dk-section-title' }, '成员账号'),
          h('p', { class: 'dk-section-meta' }, '给同事开账号、停用离职人员、给每个人配生图额度。没有「删除」——删了人他名下的历史任务会变成谁都查不到的无主数据，要让人登不进来请用「停用」。')),
        button('新建成员', { iconName: 'user-plus', onclick: createUser })),
      modeNotice(),
    );

    if (!state.users.length) {
      listSection.append(emptyState('users', '还没有成员账号', {
        description: '给每个同事开一个自己的账号，生成记录和商品图才会各归各的。',
        action: button('新建第一个成员', { iconName: 'user-plus', onclick: createUser }),
      }));
      return;
    }

    listSection.append(h('div', { class: 'dk-user-list' }, state.users.map(renderRow)));
  }

  function renderRow(user) {
    const busy = state.pending.has(user.id);
    const isSelf = currentUser && user.id === currentUser.id;
    const gateway = user.gateway || {};
    const stateText = gateway.state ? (GATEWAY_STATE_TEXT[gateway.state] || gateway.state) : '';

    return h('article', { class: 'dk-user-row', 'data-state': user.is_active ? 'enabled' : 'disabled' },
      h('div', { class: 'dk-user-row__main' },
        h('div', { class: 'dk-user-row__identity' },
          h('h3', {}, user.display_name || user.username),
          h('code', {}, user.username)),
        h('div', { class: 'dk-user-row__badges' },
          statusBadge(user.is_active ? 'active' : 'inactive'),
          h('span', { class: `dk-badge dk-badge--${user.role === 'admin' ? 'info' : 'neutral'}` },
            icon(user.role === 'admin' ? 'shield-check' : 'user-round', { size: 14 }),
            user.role === 'admin' ? '管理员' : '成员'),
          isSelf ? h('span', { class: 'dk-badge dk-badge--neutral' }, icon('circle-user', { size: 14 }), '这是你自己') : null,
          // 建了号但人一直没登录、或者密码条子丢了，只能从这个标记看出来
          user.must_change_password
            ? h('span', { class: 'dk-badge dk-badge--warning', title: '这个人还没有用初始密码登录过，或者登录后还没改密码' },
              icon('key-round', { size: 14 }), '未改初始密码')
            : null)),
      h('dl', { class: 'dk-user-row__stats' },
        h('div', {},
          h('dt', {}, '生图额度'),
          h('dd', { 'data-tone': gateway.configured ? 'ok' : 'muted' },
            gateway.configured ? '已配置' : '未配置',
            stateText ? h('small', {}, `（${stateText}）`) : null)),
        h('div', {}, h('dt', {}, '创建时间'), h('dd', {}, fmtTime(user.created_at)))),
      gateway.last_error
        ? h('p', { class: 'dk-user-row__error' }, icon('circle-alert', { size: 14 }), gateway.last_error)
        : null,
      h('div', { class: 'dk-inline-actions' },
        button(user.is_active ? '停用' : '启用', {
          variant: 'secondary',
          size: 'sm',
          iconName: user.is_active ? 'circle-x' : 'play',
          loading: busy,
          disabled: busy || (user.is_active && isSelf),
          disabledReason: user.is_active && isSelf ? '不能停用你自己的账号，否则下一次点击就会把自己锁在门外' : '',
          onclick: () => toggleUser(user),
        }),
        button('重置密码', {
          variant: 'secondary',
          size: 'sm',
          iconName: 'key-round',
          disabled: busy || isSelf,
          // 这句提示必须指到**真实存在**的位置：右上角那个用户菜单里只有
          //「接口文档」和「退出登录」两项（见 core/app-shell.js），没有「我的账户」。
          //「我的账户」在左侧导航最下面，短标签是「我的」（见 app.js 的 ROUTES）。
          // 指错地方比不给提示更糟——非技术用户会相信提示，反复在错的地方找。
          disabledReason: isSelf
            ? '给自己改密码请到左侧导航最下面的「我的」页（手机上导航在屏幕底部）；管理员也可以在「系统设置」页最下面改'
            : '',
          onclick: () => resetPassword(user),
        }),
        button('配置生图 Key', {
          variant: 'secondary',
          size: 'sm',
          iconName: 'plug',
          disabled: busy,
          onclick: () => editGateway(user),
        })));
  }

  // ── 新建成员 ───────────────────────────────────────────────────────────
  function createUser() {
    const username = h('input', { class: 'input', autocomplete: 'off', placeholder: '例如 zhangsan 或 li.si' });
    const displayName = h('input', { class: 'input', autocomplete: 'off', placeholder: '例如 张三（留空就用用户名）' });
    const role = h('select', { class: 'select' },
      h('option', { value: 'member' }, '成员（只能看到自己的图和任务）'),
      h('option', { value: 'admin' }, '管理员（能管账号、看全部设置）'));
    const password = passwordControl({ placeholder: `至少 ${MIN_PASSWORD_LEN} 位` });
    const alertRoot = h('div', { 'aria-live': 'polite' });
    let overlay;

    const submit = button('创建账号', {
      iconName: 'user-plus',
      onclick: async () => {
        alertRoot.replaceChildren();
        const name = username.value.trim();
        if (!USERNAME_RE.test(name)) {
          alertRoot.append(inlineAlert('用户名只能用字母、数字和 . _ -，长度 3~32 位（例如 zhangsan 或 li.si）。', 'error'));
          username.focus();
          return;
        }
        if (password.input.value.length < MIN_PASSWORD_LEN) {
          alertRoot.append(inlineAlert(`初始密码至少 ${MIN_PASSWORD_LEN} 位。`, 'error'));
          password.input.focus();
          return;
        }
        const initialPassword = password.input.value;
        submit.disabled = true;
        submit.setAttribute('aria-busy', 'true');
        const sessionAtSubmit = getSessionEpoch();
        try {
          await api.post('/api/web/users', {
            username: name,
            display_name: displayName.value.trim(),
            role: role.value,
            password: initialPassword,
          });
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          overlay.close();
          showInitialPassword(name, initialPassword);
          await load();
        } catch (error) {
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          alertRoot.append(inlineAlert(error.message, 'error', { title: '创建失败' }));
          submit.disabled = false;
        } finally {
          submit.removeAttribute('aria-busy');
        }
      },
    });

    overlay = modal({
      title: '新建成员账号',
      body: h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); submit.click(); } },
        field('用户名', username, {
          required: true,
          help: '登录时输入的名字。只能用字母、数字和 . _ -，长度 3~32 位；建号后改不了，想改只能重新建一个。',
        }),
        field('显示名', displayName, { help: '界面上显示的名字，可以用中文。留空就显示用户名。' }),
        field('角色', role, { help: '成员只能看到自己的商品图和生成记录；管理员还能管账号、改系统设置。' }),
        field('初始密码', password.wrap, {
          required: true,
          help: `至少 ${MIN_PASSWORD_LEN} 位。你自己定一个当面说得清的，他第一次登录会被要求立刻改掉。`,
        }),
        alertRoot),
      footer: [button('取消', { variant: 'quiet', onclick: () => overlay.close() }), submit],
    });
    queueMicrotask(() => username.focus());
  }

  // 建号成功后把初始密码原样显示一次，方便管理员当面念给本人听。
  // 这不是「泄露」：这串密码本来就是管理员自己刚刚输入的，而且本人一登录就得改掉。
  function showInitialPassword(username, password, title = '账号已创建') {
    let overlay;
    const copyButton = button('复制密码', { variant: 'secondary', iconName: 'copy', onclick: () => copyText(password, '初始密码已复制') });
    overlay = modal({
      title,
      body: h('div', { class: 'dk-panel-stack' },
        inlineAlert('请把这个初始密码告诉本人，他首次登录会被要求改掉。'
          + '密码不会再显示第二次；忘了就在这一页点「重置密码」重新设一个。', 'warning', { title: '把密码交给本人' }),
        h('dl', { class: 'dk-initial-credential' },
          h('div', {}, h('dt', {}, '用户名'), h('dd', {}, h('code', {}, username))),
          h('div', {}, h('dt', {}, '初始密码'), h('dd', {}, h('code', {}, password)))),
        h('p', { class: 'dk-section-meta' }, '建议当面或电话告知，不要发在群里。')),
      footer: [copyButton, button('知道了', { onclick: () => overlay.close() })],
    });
  }

  // ── 停用 / 启用 ───────────────────────────────────────────────────────
  async function toggleUser(user) {
    if (state.pending.has(user.id) || state.stopped) return;
    if (user.is_active) {
      // 这段文案要和后端 users.py 的 toggle_user 说的是同一件事。以前它写着
      //「图片链接也会立刻打不开」，而当时后端只停账号、不碰这个人名下的 ERP
      // API Key——对接方照样能拿签名链接下载图片、甚至继续调接口生图花钱。
      // 现在后端会把他名下还启用着的 Key 一并停掉，这句话才算兑现；
      // 同时要把「启用回来时 Key 不会自动恢复」提前讲清楚，别等对接断了才发现。
      const confirmed = await confirmDialog(
        `停用「${user.display_name || user.username}」？他会立刻被登出，已经发出去的图片链接也会立刻打不开。`
        + '他名下的 ERP API Key 会一并停用，用这些 Key 对接的 ERP 会立刻调不通——'
        + '以后把账号启用回来时这些 Key 不会自动恢复，需要你到「API 对接」页逐把重开。'
        + '他名下的商品图和生成记录都会原样保留，随时可以再启用回来。',
        { danger: true, okText: '停用账号', title: '确认停用' },
      );
      if (!confirmed) return;
    }
    state.pending.add(user.id);
    noticeRoot.replaceChildren();  // 上一次操作留下的说明先清掉，免得看串了
    renderList();
    try {
      const updated = await api.post(`/api/web/users/${user.id}/toggle`);
      if (state.stopped) return;
      // message / disabled_api_keys / inactive_api_keys 是这一次操作的说明，
      // 不属于成员本身的状态，挑出来只用于提示，别塞进 state.users
      //（列表接口不返回它们，留着会让「同一行数据这次有、下次没有」变成
      // 一个没必要的坑）。
      const {
        message,
        disabled_api_keys: disabledKeys,
        inactive_api_keys: inactiveKeys,
        ...userData
      } = updated;
      state.users = state.users.map((item) => (item.id === userData.id ? userData : item));
      // 「连带停掉了 N 把 API Key」和「他名下还有 N 把停用的 Key 不会自动恢复」
      // 都是后果比一句 toast 更重的事：toast 几秒就消失，而这两件事管理员往往
      // 得照着去「API 对接」页做后续动作。所以留在页面上，让他读完再走。
      // 判断依据用后端给的两个数字，不去匹配 message 里的字眼——
      // 那样后端改一个词，前端就悄悄退化成一闪而过的 toast，没人会发现。
      const needsFollowUp = updated.is_active ? Boolean(inactiveKeys) : Boolean(disabledKeys);
      if (needsFollowUp) {
        noticeRoot.replaceChildren(inlineAlert(message, 'warning', {
          title: updated.is_active ? '账号已启用（API Key 需要你手动重开）' : '账号已停用',
        }));
      } else {
        toast(message || (updated.is_active ? '账号已启用' : '账号已停用'), 'success');
      }
    } catch (error) {
      if (state.stopped) return;
      toast(error.message, 'error');
    } finally {
      state.pending.delete(user.id);
      if (!state.stopped) renderList();
    }
  }

  // ── 重置密码 ──────────────────────────────────────────────────────────
  function resetPassword(user) {
    const password = passwordControl({ placeholder: `至少 ${MIN_PASSWORD_LEN} 位` });
    const alertRoot = h('div', { 'aria-live': 'polite' });
    let overlay;
    const submit = button('重置密码', {
      iconName: 'key-round',
      onclick: async () => {
        alertRoot.replaceChildren();
        if (password.input.value.length < MIN_PASSWORD_LEN) {
          alertRoot.append(inlineAlert(`新密码至少 ${MIN_PASSWORD_LEN} 位。`, 'error'));
          password.input.focus();
          return;
        }
        const newPassword = password.input.value;
        submit.disabled = true;
        submit.setAttribute('aria-busy', 'true');
        const sessionAtSubmit = getSessionEpoch();
        try {
          const result = await api.post(`/api/web/users/${user.id}/reset_password`, { password: newPassword });
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          overlay.close();
          if (result.user) state.users = state.users.map((item) => (item.id === result.user.id ? result.user : item));
          renderList();
          showInitialPassword(user.username, newPassword, '密码已重置');
        } catch (error) {
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          alertRoot.append(inlineAlert(error.message, 'error', { title: '重置失败' }));
          submit.disabled = false;
        } finally {
          submit.removeAttribute('aria-busy');
        }
      },
    });

    overlay = modal({
      title: `重置「${user.display_name || user.username}」的密码`,
      body: h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); submit.click(); } },
        inlineAlert('重置后他在所有设备上的登录会立刻失效，需要用新密码重新登录，并被要求马上改掉。', 'warning'),
        field('新的初始密码', password.wrap, {
          required: true,
          help: `至少 ${MIN_PASSWORD_LEN} 位，由你自己定。保存后请把新密码当面告诉本人。`,
        }),
        alertRoot),
      footer: [button('取消', { variant: 'quiet', onclick: () => overlay.close() }), submit],
    });
    queueMicrotask(() => password.input.focus());
  }

  // ── 配置生图 Key ──────────────────────────────────────────────────────
  function editGateway(user) {
    const configured = Boolean(user.gateway && user.gateway.configured);
    // 已配置时填 8 个星号：这不是「显示 Key」，星号里没有任何真实信息。
    // 管理员不动它直接保存，后端会认出这是占位符并跳过，不会把原 Key 覆盖掉。
    const keyInput = h('input', {
      class: 'input',
      type: 'password',
      autocomplete: 'off',
      spellcheck: 'false',
      value: configured ? MASK : '',
      placeholder: configured ? MASK : 'sk-…',
    });
    const statusRoot = h('div', { class: 'dk-gateway-status', 'aria-live': 'polite' });
    const CONFIGURED_TEXT = '这个成员已经配了生图 Key。出于安全考虑，界面上永远不显示 Key 本身（连末 4 位也不显示），只显示「已配置」。';
    const NOT_CONFIGURED_TEXT = '这个成员还没有专属的生图 Key。配好之后他生图的费用从他自己这把 Key 上走。';
    const intro = h('p', { class: 'dk-section-meta' }, configured ? CONFIGURED_TEXT : NOT_CONFIGURED_TEXT);
    let overlay;

    const testButton = button('测试连接', {
      variant: 'secondary',
      iconName: 'plug',
      onclick: async () => {
        statusRoot.replaceChildren(inlineAlert('正在测试…（只查一次模型列表，不出图、不花钱）', 'info'));
        testButton.disabled = true;
        testButton.setAttribute('aria-busy', 'true');
        try {
          // 这条接口永远返回 HTTP 200，成败看响应体的 ok，不能靠 catch 判断
          const result = await api.post(`/api/web/users/${user.id}/gateway/test`);
          if (state.stopped) return;
          statusRoot.replaceChildren(inlineAlert(result.message, result.ok ? 'success' : 'error'));
        } catch (error) {
          if (state.stopped) return;
          statusRoot.replaceChildren(inlineAlert(error.message, 'error'));
        } finally {
          testButton.disabled = false;
          testButton.removeAttribute('aria-busy');
        }
      },
    });

    const clearButton = configured ? button('清除 Key', {
      variant: 'danger',
      iconName: 'trash-2',
      onclick: async () => {
        const confirmed = await confirmDialog(
          `清除「${user.display_name || user.username}」的生图 Key？`
          + '在「每人一把 Key」的计费方式下，他会立刻无法生图，直到重新配置。',
          { danger: true, okText: '清除 Key', title: '确认清除' },
        );
        if (!confirmed) return;
        clearButton.disabled = true;
        try {
          const result = await api.del(`/api/web/users/${user.id}/gateway`);
          if (state.stopped) return;
          applyGateway(user.id, result.gateway);
          overlay.close();
          toast(result.message, 'success');
        } catch (error) {
          if (state.stopped) return;
          statusRoot.replaceChildren(inlineAlert(error.message, 'error'));
          clearButton.disabled = false;
        }
      },
    }) : null;

    const saveButton = button('保存', {
      iconName: 'save',
      onclick: async () => {
        saveButton.disabled = true;
        saveButton.setAttribute('aria-busy', 'true');
        try {
          const result = await api.put(`/api/web/users/${user.id}/gateway`, { api_key: keyInput.value });
          if (state.stopped) return;
          applyGateway(user.id, result.gateway);
          // changed=false 表示「你没填新的 Key」，这时说「保存成功」是骗人的：
          // 管理员会以为自己刚才那一下真的改到了东西。
          statusRoot.replaceChildren(inlineAlert(result.message, result.changed ? 'success' : 'info'));
          if (result.changed) {
            keyInput.value = MASK;
            keyInput.placeholder = MASK;
            intro.textContent = CONFIGURED_TEXT;
            toast('已保存该成员的生图 Key', 'success');
          }
        } catch (error) {
          if (state.stopped) return;
          statusRoot.replaceChildren(inlineAlert(error.message, 'error'));
        } finally {
          saveButton.disabled = false;
          saveButton.removeAttribute('aria-busy');
        }
      },
    });

    overlay = modal({
      title: `配置「${user.display_name || user.username}」的生图 Key`,
      body: h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); saveButton.click(); } },
        intro,
        field('生图网关 Key', keyInput, {
          help: '已保存的值仅显示星号；不修改即不会覆盖原有 Key。'
            + '把网关那边给这个人开的 Key 整串粘进来即可。',
        }),
        statusRoot,
        state.gatewayMode === 'shared'
          ? inlineAlert('当前计费方式是「共用一把 Key」，这里配的个人 Key 要等在「系统设置」里改成「每人一把」之后才会真正被用到。', 'warning')
          : null),
      footer: [
        button('取消', { variant: 'quiet', onclick: () => overlay.close() }),
        clearButton,
        testButton,
        saveButton,
      ],
    });
    queueMicrotask(() => keyInput.focus());
  }

  function applyGateway(userId, gateway) {
    if (!gateway) return;
    state.users = state.users.map((item) => (item.id === userId ? { ...item, gateway } : item));
    renderList();
  }

  load();
  return () => { state.stopped = true; };
}
