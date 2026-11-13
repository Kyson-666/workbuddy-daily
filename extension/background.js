/**
 * DuMate 登录助手 — background service worker（MV3）
 *
 * 职责（见 deliverables/dumate-signin-design-2026-11-13.md 3.3）：
 *   1. 接收控制台页的登录指令（含 NAS 地址）
 *   2. 打开百度登录页，用户完成登录（唯一人工动作）
 *   3. 检测到登录痕迹后打开 console 页让 BCE 完成 CAS 握手补种 bce-session/bce-sessionid
 *   4. 用 chrome.cookies 读百度系 cookie（含 httpOnly）
 *   5. 把 cookie POST 回 NAS 的 /dumate/api/login/claim（带登录密码）
 *   6. 通知控制台页结果
 *
 * 成功判据对齐 dumate-proxy：以「cookie 能否换到 app token」为准（由 NAS 端验证），
 * 扩展侧只做「有登录痕迹 → 抓取 → 回传」。
 */

const LOGIN_URL = 'https://login.bce.baidu.com/?from=dumate_ext&redirect=https%3A%2F%2Fconsole.bce.baidu.com%2Fdumate%2F';
const CONSOLE_URL = 'https://console.bce.baidu.com/dumate/';
const POLL_MS = 1500;
const HANDSHAKE_MS = 40 * 1000;   // console 页 CAS 握手最长等待
const TOTAL_TIMEOUT_MS = 5 * 60 * 1000;

/** 有登录痕迹的 cookie 名（对齐 dumate-proxy 的 hasLoginTrace） */
const TRACE_NAMES = new Set([
  'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
  'bce-user-info', 'BDUSS', 'bce-login-accountid',
]);

let current = null; // { nasOrigin, adminToken, startedAt, tabId, consoleTabId, status }

/** 百度系域名 */
function isBaiduDomain(d) {
  if (!d) return false;
  d = d.replace(/^\./, '');
  return d === 'baidu.com' || d.endsWith('.baidu.com');
}

/** domain 优先级：.bce.baidu.com 最具体 */
function domainScore(d) {
  d = (d || '').replace(/^\./, '');
  if (d === 'bce.baidu.com') return 3;
  if (d === 'baidu.com') return 2;
  return 1;
}

/** 读全部百度系 cookie，按名称去重保留最具体域 */
async function pickBaiduCookies() {
  const all = await chrome.cookies.getAll({});
  const byName = new Map();
  for (const c of all) {
    if (!isBaiduDomain(c.domain)) continue;
    const prev = byName.get(c.name);
    if (!prev || domainScore(c.domain) > domainScore(prev.domain)) {
      byName.set(c.name, {
        name: c.name,
        value: c.value,
        domain: c.domain,
        path: c.path,
        expires: c.expirationDate || 0,
        httpOnly: c.httpOnly,
        secure: c.secure,
      });
    }
  }
  return [...byName.values()];
}

function hasTrace(cookies) {
  return cookies.some(c => TRACE_NAMES.has(c.name));
}

async function openTab(url) {
  const tab = await chrome.tabs.create({ url, active: true });
  return tab.id;
}

async function closeTab(id) {
  if (id == null) return;
  try { await chrome.tabs.remove(id); } catch {}
}

/** 等待用户登录：轮询 cookie 直到出现登录痕迹 */
async function waitLoginTrace() {
  const deadline = Date.now() + TOTAL_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (!current) throw new Error('已取消');
    const cookies = await pickBaiduCookies();
    if (hasTrace(cookies)) return cookies;
    await new Promise(r => setTimeout(r, POLL_MS));
  }
  throw new Error('登录超时（5 分钟），请重试');
}

/** 等 console 页完成 CAS 握手：bce-session / bce-sessionid 出现或超时 */
async function waitHandshake() {
  const deadline = Date.now() + HANDSHAKE_MS;
  while (Date.now() < deadline) {
    if (!current) throw new Error('已取消');
    const cookies = await pickBaiduCookies();
    if (cookies.some(c => c.name === 'bce-session' || c.name === 'bce-sessionid')) {
      return cookies;
    }
    await new Promise(r => setTimeout(r, POLL_MS));
  }
  // 超时也返回现有 cookie——交给 NAS 端以「能否换 token」判定
  return pickBaiduCookies();
}

/** 回传 cookie 到 NAS（需先确保已申请该 origin 的 host 权限） */
async function submitToNAS(cookies) {
  const resp = await fetch(current.nasOrigin + '/dumate/api/login/claim', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Admin-Token': current.adminToken,
    },
    body: JSON.stringify({ cookies }),
  });
  const text = await resp.text();
  let json = null;
  try { json = JSON.parse(text); } catch {}
  if (!resp.ok || !json || json.ok !== true) {
    const msg = (json && (json.error || json.message)) || ('HTTP ' + resp.status);
    throw new Error('回传失败：' + msg);
  }
  return json;
}

/** 申请 NAS origin 的可选 host 权限（MV3 动态权限，仅用户填写的地址） */
async function ensureNASPermission(nasOrigin) {
  const origin = nasOrigin.endsWith('/') ? nasOrigin + '*' : nasOrigin + '/*';
  const granted = await chrome.permissions.contains({ origins: [origin] });
  if (granted) return true;
  return chrome.permissions.request({ origins: [origin] });
}

/** 主流程 */
async function runLogin(cfg) {
  // 先申请 NAS 地址的 host 权限（用户确认一次即可，之后免询问）
  const granted = await ensureNASPermission(cfg.nasOrigin);
  if (!granted) throw new Error('未授权访问签到服务地址，无法回传登录信息');

  current = { ...cfg, status: 'opening', startedAt: Date.now() };
  notifyPopup('opening', '正在打开百度登录页…');

  current.tabId = await openTab(LOGIN_URL);
  current.status = 'waiting';
  notifyPopup('waiting', '请在打开的页面中登录百度账号…');

  await waitLoginTrace();

  current.status = 'handshake';
  notifyPopup('handshake', '登录成功，正在完成会话握手（约 30~40 秒）…');
  current.consoleTabId = await openTab(CONSOLE_URL);
  await waitHandshake();

  current.status = 'submitting';
  notifyPopup('submitting', '正在回传登录信息…');
  const cookies = await pickBaiduCookies();
  if (cookies.length === 0) throw new Error('未读到百度 cookie');

  const res = await submitToNAS(cookies);

  current.status = 'ok';
  notifyPopup('ok', '登录成功：' + ((res.label || '账号') + ' 已保存到签到服务'));
  await closeTab(current.tabId);
  await closeTab(current.consoleTabId);
  const done = current;
  current = null;
  return { ok: true, label: res.label || '', id: res.id || '' };
}

async function cancelLogin() {
  if (current) {
    await closeTab(current.tabId);
    await closeTab(current.consoleTabId);
    notifyPopup('canceled', '已取消');
    current = null;
  }
}

/** 通知 popup（可能没开，忽略错误） */
function notifyPopup(status, message) {
  chrome.runtime.sendMessage({ type: 'login-status', status, message }).catch(() => {});
}

/* ---------------- 消息入口 ---------------- */

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === 'start-login') {
    if (current) {
      sendResponse({ ok: false, error: '已有登录进行中，请先完成或取消' });
      return;
    }
    runLogin({ nasOrigin: msg.nasOrigin, adminToken: msg.adminToken })
      .then(res => sendResponse(res))
      .catch(e => {
        notifyPopup('error', e.message || String(e));
        sendResponse({ ok: false, error: e.message || String(e) });
        cancelLogin();
      });
    return true; // async response
  }
  if (msg.type === 'cancel-login') {
    cancelLogin().then(() => sendResponse({ ok: true }));
    return true;
  }
  if (msg.type === 'login-state') {
    sendResponse({ active: !!current, status: current ? current.status : 'idle' });
    return false;
  }
});
