// 我的账户（所有人都能进）：我是谁、我现在能不能生图、改密码、退出其他所有设备。
//
// 这一页上**没有、也不该有任何 API Key 输入框**。这不是漏做，理由见
// backend/app/routers/account.py 的文件头：成员的生图额度是管理员在后台替他开通的，
// 那把 Key 从头到尾不是他自己申请的。给他一个输入框，只会多出一条
// 「密钥在聊天记录里流转」的路径，或者他填错东西把自己的账号搞坏。
// 需要开通 / 换 Key，请管理员去「成员账号」页操作。
import { api, clearSession, getSessionEpoch, getUser, setSession } from '../api.js';
import { button, confirmDialog, field, fmtTime, h, icon, inlineAlert, skeleton, toast } from '../ui.js';

const MIN_PASSWORD_LEN = 8;
// 「余额数据 X 分钟前」这句话每分钟重算一次（定时器见 renderAccount 末尾）。
const AGE_TICK_MS = 60000;

// 余额金额写成人话。有三个坑，全是「看着像 0」引起的：
// ① 0.004 美元经 toFixed(2) 会变成 $0.00，和一分钱不剩长得一模一样；
// ② 余额可以是负数（网关允许透支时），写成「$-1.20」不如「已欠费」直白；
// ③ 后端在读不到余额时给的是 null 而不是 0，所以这里遇到非数字要显示「—」，
//    绝不能顺手 Number(null) 变成 0 —— 那正是这次要修的那个「以为钱没了」。
function formatUsd(usd) {
  const value = Number(usd);
  if (usd === null || usd === undefined || !Number.isFinite(value)) return '—';
  if (value < 0) return `已欠费 $${Math.abs(value).toFixed(2)}`;
  if (value > 0 && value < 0.01) return '不足 $0.01';
  return `$${value.toFixed(2)}`;
}

// 秒数 → 「3 分钟前」。不满一分钟说「刚刚」，不要写成「0 分钟前」。
function ageText(seconds) {
  if (seconds === null || seconds === undefined) return '时间不详';
  const total = Math.max(0, Math.round(Number(seconds) || 0));
  if (total < 60) return '刚刚';
  const mins = Math.round(total / 60);
  if (mins < 60) return `${mins} 分钟前`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours} 小时前`;
  return `${Math.round(hours / 24)} 天前`;
}

export function renderAccount(container) {
  const state = {
    account: null,
    stopped: false,
    // 这份数据是几点几分拿到的。余额那块的「X 分钟前」要拿它来加上
    // 「打开页面之后又过去了多久」，理由见 balanceAgeSeconds。
    loadedAt: 0,
    // 余额那一小块的容器，定时器每分钟往里重画一次
    balanceRoot: null,
  };
  const page = h('section', { class: 'dk-page dk-page--account', 'aria-labelledby': 'account-page-title' });
  const stack = h('div', { class: 'dk-account-stack', 'aria-busy': 'true' }, skeleton({ lines: 8, className: 'dk-loading' }));
  page.append(h('h1', { id: 'account-page-title', class: 'dk-visually-hidden' }, '我的账户'), stack);
  container.append(page);

  async function load() {
    try {
      const account = await api.get('/api/web/account');
      if (state.stopped) return;
      state.account = account;
      state.loadedAt = Date.now();
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

  // 这份快照到现在有多旧（秒）。后端给的 age_seconds 是「响应发出的那一刻」
  // 的年龄，页面一挂就是一下午，所以要把「打开之后又过去的时间」加回去。
  // 不加的话，开了两小时的标签页会一直写着「3 分钟前」——而这一块存在的
  // 全部意义就是让人知道这个数字有多旧，写错了还不如不写。
  //
  // 为什么用后端给的年龄、而不是自己拿时间戳去减：用户那台 Mac 或群晖的时钟
  // 不一定准，两台机器差几分钟就能算出「-3 分钟前」这种鬼话。
  function balanceAgeSeconds() {
    const balance = ((state.account || {}).gateway || {}).balance || {};
    if (balance.age_seconds === null || balance.age_seconds === undefined) return null;
    return Number(balance.age_seconds) + Math.max(0, (Date.now() - state.loadedAt) / 1000);
  }

  /** 余额那一小块的内容（返回节点数组）。四种状态各说各的话。 */
  function balanceNodes() {
    const gateway = state.account.gateway || {};
    const balance = gateway.balance || {};
    // 间隔和阈值都由后端给（后端从 scheduler 的常量来），前端不硬编：
    // 硬编一遍，将来后台间隔一改，这里的文案就开始骗人了。
    const everyMin = Math.max(1, Math.round(Number(balance.sync_interval_seconds || 600) / 60));
    const staleAfter = Number(balance.stale_after_seconds || 1800);
    const age = balanceAgeSeconds();
    let kind = String(balance.state || 'none');
    // 后端判「新鲜」是按响应发出那一刻算的，页面挂久了就未必还新鲜。
    // 这里按当前年龄再判一次，免得一个开了一小时的页面还挂着「同步正常」。
    if (kind === 'ok' && age !== null && age > staleAfter) kind = 'stale';

    if (kind === 'none') {
      // 这里**绝不摆一个 $0.00**。还没开通的人看到 0，只会以为自己的钱被花光了，
      // 而他其实压根还没有「余额」这回事。要显示的是「接下来该做什么」。
      return [inlineAlert(
        gateway.can_generate
          ? '你的账号没有单独的余额：这台服务器现在用的是全站共用的额度（或模拟生图模式），'
            + '生图的花费不从你名下走，所以这里没有数字可看。你能不能生图，看上面那一栏。'
          : '还没有给你开通单独的生图额度，所以这里暂时没有余额可看。'
            + '请联系管理员在「成员账号」页给你开通；开通好之后，你的余额会自动出现在这里。',
        'info', { title: '余额' })];
    }

    if (kind === 'pending') {
      return [inlineAlert(
        `额度已经开通好了，正在等后台第一次同步余额（后台每 ${everyMin} 分钟同步一轮，`
        + '人多的时候可能要多等一两轮）。这不影响生图，可以照常用。'
        + '要是等了半小时这里还是空的，请联系管理员看一眼网关那边。',
        'info', { title: '余额还没同步过来' })];
    }

    // 到这里一定有数字（ok / stale）。数字和「这个数字有多旧」必须同时出现，
    // 少了后者，用户会拿它当实时余额，然后为「我刚花的钱怎么没扣」来问一圈。
    const facts = h('dl', { class: 'dk-account-facts' },
      h('div', {},
        h('dt', {}, '余额（美元）'),
        h('dd', {}, formatUsd(balance.usd))),
      h('div', {},
        h('dt', {}, '余额数据'),
        // 同时给「多久以前」和确切时间：前者一眼能看懂，后者能对得上账。
        h('dd', {}, balance.synced_at
          ? `${ageText(age)} · ${fmtTime(balance.synced_at)}`
          : ageText(age))));

    if (kind === 'stale') {
      return [facts, inlineAlert(
        '服务器有一阵子没能和生图网关对上账了（多半是网络一时不通）。'
        + '上面这个数字是当时的余额、不是现在的，这段时间里花掉的钱还没算进去。'
        + '网络恢复后它会自己更新，你不用做什么；要是一直这样，请联系管理员。',
        // 标题里直接把「多久以前」摆出来：这是用户最该先看到的一句话
        'warning', { title: `余额数据 ${ageText(age)}` })];
    }

    const nodes = [facts, h('p', { class: 'dk-section-meta' },
      `余额是后台每 ${everyMin} 分钟同步一次的快照，不是实时数字：`
      + '你刚生成的那几张图，花掉的钱可能还没算进去。')];
    // 快照本身就是 0 或负数，那是真的没钱了——这时才该提醒充值。
    // 注意和上面「读不到余额」的区别：那种情况 usd 是 null，走不到这里。
    if (Number(balance.usd) <= 0) {
      nodes.push(inlineAlert(
        '这份快照显示额度已经用完了，现在点「生成」多半会失败。请联系管理员充值。',
        'warning', { title: '额度已用完' }));
    }
    return nodes;
  }

  function paintBalance() {
    if (!state.account || !state.balanceRoot) return;
    state.balanceRoot.replaceChildren(...balanceNodes());
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

    // 余额那一小块单独放在一个稳定的容器里：定时器每分钟只重画它，
    // 不动上面的状态框、也不动下面的两个按钮（重画按钮会让点到一半的人扑空）。
    const balanceRoot = h('div', { class: 'dk-panel-stack' });
    state.balanceRoot = balanceRoot;

    const section = h('section', { class: 'dk-settings-section', 'aria-labelledby': 'account-gateway-title' },
      sectionHeader('account-gateway-title', '生图额度',
        '这里回答两个问题：我现在点「生成」会不会被拦住、我还剩多少钱。'),
      // 这一层 dk-panel-stack 是为了让「状态框 / 余额 / 测试结果 / 按钮」之间
      // 有统一的间距——.dk-settings-section 本身只是个带内边距的块，
      // 子元素之间没有任何间隔，多塞一块进去就会和上面的框贴在一起。
      // 下面的「修改密码」「登录设备」两节本来就是这么包的，这里跟它们对齐。
      h('div', { class: 'dk-panel-stack' },
        h('div', { class: 'dk-account-gateway', 'data-ok': ok ? 'true' : 'false' },
          h('span', { class: 'dk-account-gateway__icon', 'aria-hidden': 'true' }, icon(ok ? 'circle-check' : 'circle-alert', { size: 22 })),
          h('div', {},
            h('strong', {}, ok ? '可以生图' : '暂时不能生图'),
            h('p', {}, ok
              ? '你的账号已经可以正常提交生成任务。'
              // 不能生图时后端给的 message 已经是中文人话，自带「找谁、做什么」，原样显示
              : (gateway.message || '你的账号还没有开通生图额度，请联系管理员在「成员账号」页配置。')))),
        balanceRoot,
        statusRoot,
        h('div', { class: 'dk-settings-section__actions' }, testProvider, testText)));
    paintBalance();
    return section;
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

  // 「余额数据 X 分钟前」这句话自己会变旧，所以每分钟重画一次那一小块。
  // 只重画余额块、不整页刷新：整页刷新会把用户正在填的密码框清掉。
  // 也**不**在这里偷偷去重新拉一次接口——用户没做任何动作，页面上的数字
  // 自己跳一下，反而会让人怀疑刚才看到的是不是真的。
  const ageTimer = window.setInterval(() => {
    if (state.stopped) return;
    if (!state.balanceRoot || !state.balanceRoot.isConnected) return;
    paintBalance();
  }, AGE_TICK_MS);

  load();
  // 定时器必须在这里清掉：这个函数是切走页面时调用的，忘了清就是每切一次
  // 多留一个定时器在后台跑，一天下来几十个，还会抓着已经不在文档里的节点不放。
  return () => {
    state.stopped = true;
    window.clearInterval(ageTimer);
  };
}
