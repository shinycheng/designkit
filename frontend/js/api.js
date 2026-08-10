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

// 后端在「这个人还没改掉初始密码」时，会对**所有**业务接口返回 HTTP 403，
// 响应体是 {"detail": "<下面这一串>"}。这里必须逐字一致地硬编码同一句话——
// 出处：backend/app/deps.py 的 MUST_CHANGE_PASSWORD_DETAIL。
// 改后端那个常量时，这里要一起改，否则前端会把「该去改密码了」当成普通报错弹出来，
// 用户看到一句「请先修改初始密码」却没有任何地方可以改，彻底卡死。
export const MUST_CHANGE_PASSWORD_DETAIL = '请先修改初始密码，再使用其他功能';

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
    // 改密闸门：整站任何一条业务接口都可能撞上它（模板、上传、灵感库……）。
    // 在这里统一识别，各个页面就不用各自判断一次，也不会漏。
    // app.js 监听这个事件后会切到「设置安全密码」页，而不是弹一个用户无从下手的报错。
    if (resp.status === 403 && msg === MUST_CHANGE_PASSWORD_DETAIL) {
      window.dispatchEvent(new Event('dk-must-change-password'));
    }
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
