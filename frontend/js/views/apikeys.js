// API 对接：密钥管理 + 给 ERP 开发者的快速上手说明
import { api } from '../api.js';
import { confirmDialog, copyText, emptyState, field, fmtTime, h, modal, toast } from '../ui.js';

export function renderApiKeys(container) {
  const listCard = h('div', { class: 'card' });
  const docsCard = h('div', { class: 'card', style: { marginTop: '16px' } });
  container.append(listCard, docsCard);

  async function load() {
    let keys = [];
    try { keys = await api.get('/api/web/apikeys'); }
    catch (e) { return toast(e.message, 'error'); }

    listCard.innerHTML = '';
    listCard.append(
      h('div', { class: 'card-title' }, '对外 API 密钥',
        h('span', { class: 'hint' }, '给 ERP 等第三方系统调用本平台时使用')),
      h('div', { style: { marginBottom: '14px' } },
        h('button', { class: 'btn', onclick: createKey }, '＋ 创建密钥')));

    if (!keys.length) {
      listCard.append(emptyState('🔑', '还没有创建 API 密钥'));
    } else {
      listCard.append(h('table', { class: 'table' },
        h('thead', {}, h('tr', {},
          h('th', {}, '名称'), h('th', {}, '密钥前缀'), h('th', {}, '状态'),
          h('th', {}, '月额度'), h('th', {}, '累计调用'), h('th', {}, '最近使用'), h('th', { style: { width: '150px' } }, '操作'))),
        h('tbody', {}, keys.map(k =>
          h('tr', {},
            h('td', { style: { fontWeight: 600 } }, k.name),
            h('td', {}, h('code', {}, k.key_prefix + '…')),
            h('td', {}, k.is_active ? h('span', { class: 'badge green' }, '启用') : h('span', { class: 'badge gray' }, '停用')),
            h('td', {}, k.monthly_quota ? k.monthly_quota + ' 次/月' : '不限'),
            h('td', {}, k.used_total + ' 次'),
            h('td', {}, fmtTime(k.last_used_at)),
            h('td', {},
              h('button', {
                class: 'btn ghost sm', onclick: async () => {
                  try { await api.post('/api/web/apikeys/' + k.id + '/toggle'); load(); }
                  catch (e) { toast(e.message, 'error'); }
                },
              }, k.is_active ? '停用' : '启用'), ' ',
              h('button', {
                class: 'btn danger sm', onclick: async () => {
                  if (!(await confirmDialog(`删除密钥「${k.name}」后，使用它的系统将立即无法调用。确定？`, { danger: true, okText: '删除' }))) return;
                  try { await api.del('/api/web/apikeys/' + k.id); toast('已删除', 'success'); load(); }
                  catch (e) { toast(e.message, 'error'); }
                },
              }, '删除')))))));
    }
    renderDocs();
  }

  function createKey() {
    const nameInput = h('input', { class: 'input', placeholder: '例如：金蝶ERP-生产环境' });
    const quotaInput = h('input', { class: 'input', type: 'number', min: 1, placeholder: '留空 = 不限制' });
    const m = modal({
      title: '创建 API 密钥',
      body: h('div', {},
        field('名称', nameInput, { required: true, help: '标注给哪个系统用，方便日后管理' }),
        field('每月调用额度', quotaInput, { help: '超过额度后调用会被拒绝（HTTP 429），留空表示不限制' })),
      footer: [h('button', {
        class: 'btn', onclick: async () => {
          if (!nameInput.value.trim()) return toast('请填写名称', 'error');
          try {
            const data = await api.post('/api/web/apikeys', {
              name: nameInput.value.trim(),
              monthly_quota: quotaInput.value ? parseInt(quotaInput.value) : null,
            });
            m.close(); showSecret(data); load();
          } catch (e) { toast(e.message, 'error'); }
        },
      }, '创建')],
    });
  }

  function showSecret(data) {
    modal({
      title: '密钥创建成功（只显示这一次！）',
      body: h('div', {},
        h('p', { style: { color: 'var(--danger)', fontWeight: 600, marginBottom: '12px' } },
          '⚠️ 请立即复制并妥善保存，关闭此窗口后将无法再次查看完整密钥。'),
        h('div', { class: 'kv' }, 'API Key（调用接口时放在请求头 X-API-Key）：'),
        h('div', { class: 'secret-box' }, h('span', {}, data.api_key),
          h('button', { class: 'btn sm', onclick: () => copyText(data.api_key) }, '复制')),
        h('div', { class: 'kv' }, 'Webhook 签名密钥（校验回调来源用，给 ERP 开发者）：'),
        h('div', { class: 'secret-box' }, h('span', {}, data.webhook_secret),
          h('button', { class: 'btn sm', onclick: () => copyText(data.webhook_secret) }, '复制'))),
    });
  }

  function renderDocs() {
    const base = location.origin;
    docsCard.innerHTML = '';
    docsCard.append(
      h('div', { class: 'card-title' }, 'ERP 对接快速上手',
        h('span', { class: 'hint' }, '把这一段发给对接方的开发人员即可')),
      h('div', { class: 'kv' }, '1️⃣ 提交生成任务（异步，立即返回 job_id）：'),
      h('div', { class: 'code-block' },
`curl -X POST ${base}/api/v1/generations \\
  -H "X-API-Key: dk_你的密钥" \\
  -H "Content-Type: application/json" \\
  -d '{
    "template_id": 1,
    "image_urls": ["https://你的图片地址/product.jpg"],
    "n": 2,
    "size": "1024x1024",
    "callback_url": "https://你的ERP/回调地址",
    "external_ref": "订单号ABC123"
  }'`),
      h('div', { class: 'kv' }, '2️⃣ 查询任务结果（或等回调）：'),
      h('div', { class: 'code-block' },
`curl ${base}/api/v1/generations/任务id -H "X-API-Key: dk_你的密钥"`),
      h('div', { class: 'kv' }, '3️⃣ 任务完成后平台会 POST 回调到 callback_url，带 X-DesignKit-Signature 签名头。'),
      h('p', { style: { fontSize: '13px', color: 'var(--text-2)', marginTop: '10px' } },
        '完整接口文档（可在线调试）：',
        h('a', { href: '/docs', target: '_blank' }, base + '/docs'),
        '，详细对接说明见项目内 docs/erp-api.md。'));
  }

  load();
}
