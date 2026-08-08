// 与后端交互的统一入口：自动带登录令牌、统一错误处理
const TOKEN_KEY = 'dk_token';
const USER_KEY = 'dk_user';
let sessionEpoch = 0;

export function getToken() { return localStorage.getItem(TOKEN_KEY) || ''; }
export function getSessionEpoch() { return sessionEpoch; }
export function setSession(token, user) {
  sessionEpoch += 1;
  localStorage.setItem(TOKEN_KEY, token);
  localStorage.setItem(USER_KEY, JSON.stringify(user));
}
export function clearSession() {
  sessionEpoch += 1;
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
}
export function getUser() {
  try { return JSON.parse(localStorage.getItem(USER_KEY) || 'null'); }
  catch { return null; }
}

export class ApiError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

async function request(method, path, body, options = {}) {
  const headers = { ...(options.headers || {}) };
  const token = getToken();
  const sessionAtRequest = getSessionEpoch();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  let payload = body;
  if (body && !(body instanceof FormData)) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, { method, headers, body: payload, signal: options.signal });
  } catch (error) {
    if (error?.name === 'AbortError') throw error;
    throw new ApiError('无法连接服务器，请确认服务已启动', 0);
  }
  if (resp.status === 401 && !path.includes('/auth/login')) {
    if (token === getToken() && sessionAtRequest === getSessionEpoch()) {
      clearSession();
      window.dispatchEvent(new Event('dk-unauthorized'));
    }
    throw new ApiError('登录已过期，请重新登录', 401);
  }
  let data = null;
  try { data = await resp.json(); } catch { /* 空响应 */ }
  if (!resp.ok) {
    let msg = (data && (data.detail || data.message)) || ('请求失败（HTTP ' + resp.status + '）');
    if (Array.isArray(msg)) msg = msg.map(e => e.msg || JSON.stringify(e)).join('；');
    if (typeof msg === 'object') msg = JSON.stringify(msg);
    throw new ApiError(msg, resp.status);
  }
  return data;
}

export const api = {
  get: (path, options) => request('GET', path, undefined, options),
  post: (path, body, options) => request('POST', path, body, options),
  put: (path, body, options) => request('PUT', path, body, options),
  del: (path, options) => request('DELETE', path, undefined, options),
};
