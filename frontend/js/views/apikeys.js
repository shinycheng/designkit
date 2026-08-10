// API 对接：密钥列表 + 分步接入指南 + 一次性 SecretReveal
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
  inlineAlert,
  modal,
  skeleton,
  statusBadge,
  toast,
} from '../ui.js';

export function renderApiKeys(container) {
  const state = { keys: [], loading: false, stopped: false, secretOverlay: null, pendingKeyIds: new Set() };
  const page = h('section', { class: 'dk-page dk-page--apikeys', 'aria-labelledby': 'apikeys-page-title' });
  const layout = h('div', { class: 'dk-api-layout' });
  const listSection = h('section', { class: 'dk-section', 'aria-labelledby': 'apikey-list-title', 'aria-busy': 'true' },
    skeleton({ lines: 6, className: 'dk-loading' }));
  const guideSection = h('section', { class: 'dk-section', 'aria-labelledby': 'api-guide-title' });
  page.append(h('h1', { id: 'apikeys-page-title', class: 'dk-visually-hidden' }, 'API 对接'), layout);
  layout.append(listSection, guideSection);
  container.append(page);
  renderGuide();

  async function load() {
    if (state.loading || state.stopped) return;
    state.loading = true;
    listSection.setAttribute('aria-busy', 'true');
    try {
      state.keys = await api.get('/api/web/apikeys');
      if (!state.stopped) renderKeyList();
    } catch (error) {
      listSection.replaceChildren(
        inlineAlert(error.message, 'error', { title: '无法加载 API 密钥' }),
        button('重试', { variant: 'secondary', iconName: 'refresh-cw', onclick: load }),
      );
    } finally {
      state.loading = false;
      listSection.setAttribute('aria-busy', 'false');
    }
  }

  function renderKeyList() {
    listSection.replaceChildren(
      h('div', { class: 'dk-section-head' },
        h('div', {},
          h('h2', { id: 'apikey-list-title', class: 'dk-section-title' }, 'API Key'),
          h('p', { class: 'dk-section-meta' }, '为 ERP、自动化工具和其他可信系统分别创建密钥。')),
        button('创建密钥', { iconName: 'plus', onclick: createKey })),
    );

    if (state.keys.length) {
      // 这句必须留在页面上（docs/UI_REFACTOR_SPEC.md §16）：停用一把 Key，
      // 后端会连它此前发出去的图片链接一起挡掉（backend/app/routers/files.py 里
      // 「Key 已停用 → 403」那一段）。不写明的话，对接方反馈「历史图片集体打不开」时，
      // 没有人会联想到是这里点了「停用」。
      listSection.append(inlineAlert(
        '停用一把 Key，不只是它以后调不通接口：这把 Key 此前发给对方的所有图片链接会立刻一起打不开（对方会看到「这把 API Key 已停用，图片链接同时失效」）。重新启用回来，还没过期的链接就又能打开了。',
        'warning',
        { title: '停用会连带影响已经发出去的图片链接' },
      ));
    }

    if (!state.keys.length) {
      listSection.append(emptyState('key-round', '还没有 API Key', {
        description: '建议为每个外部系统单独创建，以便控制额度和快速停用。',
        action: button('创建第一个密钥', { iconName: 'plus', onclick: createKey }),
      }));
      return;
    }

    listSection.append(h('div', { class: 'dk-key-list' }, state.keys.map((key) =>
      h('article', { class: 'dk-key-row', 'data-state': key.is_active ? 'enabled' : 'disabled' },
        h('div', { class: 'dk-key-main' },
          h('div', {},
            h('h3', {}, key.name),
            h('code', {}, key.key_prefix + '…')),
          statusBadge(key.is_active ? 'active' : 'inactive')),
        h('dl', { class: 'dk-key-stats' },
          // 属主要显示出来：这把 Key 归谁，对接方用它建的任务就花谁的生图余额。
          // 不显示的话，管理员看着一排 Key 完全分不出钱记在谁头上。空串 = 无主的老 Key。
          h('div', {}, h('dt', {}, '归属'), h('dd', {}, key.owner || '—')),
          h('div', {}, h('dt', {}, '每月额度'), h('dd', {}, key.monthly_quota ? `${key.monthly_quota} 次` : '不限')),
          h('div', {}, h('dt', {}, '累计调用'), h('dd', {}, `${key.used_total || 0} 次`)),
          h('div', {}, h('dt', {}, '最近使用'), h('dd', {}, fmtTime(key.last_used_at))),
          h('div', {}, h('dt', {}, '创建时间'), h('dd', {}, fmtTime(key.created_at)))),
        h('div', { class: 'dk-inline-actions' },
          button(key.is_active ? '停用' : '启用', {
            variant: 'secondary',
            size: 'sm',
            iconName: key.is_active ? 'circle-x' : 'play',
            loading: state.pendingKeyIds.has(key.id),
            disabled: state.pendingKeyIds.has(key.id),
            onclick: () => toggleKey(key),
          }),
          button('删除', {
            variant: 'danger', size: 'sm', iconName: 'trash-2', disabled: state.pendingKeyIds.has(key.id), onclick: () => deleteKey(key),
          }))))));
  }

  function createKey() {
    const nameInput = h('input', { class: 'input', placeholder: '例如：金蝶 ERP · 生产环境' });
    const quotaInput = h('input', { class: 'input', type: 'number', min: 1, inputmode: 'numeric', placeholder: '留空表示不限制' });
    const alertRoot = h('div', { 'aria-live': 'polite' });
    let overlay;
    const createButton = button('创建密钥', {
      iconName: 'key-round',
      onclick: async () => {
        alertRoot.replaceChildren();
        const name = nameInput.value.trim();
        const quota = quotaInput.value ? Number(quotaInput.value) : null;
        if (!name) {
          alertRoot.append(inlineAlert('请填写密钥名称。', 'error'));
          nameInput.focus();
          return;
        }
        if (quota !== null && (!Number.isInteger(quota) || quota < 1)) {
          alertRoot.append(inlineAlert('每月额度必须是大于 0 的整数。', 'error'));
          quotaInput.focus();
          return;
        }
        createButton.disabled = true;
        createButton.setAttribute('aria-busy', 'true');
        const sessionAtSubmit = getSessionEpoch();
        try {
          const data = await api.post('/api/web/apikeys', { name, monthly_quota: quota });
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          overlay.close();
          showSecret(data);
          await load();
        } catch (error) {
          if (state.stopped || sessionAtSubmit !== getSessionEpoch()) return;
          alertRoot.append(inlineAlert(error.message, 'error', { title: '创建失败' }));
          createButton.disabled = false;
        } finally {
          createButton.removeAttribute('aria-busy');
        }
      },
    });
    overlay = modal({
      title: '创建 API Key',
      body: h('form', { class: 'dk-panel-stack', onsubmit: (event) => { event.preventDefault(); createButton.click(); } },
        field('密钥名称', nameInput, { required: true, help: '标明使用系统和环境，不要填写密钥本身。' }),
        field('每月调用额度', quotaInput, { help: '超过额度后请求会返回 HTTP 429。' }),
        alertRoot),
      footer: [button('取消', { variant: 'quiet', onclick: () => overlay.close() }), createButton],
    });
    queueMicrotask(() => nameInput.focus());
  }

  function showSecret(data) {
    const apiKey = data.api_key;
    const webhookSecret = data.webhook_secret;
    let overlay;
    const body = h('div', { class: 'dk-secret-reveal' },
      inlineAlert('这两个完整密钥只显示这一次。关闭窗口后无法再从页面恢复，请立即保存到安全的密钥管理工具。', 'warning', { title: '立即备份密钥' }),
      secretRow('API Key', apiKey, 'X-API-Key 请求头', 'API Key 已复制'),
      secretRow('Webhook Secret', webhookSecret, '验证 X-DesignKit-Signature 回调签名', 'Webhook Secret 已复制'));
    overlay = modal({
      title: '密钥创建成功',
      body,
      wide: true,
      closeable: true,
      footer: [button('我已安全保存', { onclick: () => overlay.close() })],
      onClose: () => {
        // 显式清空引用，关闭后不再从前端恢复完整值。
        data.api_key = '';
        data.webhook_secret = '';
        state.secretOverlay = null;
      },
    });
    state.secretOverlay = overlay;
  }

  function secretRow(label, value, help, successMessage) {
    const copyButton = button('复制', { variant: 'secondary', size: 'sm', iconName: 'copy', 'data-label': '复制' });
    copyButton.addEventListener('click', () => copyWithState(copyButton, value, successMessage));
    return h('section', { class: 'dk-secret-row' },
      h('div', {}, h('h3', {}, label), h('p', { class: 'dk-section-meta' }, help)),
      h('code', { tabindex: '0', 'aria-label': label }, value),
      copyButton);
  }

  async function copyWithState(copyButton, value, successMessage) {
    const copied = await copyText(value, successMessage);
    if (!copied) return;
    // 重建「图标 + 文字」结构，直接写 textContent 会把图标和 label 节点清掉且不再恢复
    const label = copyButton.dataset.label || '复制';
    const paint = (iconName, text) => copyButton.replaceChildren(
      icon(iconName), h('span', { class: 'dk-button-label' }, text),
    );
    paint('circle-check', '已复制');
    copyButton.setAttribute('data-state', 'success');
    clearTimeout(copyButton._resetTimer);
    copyButton._resetTimer = setTimeout(() => {
      if (!copyButton.isConnected) return;
      paint('copy', label);
      copyButton.removeAttribute('data-state');
    }, 1800);
  }

  async function toggleKey(key) {
    if (state.pendingKeyIds.has(key.id) || state.stopped) return;
    // 停用要二次确认：这一下是有对外后果的（对接方当场 401、已发出的图片链接
    // 同时打不开），和「删除」一样不该点一下就生效。启用没有破坏性，不拦。
    if (key.is_active) {
      const confirmed = await confirmDialog(
        `停用密钥「${key.name}」？用它对接的系统会立刻调不通（收到 401），`
        + '它此前发出去的图片链接也会立刻打不开。之后可以在这一页把它重新启用。',
        { danger: true, okText: '停用密钥' },
      );
      if (!confirmed || state.stopped) return;
      if (state.pendingKeyIds.has(key.id)) return;
    }
    state.pendingKeyIds.add(key.id);
    renderKeyList();
    try {
      const updated = await api.post('/api/web/apikeys/' + key.id + '/toggle');
      if (state.stopped) return;
      state.keys = state.keys.map((item) => item.id === updated.id ? updated : item);
      toast(updated.is_active ? '密钥已启用' : '密钥已停用', 'success');
    } catch (error) {
      if (state.stopped) return;
      toast(error.message, 'error');
    } finally {
      state.pendingKeyIds.delete(key.id);
      if (!state.stopped) renderKeyList();
    }
  }

  async function deleteKey(key) {
    const confirmed = await confirmDialog(
      `删除密钥「${key.name}」？使用它的所有系统将立即无法调用 API，此操作无法撤销。`,
      { danger: true, okText: '删除密钥' },
    );
    if (!confirmed) return;
    try {
      await api.del('/api/web/apikeys/' + key.id);
      toast('密钥已删除', 'success');
      await load();
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  function renderGuide() {
    const base = location.origin;
    const createExample = `curl -X POST ${base}/api/v1/generations \\\n  -H "X-API-Key: dk_你的密钥" \\\n  -H "Content-Type: application/json" \\\n  -d '{
    "template_id": 1,
    "image_urls": ["https://example.com/product.jpg"],
    "n": 2,
    "size": "1024x1024",
    "callback_url": "https://erp.example.com/designkit/callback",
    "external_ref": "ORDER-2026-001"
  }'`;
    const queryExample = `curl ${base}/api/v1/generations/任务id \\\n  -H "X-API-Key: dk_你的密钥"`;
    const verifyExample = `X-DesignKit-Signature: sha256=<HMAC-SHA256 签名>`;
    guideSection.replaceChildren(
      h('div', { class: 'dk-section-head' },
        h('div', {},
          h('h2', { id: 'api-guide-title', class: 'dk-section-title' }, '接入指南'),
          h('p', { class: 'dk-section-meta' }, '将以下步骤交给对接方开发人员。'))),
      h('ol', { class: 'dk-guide-list' },
        guideStep('1', '创建异步生成任务', '接口会立即返回 job_id，不需要保持长连接。', createExample),
        guideStep('2', '查询任务结果', '可主动查询，也可等待 callback_url 回调。', queryExample),
        guideStep('3', '验证 Webhook 回调', '使用创建 Key 时保存的 Webhook Secret 验证签名。', verifyExample)),
      inlineAlert('不要在前端代码、截图、聊天记录或公开仓库中保存 API Key。', 'warning', { title: '密钥安全' }),
      h('p', { class: 'dk-section-meta' },
        '需要在线调试时，打开 ',
        h('a', { href: '/docs', target: '_blank', rel: 'noopener noreferrer' }, 'OpenAPI 接口文档'),
        '。更完整的回调和签名说明见项目内 docs/erp-api.md。'));
  }

  function guideStep(number, title, description, code) {
    const copyButton = button('复制代码', { variant: 'quiet', size: 'sm', iconName: 'copy', 'data-label': '复制代码' });
    copyButton.addEventListener('click', () => copyWithState(copyButton, code, `${title}代码已复制`));
    return h('li', { class: 'dk-guide-step' },
      h('div', { class: 'dk-guide-step__number', 'aria-hidden': 'true' }, number),
      h('div', {},
        h('h3', {}, title),
        h('p', {}, description),
        h('div', { class: 'dk-code-example' },
          h('div', { class: 'dk-code-example__head' }, h('span', {}, '命令示例'), copyButton),
          h('pre', { class: 'code-block', tabindex: '0' }, code))));
  }

  load();
  return () => {
    state.stopped = true;
    if (state.secretOverlay) state.secretOverlay.close();
  };
}
