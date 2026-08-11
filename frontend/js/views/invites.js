// 邀请码（只有管理员看得到）：发码、看码被谁用了、作废。
//
// 这一页服务的是一个很具体的场景：运营同学要拉几个同事进来用系统，
// 但又不想一个个手工建号、也不想把密码用微信发出去。于是流程变成
//「在这里点一下生成 → 把码发给对方 → 对方自己在注册页填个用户名密码」。
//
// 界面上有三件事是**必须说清楚**的，少一件运营就会做出错误的决定：
//
// 一、码发出去就收不回来，只能作废。所以生成时的「可用次数」和「有效天数」
//    是真正的决策点，不能只当成两个可有可无的输入框。
//
// 二、「作废」不等于「把用这张码开的号停掉」。后端把这句话写进了 revoke 的
//    返回值里（连同还有几个号是这张码开的），所以那段 message 要**整段显示**，
//    不能用 toast 一闪而过——它是管理员判断「还要不要去停号」的唯一依据。
//
// 三、注册总开关没打开时，码发了也没用。这件事在列表里一行都看不出来，
//    所以后端专门给了 notice 字段，这里把它挂成页顶告警条。
//
// 还有一条不在界面上、但改这个文件时要记住的：**码本身是敏感信息**。
// 后端只对管理员返回 code，这一页也只在管理员路由下（app.js 的 adminOnly）。
// 不要把码写进 URL、不要塞进 document.title、也不要在控制台打出来——
// 那些地方会被浏览器历史、截图、录屏顺手带走。
import { api } from '../api.js';
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
  toast,
} from '../ui.js';

// 后端 state → 徽标配色与图标。**文字一律用后端给的 state_label**，
// 不在这里另写一份中文：两边写岔的话，同一张码在列表和详情里会有两个说法。
const STATE_TONE = {
  active: 'success',
  used_up: 'neutral',
  expired: 'neutral',
  revoked: 'danger',
};
const STATE_ICON = {
  active: 'circle-check',
  used_up: 'circle-x',
  expired: 'clock',
  revoked: 'shield-alert',
};

/** 有效期写成人话。
 *
 * 只显示一个绝对时间（2026-08-18 10:30）对运营是不够的——他要判断的是
 *「这批码还来不来得及发出去」，需要的是「还有几天」。两个都给。
 */
function expiryText(item) {
  if (!item.expires_at) return '永不过期';
  const at = new Date(item.expires_at);
  if (Number.isNaN(at.getTime())) return String(item.expires_at);
  const stamp = fmtTime(item.expires_at);
  if (item.state === 'expired') return `${stamp}（已过期）`;
  const days = Math.ceil((at.getTime() - Date.now()) / 86400000);
  if (days <= 0) return `${stamp}（今天到期）`;
  if (days === 1) return `${stamp}（还有 1 天）`;
  return `${stamp}（还有 ${days} 天）`;
}

/** 页顶告警条的轻重。
 *
 * notice 是后端把几句话拼起来的（见 invites.py 的 _notice）。
 *「危险组合」那一句说的是「陌生人能注册 + 服务端愿意访问内网」同时成立，
 * 这是本期唯一一条会造成真实安全后果的状态，必须用最重的红色，
 * 不能和「有永不过期的码」那种提醒挤在同一个黄条里。
 */
function noticeTone(notice) {
  if (notice.includes('危险组合')) return 'danger';
  if (notice.includes('总开关')) return 'warning';
  return 'info';
}

export function renderInvites(container) {
  const state = {
    items: [],
    defaults: { max_uses: 1, valid_days: 7 },
    notice: '',
    selfRegisterEnabled: false,
    stopped: false,
    // 正在作废中的码 id，防止连点两次
    pending: new Set(),
  };

  const page = h('section', { class: 'dk-page dk-page--invites', 'aria-labelledby': 'invites-page-title' });
  // 「作废成功」那段长说明挂在这里，而不是挂进 listSection：
  // listSection 每次 renderList() 都整体 replaceChildren 重画，
  // 写进去的说明会被紧接着的那次重画抹掉（照抄 views/users.js 的做法）。
  const noticeRoot = h('div', { 'aria-live': 'polite' });
  const listSection = h('section', {
    class: 'dk-section',
    'aria-labelledby': 'invite-list-title',
    'aria-busy': 'true',
  }, skeleton({ lines: 6, className: 'dk-loading' }));
  page.append(
    h('h1', { id: 'invites-page-title', class: 'dk-visually-hidden' }, '邀请码'),
    noticeRoot,
    listSection,
  );
  container.append(page);

  async function load() {
    if (state.stopped) return;
    listSection.setAttribute('aria-busy', 'true');
    try {
      const data = await api.get('/api/web/invites?limit=200');
      if (state.stopped) return;
      state.items = Array.isArray(data?.items) ? data.items : [];
      state.defaults = {
        max_uses: Number(data?.defaults?.max_uses) || 1,
        valid_days: Number.isFinite(Number(data?.defaults?.valid_days)) ? Number(data.defaults.valid_days) : 7,
      };
      state.notice = String(data?.notice || '');
      state.selfRegisterEnabled = Boolean(data?.self_register_enabled);
      renderList();
    } catch (error) {
      if (state.stopped) return;
      listSection.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载邀请码列表' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
    } finally {
      listSection.setAttribute('aria-busy', 'false');
    }
  }

  function renderList() {
    // 这里必须 filter(Boolean)：ui.js 的 h() 会把 null 子节点丢掉，但
    // replaceChildren 不会——它照 DOM 规范把 null 转成字符串，页面上会凭空
    // 多出一个「null」。没有告警条时（notice 为空）就会撞上。
    listSection.replaceChildren(...[
      h('div', { class: 'dk-section-head' },
        h('div', {},
          h('h2', { id: 'invite-list-title', class: 'dk-section-title' }, '邀请码'),
          h('p', { class: 'dk-section-meta' },
            '生成一张码发给对方，他就能自己在注册页开一个账号，不用你手工建号、也不用把密码发出去。'
            + '码只能作废、不能删除——删掉码就等于删掉「这张码开过哪些账号」这份记录。')),
        h('div', { class: 'dk-inline-actions' },
          button('刷新', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
          button('生成邀请码', { iconName: 'circle-plus', onclick: createCodes }))),
      state.notice ? inlineAlert(state.notice, noticeTone(state.notice), {
        title: noticeTone(state.notice) === 'danger' ? '请立刻处理' : '注意',
      }) : null,
    ].filter(Boolean));

    if (!state.items.length) {
      listSection.append(emptyState('key-round', '还没有生成过邀请码', {
        description: '生成一张码发给同事，他就能自己注册账号。默认一张码只能用一次，'
          + '这样每个号是谁开的一目了然。',
        action: button('生成第一张邀请码', { iconName: 'circle-plus', onclick: createCodes }),
      }));
      return;
    }

    listSection.append(h('div', { class: 'dk-invite-list' }, state.items.map(renderRow)));
  }

  function renderRow(item) {
    const busy = state.pending.has(item.id);
    const tone = STATE_TONE[item.state] || 'neutral';
    const used = Number(item.used_count) || 0;
    const total = Number(item.max_uses) || 0;
    const redemptions = Array.isArray(item.redemptions) ? item.redemptions : [];

    return h('article', { class: 'dk-invite-row', 'data-state': item.state },
      h('div', { class: 'dk-invite-row__main' },
        h('div', { class: 'dk-invite-row__code' },
          // 码用等宽字体显示，并且允许用户直接选中——运营有可能不点复制按钮，
          // 而是习惯性地拖蓝再 Ctrl+C。
          h('code', { class: 'dk-invite-code' }, item.code_display || item.code || ''),
          iconButton('copy', '复制这张邀请码', {
            onclick: () => copyText(item.code_display || item.code || '', '邀请码已复制，发给对方即可'),
          }),
          h('span', { class: `dk-badge dk-badge--${tone}` },
            icon(STATE_ICON[item.state] || 'info', { size: 14 }),
            item.state_label || item.state || '')),
        item.note ? h('p', { class: 'dk-invite-row__note' }, icon('info', { size: 14 }), h('span', {}, item.note)) : null,
        renderRedemptions(item, redemptions)),
      h('dl', { class: 'dk-invite-row__stats' },
        h('div', {},
          h('dt', {}, '已用 / 可用'),
          h('dd', {}, `${used} / ${total}`,
            item.state === 'active' ? h('small', {}, `　还能用 ${Number(item.remaining) || 0} 次`) : null)),
        h('div', {},
          h('dt', {}, '有效期'),
          // 永不过期是一个需要管理员回头处理的状态，标出来而不是平铺直叙
          h('dd', { 'data-tone': !item.expires_at && item.state === 'active' ? 'warn' : 'plain' },
            expiryText(item))),
        h('div', {},
          h('dt', {}, '发码人'),
          h('dd', {}, item.created_by || '—')),
        h('div', {},
          h('dt', {}, '生成时间'),
          h('dd', {}, fmtTime(item.created_at)))),
      h('div', { class: 'dk-inline-actions' },
        button('复制邀请码', {
          variant: 'secondary',
          size: 'sm',
          iconName: 'copy',
          onclick: () => copyText(item.code_display || item.code || '', '邀请码已复制，发给对方即可'),
        }),
        button(item.state === 'revoked' ? '已作废' : '作废', {
          variant: 'secondary',
          size: 'sm',
          iconName: 'circle-x',
          loading: busy,
          disabled: busy || item.state === 'revoked',
          disabledReason: item.state === 'revoked' ? '这张码已经作废了，不用再点' : '',
          onclick: () => revoke(item),
        })));
  }

  /** 「被谁用了」。这是这一页存在的第二个理由（第一个是发码）：
   * 码被转发到群里刷号时，这里是唯一能定位到具体账号的地方。
   * 一次都没用过时也要显示一句话，留空会让人以为是没加载出来。
   */
  function renderRedemptions(item, redemptions) {
    if (!redemptions.length) {
      const hint = item.state === 'active'
        ? '还没有人用这张码注册。'
        : '没有人用这张码注册过。';
      return h('p', { class: 'dk-invite-row__empty' }, hint);
    }
    return h('div', { class: 'dk-invite-redemptions' },
      h('span', { class: 'dk-invite-redemptions__title' }, `被谁用了（${redemptions.length}）`),
      h('ul', { class: 'dk-invite-redemptions__list' }, redemptions.map((entry) => h('li', {},
        h('strong', {}, entry.username || '（账号已删除）'),
        h('span', {}, fmtTime(entry.created_at)),
        // IP 只是线索不是判据：套了反向代理又没配好 trusted_proxy_hops 时，
        // 这一列会全站显示成同一个地址。所以写成「来自 …」而不是「注册人 IP」，
        // 也不拿它当「同一个人刷号」的结论。
        entry.client_ip ? h('span', { class: 'dk-invite-redemptions__ip' }, `来自 ${entry.client_ip}`) : null))));
  }

  // ── 生成 ────────────────────────────────────────────────────────────
  function createCodes() {
    const maxUses = h('input', {
      class: 'input', type: 'number', min: '1', max: '1000', inputmode: 'numeric',
      value: String(state.defaults.max_uses),
    });
    const validDays = h('input', {
      class: 'input', type: 'number', min: '0', max: '365', inputmode: 'numeric',
      value: String(state.defaults.valid_days),
    });
    const count = h('input', {
      class: 'input', type: 'number', min: '1', max: '20', inputmode: 'numeric', value: '1',
    });
    const note = h('input', { class: 'input', maxlength: '128', placeholder: '例如：给运营部小李' });
    const feedback = h('div', { class: 'dk-form-message', tabindex: '-1', 'aria-live': 'assertive' });

    const submit = button('生成', {
      onclick: async () => {
        feedback.replaceChildren();
        submit.disabled = true;
        submit.dataset.loading = 'true';
        try {
          // 数字一律原样把字符串传过去：后端 _as_int 认得字符串，
          // 而且范围报错是它写的中文（「可用次数只能填 1 到 1000 之间的数字」），
          // 比前端自己拦一道再另写一句要一致得多。
          const result = await api.post('/api/web/invites', {
            max_uses: maxUses.value,
            valid_days: validDays.value,
            count: count.value,
            note: note.value.trim(),
          });
          if (state.stopped) return;
          dialog.close('created');
          showCreated(result);
          load();
        } catch (error) {
          if (state.stopped) return;
          feedback.append(inlineAlert(error.message, 'error'));
          feedback.focus();
          submit.disabled = false;
          submit.dataset.loading = 'false';
        }
      },
    });

    const dialog = modal({
      title: '生成邀请码',
      body: h('div', { class: 'dk-stack' },
        feedback,
        state.selfRegisterEnabled
          ? null
          : inlineAlert('「自助注册」总开关还没打开，现在生成的码别人也用不了。'
            + '码可以先发，但记得到「系统设置」里把开关打开。', 'warning'),
        field('每张码可以用几次', maxUses, {
          required: true,
          help: '默认 1 次。填大于 1 的数字看起来省事，但那张码一旦被转发到群里，'
            + '你只知道「有几个号是它开的」、分不清谁是谁。要精确到人，请保持 1 次、多发几张。',
        }),
        field('几天后过期', validDays, {
          required: true,
          help: '填 0 表示永不过期——一张永久有效的码就是一张长期通行证，发出去只能靠事后作废收回。',
        }),
        field('这次生成几张', count, {
          required: true,
          help: '一次最多 20 张。要拉一批人进来，建议每人一张一次性的码。',
        }),
        field('备注（选填）', note, {
          help: '写清楚这张码打算发给谁。出事时这是唯一能把「码」和「人」对上的线索。',
        })),
      footer: [
        button('取消', { variant: 'secondary', onclick: () => dialog.close('cancel') }),
        submit,
      ],
    });
  }

  /** 生成成功后的那一屏：**这是管理员唯一一次能方便地把码拿走的机会**。
   *
   * 列表里当然也能复制，但刚生成的这几张混在一堆旧码里并不好找，
   * 所以单独弹一屏，并且给一个「复制全部」——一次发给一批人时，
   * 逐张点复制、逐张切到微信粘贴，是会让人放弃用这个功能的那种麻烦。
   */
  function showCreated(result) {
    const items = Array.isArray(result?.items) ? result.items : [];
    const codes = items.map((item) => item.code_display || item.code || '').filter(Boolean);
    const dialog = modal({
      title: `已生成 ${codes.length} 张邀请码`,
      body: h('div', { class: 'dk-stack' },
        // message 是后端拼的一整段（永不过期的代价、可用次数大于 1 的代价、
        // 注册开关没开……），整段显示，不要截断也不要挑着显示。
        inlineAlert(result?.message || '已生成，把码发给对方即可。', 'success'),
        h('ul', { class: 'dk-invite-new-list' }, items.map((item) => h('li', {},
          h('code', { class: 'dk-invite-code' }, item.code_display || item.code || ''),
          iconButton('copy', '复制这张邀请码', {
            onclick: () => copyText(item.code_display || item.code || '', '邀请码已复制'),
          })))),
        h('p', { class: 'dk-invite-hint' },
          '把码发给对方，让他打开本系统的登录页，点「我有邀请码，去注册」。')),
      footer: [
        codes.length > 1
          ? button('复制全部', {
            variant: 'secondary',
            iconName: 'copy',
            onclick: () => copyText(codes.join('\n'), `已复制 ${codes.length} 张邀请码`),
          })
          : null,
        button('知道了', { onclick: () => dialog.close('ok') }),
      ],
    });
  }

  // ── 作废 ────────────────────────────────────────────────────────────
  async function revoke(item) {
    if (state.pending.has(item.id) || state.stopped) return;
    const ok = await confirmDialog(
      `作废后，「${item.code_display || item.code}」这张码就不能再用来注册了。`
      + '已经用它注册成功的账号不受影响，仍然能正常登录。',
      { danger: true, okText: '作废这张码', title: '确认作废邀请码' },
    );
    if (!ok || state.stopped) return;
    state.pending.add(item.id);
    noticeRoot.replaceChildren();
    renderList();
    try {
      const result = await api.post(`/api/web/invites/${item.id}/revoke`);
      if (state.stopped) return;
      if (result?.item) {
        state.items = state.items.map((row) => (row.id === item.id ? result.item : row));
      }
      // 这段 message **必须整段留在页面上**：它会写明「此前已经有 N 个账号是用
      // 这张码注册的，作废不会把它们停掉」——管理员正是靠这句话决定要不要
      // 再去「成员账号」页停号。用 toast 的话它 5 秒后就没了。
      noticeRoot.replaceChildren(inlineAlert(
        result?.message || '已作废。',
        'success',
        { title: `邀请码 ${item.code_display || item.code} 已作废` },
      ));
    } catch (error) {
      if (state.stopped) return;
      toast(error.message, 'error');
    } finally {
      state.pending.delete(item.id);
      if (!state.stopped) renderList();
    }
  }

  load();

  return () => { state.stopped = true; };
}
