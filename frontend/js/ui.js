// 轻量 UI 工具：DOM 构建、弹窗、提示、通用格式化

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k === 'disabled') el.disabled = !!v;
    else el.setAttribute(k, v);
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child.nodeType ? child : document.createTextNode(String(child)));
  }
  return el;
}

export function toast(message, type = 'info', ms = 2600) {
  const root = document.getElementById('toast-root');
  const el = h('div', { class: 'toast ' + (type === 'info' ? '' : type) }, message);
  root.append(el);
  setTimeout(() => el.remove(), ms);
}

export function modal({ title, body, footer, wide = false, onClose }) {
  const root = document.getElementById('modal-root');
  const close = () => { mask.remove(); if (onClose) onClose(); };
  const mask = h('div', { class: 'modal-mask', onclick: (e) => { if (e.target === mask) close(); } },
    h('div', { class: 'modal' + (wide ? ' wide' : '') },
      h('div', { class: 'modal-head' },
        h('div', { class: 'modal-title' }, title),
        h('button', { class: 'modal-close', onclick: close }, '✕')),
      h('div', { class: 'modal-body' }, body),
      footer ? h('div', { class: 'modal-foot' }, footer) : null,
    ));
  root.append(mask);
  return { close, mask };
}

export function confirmDialog(message, { danger = false, okText = '确定' } = {}) {
  return new Promise((resolve) => {
    // settled 保证只 resolve 一次：close() 会触发 onClose，若不加标志，
    // 点「确定」时 m.close() 先 resolve(false) 会盖掉 resolve(true)
    let settled = false;
    const done = (v) => { if (!settled) { settled = true; resolve(v); } };
    const okBtn = h('button', { class: 'btn' + (danger ? ' danger' : ''), onclick: () => { done(true); m.close(); } }, okText);
    const cancelBtn = h('button', { class: 'btn ghost', onclick: () => { done(false); m.close(); } }, '取消');
    const m = modal({
      title: '请确认', body: h('div', {}, message), footer: [cancelBtn, okBtn],
      onClose: () => done(false),
    });
  });
}

export function lightbox(url) {
  const el = h('div', { class: 'lightbox', onclick: () => el.remove() }, h('img', { src: url }));
  document.body.append(el);
}

export function statusBadge(status) {
  const map = {
    pending: ['排队中', 'amber'],
    processing: ['生成中', 'blue'],
    succeeded: ['已完成', 'green'],
    failed: ['失败', 'red'],
  };
  const [label, color] = map[status] || [status, 'gray'];
  return h('span', { class: 'badge ' + color }, label);
}

export function fmtTime(iso) {
  if (!iso) return '-';
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function copyText(text, okMsg = '已复制') {
  const fallback = () => {
    try {
      const ta = h('textarea', { value: text, style: { position: 'fixed', opacity: 0 } });
      document.body.append(ta); ta.select();
      document.execCommand('copy'); ta.remove();
      toast(okMsg, 'success');
    } catch { toast('复制失败，请手动选择文本复制', 'error'); }
  };
  // navigator.clipboard 仅在 https / localhost 可用；普通 http 部署下会抛错，需降级
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(() => toast(okMsg, 'success'), fallback);
  } else {
    fallback();
  }
}

export function field(labelText, control, { required = false, help = '' } = {}) {
  return h('div', { class: 'field' },
    h('label', { class: 'field-label' }, labelText, required ? h('span', { class: 'req' }, '*') : null),
    control,
    help ? h('div', { class: 'field-help' }, help) : null);
}

export function emptyState(icon, text) {
  return h('div', { class: 'empty' }, h('div', { class: 'empty-icon' }, icon), h('div', {}, text));
}

export function downloadUrl(url, filename) {
  const a = h('a', { href: url, download: filename || '' });
  document.body.append(a); a.click(); a.remove();
}
