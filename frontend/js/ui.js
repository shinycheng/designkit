// Accessible, dependency-free UI primitives used by every route.
import { createLucideIcon } from '../vendor/lucide/icons.js';

let uid = 0;
const overlayStack = [];

function nextId(prefix = 'dk') {
  uid += 1;
  return `${prefix}-${uid}`;
}

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === 'class' || key === 'className') el.className = value;
    else if (key === 'style' && typeof value === 'object') Object.assign(el.style, value);
    else if (key === 'dataset' && typeof value === 'object') {
      for (const [dataKey, dataValue] of Object.entries(value)) el.dataset[dataKey] = String(dataValue);
    } else if (key.startsWith('on') && typeof value === 'function') {
      el.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (key === 'value') el.value = value;
    else if (key === 'checked') el.checked = Boolean(value);
    else if (key === 'selected') el.selected = Boolean(value);
    else if (key === 'disabled') el.disabled = Boolean(value);
    else if (key === 'textContent') el.textContent = String(value);
    else el.setAttribute(key, value === true ? '' : String(value));
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child?.nodeType ? child : document.createTextNode(String(child)));
  }
  return el;
}

export function icon(name, options = {}) {
  return createLucideIcon(name, options);
}

export function button(label, options = {}) {
  const {
    variant = 'primary', size = 'md', iconName = '', loading = false,
    className = '', disabledReason = '', ...attrs
  } = options;
  const content = [];
  if (loading) content.push(icon('loader-circle', { className: 'dk-spin' }));
  else if (iconName) content.push(icon(iconName));
  if (label) content.push(h('span', { class: 'dk-button-label' }, label));
  const el = h('button', {
    type: attrs.type || 'button',
    class: `dk-button dk-button--${variant} dk-button--${size}${className ? ` ${className}` : ''}`,
    'data-loading': loading ? 'true' : 'false',
    'aria-busy': loading ? 'true' : null,
    ...attrs,
  }, content);
  if (loading) el.disabled = true;
  if (el.disabled && disabledReason) {
    el.title = disabledReason;
    el.setAttribute('aria-description', disabledReason);
  }
  return el;
}

export function iconButton(name, label, options = {}) {
  const { variant = 'quiet', size = 'md', className = '', ...attrs } = options;
  return h('button', {
    type: 'button',
    class: `dk-icon-button dk-icon-button--${variant} dk-icon-button--${size}${className ? ` ${className}` : ''}`,
    'aria-label': label,
    title: label,
    ...attrs,
  }, icon(name));
}

export function field(labelText, control, { required = false, help = '', error = '' } = {}) {
  const labelledControl = control.matches?.('input,select,textarea,button,[contenteditable="true"]')
    ? control
    : control.querySelector?.('input,select,textarea,[contenteditable="true"]') || control;
  const id = labelledControl.id || nextId('field');
  labelledControl.id = id;
  if (required) labelledControl.setAttribute('aria-required', 'true');
  const helpId = help ? `${id}-help` : '';
  const errorId = error ? `${id}-error` : '';
  const describedBy = [labelledControl.getAttribute('aria-describedby'), helpId, errorId].filter(Boolean).join(' ');
  if (describedBy) labelledControl.setAttribute('aria-describedby', describedBy);
  if (error) {
    labelledControl.setAttribute('aria-invalid', 'true');
  }
  return h('div', { class: 'dk-field field', 'data-invalid': error ? 'true' : 'false' },
    h('label', { class: 'dk-field-label field-label', for: id },
      labelText,
      required ? h('span', { class: 'dk-required req', 'aria-hidden': 'true' }, ' *') : null),
    control,
    help ? h('div', { class: 'dk-field-help field-help', id: helpId }, help) : null,
    error ? h('div', { class: 'dk-field-error', id: errorId, role: 'alert' }, error) : null,
  );
}

export function inlineAlert(message, type = 'info', options = {}) {
  const iconName = { info: 'info', success: 'circle-check', warning: 'circle-alert', danger: 'shield-alert', error: 'circle-alert' }[type] || 'info';
  return h('div', {
    class: `dk-alert dk-alert--${type}${options.className ? ` ${options.className}` : ''}`,
    role: type === 'danger' || type === 'error' ? 'alert' : 'status',
  }, icon(iconName), h('div', { class: 'dk-alert-content' },
    options.title ? h('strong', { class: 'dk-alert-title' }, options.title) : null,
    h('div', { class: 'dk-alert-message' }, message)));
}

export function toast(message, type = 'info', ms) {
  const root = document.getElementById('toast-root');
  if (!root) return null;
  root.setAttribute('aria-live', type === 'error' ? 'assertive' : 'polite');
  root.setAttribute('aria-atomic', 'false');
  const duration = ms ?? (type === 'error' ? 5600 : 2600);
  const el = h('div', { class: `dk-toast toast${type !== 'info' ? ` ${type}` : ''}`, role: type === 'error' ? 'alert' : 'status' },
    icon(type === 'success' ? 'circle-check' : type === 'error' ? 'circle-alert' : 'info'),
    h('span', {}, message),
    iconButton('x', '关闭提示', { className: 'dk-toast-close', onclick: () => el.remove() }),
  );
  root.append(el);
  if (duration > 0) window.setTimeout(() => el.remove(), duration);
  return el;
}

function focusable(root) {
  return [...root.querySelectorAll('a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])')]
    .filter((node) => node.getClientRects().length > 0);
}

export function modal({ title, body, footer, wide = false, onClose, closeable = true, drawer = false, labelledBy = '' }) {
  const root = document.getElementById('modal-root');
  const returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const titleId = labelledBy || nextId('dialog-title');
  let closed = false;

  const close = (reason = 'programmatic') => {
    if (closed) return;
    closed = true;
    document.removeEventListener('keydown', onKeydown, true);
    mask.dataset.state = 'closing';
    mask.remove();
    const index = overlayStack.indexOf(api);
    if (index >= 0) overlayStack.splice(index, 1);
    onClose?.(reason);
    if (returnFocus?.isConnected) returnFocus.focus();
  };

  const panel = h('div', {
    class: `dk-dialog modal${wide ? ' wide dk-dialog--wide' : ''}${drawer ? ' dk-drawer' : ''}`,
    role: 'dialog',
    'aria-modal': 'true',
    'aria-labelledby': titleId,
    tabindex: '-1',
  },
    h('div', { class: `${drawer ? 'dk-drawer__head' : 'dk-dialog__head'} dk-dialog-head modal-head` },
      h('h2', { class: `${drawer ? 'dk-drawer__title' : 'dk-dialog__title'} dk-dialog-title modal-title`, id: titleId }, title),
      closeable ? iconButton('x', '关闭', { className: `${drawer ? 'dk-drawer__close' : 'dk-dialog__close'} modal-close`, onclick: () => close('close-button') }) : null),
    h('div', { class: `${drawer ? 'dk-drawer__body' : 'dk-dialog__body'} dk-dialog-body modal-body` }, body),
    footer ? h('div', { class: `${drawer ? 'dk-drawer__foot' : 'dk-dialog__foot'} dk-dialog-foot modal-foot` }, footer.filter(Boolean)) : null,
  );

  const mask = h('div', {
    class: `dk-overlay modal-mask${drawer ? ' dk-overlay--drawer' : ''}`,
    'data-state': 'opening',
    onclick: (event) => { if (closeable && event.target === mask) close('backdrop'); },
  }, panel);

  function onKeydown(event) {
    if (overlayStack.at(-1) !== api) return;
    if (event.key === 'Escape' && closeable) {
      event.preventDefault();
      close('escape');
      return;
    }
    if (event.key !== 'Tab') return;
    const items = focusable(panel);
    if (!items.length) {
      event.preventDefault();
      panel.focus();
      return;
    }
    const first = items[0];
    const last = items.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault(); last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault(); first.focus();
    }
  }

  const api = { close, mask, panel };
  overlayStack.push(api);
  root.append(mask);
  document.addEventListener('keydown', onKeydown, true);
  requestAnimationFrame(() => {
    mask.dataset.state = 'open';
    (focusable(panel)[0] || panel).focus();
  });
  return api;
}

export function drawer(options) {
  return modal({ ...options, drawer: true, wide: true });
}

export function closeOverlays() {
  [...overlayStack].reverse().forEach((overlay) => overlay.close('route-change'));
}

export function confirmDialog(message, { danger = false, okText = '确定', title = '请确认' } = {}) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (value) => {
      if (settled) return;
      settled = true;
      resolve(value);
    };
    const cancel = button('取消', { variant: 'secondary', onclick: () => { done(false); dialog.close('cancel'); } });
    const ok = button(okText, { variant: danger ? 'danger' : 'primary', onclick: () => { done(true); dialog.close('confirm'); } });
    const dialog = modal({
      title,
      body: h('p', { class: 'dk-dialog-message' }, message),
      footer: [cancel, ok],
      onClose: () => done(false),
    });
  });
}

export function lightbox(url, { alt = '图片预览', filename = '' } = {}) {
  const image = h('img', { src: url, alt, class: 'dk-lightbox-image' });
  const actions = filename ? [button('下载', { variant: 'secondary', iconName: 'download', onclick: () => downloadUrl(url, filename) })] : null;
  return modal({ title: alt, body: h('div', { class: 'dk-lightbox' }, image), footer: actions, wide: true });
}

export function statusBadge(status) {
  const map = {
    pending: ['排队中', 'warning', 'clock'],
    processing: ['生成中', 'info', 'loader-circle'],
    succeeded: ['已完成', 'success', 'circle-check'],
    failed: ['失败', 'danger', 'circle-alert'],
    active: ['启用', 'success', 'circle-check'],
    inactive: ['停用', 'neutral', 'circle-x'],
  };
  const [label, tone, iconName] = map[status] || [String(status || '未知'), 'neutral', 'info'];
  return h('span', { class: `dk-badge dk-badge--${tone} badge ${tone}`, 'data-status': status }, icon(iconName, { size: 14 }), label);
}

export function emptyState(iconName, title, options = {}) {
  // Backwards-compatible two-argument calls now render a real library icon.
  const description = options.description || '';
  const action = options.action || null;
  const safeName = typeof iconName === 'string' && /^[a-z0-9-]+$/.test(iconName) ? iconName : 'image';
  return h('div', { class: 'dk-empty empty' },
    h('div', { class: 'dk-empty-icon empty-icon', 'aria-hidden': 'true' }, icon(safeName, { size: 32 })),
    h('h2', { class: 'dk-empty-title' }, title),
    description ? h('p', { class: 'dk-empty-description' }, description) : null,
    action ? h('div', { class: 'dk-empty-action' }, action) : null,
  );
}

export function skeleton({ lines = 3, className = '' } = {}) {
  return h('div', { class: `dk-skeleton${className ? ` ${className}` : ''}`, 'aria-label': '正在加载', role: 'status' },
    Array.from({ length: lines }, (_, index) => h('span', { class: 'dk-skeleton-line', 'data-line': index + 1 })),
  );
}

export function segmentedControl(items, value, onChange, { label = '选择选项', className = '' } = {}) {
  const controls = items.map((item, index) => h('button', {
    type: 'button',
    class: 'dk-segmented-item',
    role: 'radio',
    'aria-checked': item.value === value ? 'true' : 'false',
    'data-selected': item.value === value ? 'true' : 'false',
    tabindex: item.value === value || (!items.some((entry) => entry.value === value) && index === 0) ? '0' : '-1',
    onclick: () => onChange(item.value),
    onkeydown: (event) => {
      if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)) return;
      event.preventDefault();
      const step = event.key === 'ArrowRight' || event.key === 'ArrowDown' ? 1 : -1;
      const next = (index + step + items.length) % items.length;
      controls[next].focus();
      onChange(items[next].value);
    },
  }, item.iconName ? icon(item.iconName) : null, item.label));
  return h('div', { class: `dk-segmented${className ? ` ${className}` : ''}`, role: 'radiogroup', 'aria-label': label }, controls);
}

export function fmtTime(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return String(iso);
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(date).replaceAll('/', '-');
}

export async function copyText(value, okMsg = '已复制') {
  const text = String(value ?? '');
  try {
    if (!navigator.clipboard || !window.isSecureContext) throw new Error('clipboard unavailable');
    await navigator.clipboard.writeText(text);
  } catch {
    const textarea = h('textarea', { value: text, class: 'dk-visually-hidden' });
    document.body.append(textarea);
    textarea.select();
    const copied = document.execCommand('copy');
    textarea.remove();
    if (!copied) {
      toast('复制失败，请手动选择文本复制', 'error');
      return false;
    }
  }
  toast(okMsg, 'success');
  return true;
}

export function downloadUrl(url, filename = '') {
  const anchor = h('a', { href: url, download: filename, class: 'dk-visually-hidden' });
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
}

export function setButtonLoading(target, loading, label = '') {
  if (!target) return;
  target.dataset.loading = loading ? 'true' : 'false';
  target.setAttribute('aria-busy', loading ? 'true' : 'false');
  target.disabled = Boolean(loading);
  const labelNode = target.querySelector('.dk-button-label');
  if (label && labelNode) labelNode.textContent = label;
}
