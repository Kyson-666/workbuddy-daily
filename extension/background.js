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
 *
 * v1.0.3 修复「登录第二个账号时页面刚打开就被关掉」（2026-11-13 线上问题）：
 *   原判据只看「有没有登录痕迹」，而上一账号的 cookie 仍在浏览器里未清，
 *   于是第一轮轮询立刻命中 → 直接跳握手并关掉标签页，回传的还是旧账号。
 *   现改为**身份签名基线比对**：启动时先记录身份类 cookie 的签名，
 *   只有签名发生变化（新账号必然改写 bce-session/BDUSS/accountid）才算登录完成。
 */

const LOGIN_URL = 'https://login.bce.baidu.com/?from=dumate_ext&redirect=https%3A%2F%2Fconsole.bce.baidu.com%2Fdumate%2F';
const CONSOLE_URL = 'https://console.bce.baidu.com/dumate/';
const POLL_MS = 1500;
const HANDSHAKE_MS = 40 * 1000;   // console 页 CAS 握手最长等待
const TOTAL_TIMEOUT_MS = 5 * 60 * 1000;
/** 签名需要连续两次采样一致才认（避免读到写一半的过渡态） */
const STABLE_MS = 2000;

/** 有登录痕迹的 cookie 名（对齐 dumate-proxy 的 hasLoginTrace） */
const TRACE_NAMES = new Set([
  'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
  'bce-user-info', 'BDUSS', 'bce-login-accountid',
]);

/** 身份类 cookie：换账号时这些值必然变化，用于区分「上一账号残留」与「本次真的登录」 */
const IDENTITY_NAMES = new Set([
  'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
  'bce-user-info', 'BDUSS', 'bce-login-accountid', 'bce-login-display-name',
  'bce-login-domain-account', 'STOKEN', 'PTOKEN',
]);

let current = null; // { nasOrigin, adminToken, startedAt, tabId, consoleTabId, status, baseline }

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

/**
 * 身份签名：只取与账号身份相关的 cookie 拼成可比较字符串。
 * 换账号时这些值必然变化，因此可用来区分「上一账号的残留」与「本次真的登录成功」。
 */
function identitySignature(cookies) {
  return cookies
    .filter(c => IDENTITY_NAMES.has(c.name))
    .map(c => c.name + '=' + c.value)
    .sort()
    .join('|');
}

async function openTab(url) {
  const tab = await chrome.tabs.create({ url, active: true });
  return tab.id;
}

async function closeTab(id) {
  if (id == null) return;
  try { await chrome.tabs.remove(id); } catch {}
}

/**
 * 等待「新账号登录完成」：必须相对基线发生变化，且连续两次采样一致。
 * @param {string} baseline 启动时的身份签名
 */
async function waitLoginTrace(baseline) {
  const deadline = Date.now() + TOTAL_TIMEOUT_MS;
  let pendingSig = null;
  let pendingAt = 0;
  while (Date.now() < deadline) {
    if (!current) throw new Error('已取消');
    const cookies = await pickBaiduCookies();
    const sig = identitySignature(cookies);
    // 变了 + 连续两次一致（稳定）才算数，避免读到登录过程中的中间态
    if (sig && sig !== baseline) {
      if (sig !== pendingSig) { pendingSig = sig; pendingAt = Date.now(); }
      else if (Date.now() - pendingAt >= STABLE_MS) return cookies;
    }
    await new Promise(r => setTimeout(r, POLL_MS));
  }
  throw new Error('登录超时（5 分钟）。若该账号此前已在此浏览器登录过，请先在百度页面退出登录，或改用无痕窗口后再试');
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

/**
 * 由 origin 生成 match pattern。
 * Chrome 匹配模式不支持端口，host 权限本身覆盖该主机全部端口，故只用 scheme + hostname。
 */
function patternOf(origin) {
  try {
    const u = new URL(origin);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u.protocol + '//' + u.hostname + '/*';
  } catch (e) {
    return null;
  }
}

/**
 * 检查 NAS origin 的 host 权限。
 * 注意：这里**只检查不申请** —— permissions.request 必须在用户手势内调用，
 * 由 popup 的点击处理器发起；service worker 里调用会报
 * "This function must be called during a user gesture"。
 */
async function hasNASPermission(nasOrigin) {
  const pattern = patternOf(nasOrigin);
  if (!pattern) return false;
  return chrome.permissions.contains({ origins: [pattern] });
}

/** 主流程 */
async function runLogin(cfg) {
  if (!(await hasNASPermission(cfg.nasOrigin))) {
    throw new Error('缺少访问 ' + cfg.nasOrigin + ' 的权限，请回到扩展弹窗重新点一次「登录百度账号」并允许访问');
  }
  // ★ 关键修复：启动时先记录基线签名，之后只有签名变化才算登录成功
  const baseline = identitySignature(await pickBaiduCookies());
  current = { ...cfg, status: 'opening', startedAt: Date.now(), baseline };
  notifyPopup('opening', '正在打开百度登录页…');

  current.tabId = await openTab(LOGIN_URL);
  current.status = 'waiting';
  notifyPopup('waiting', '请在打开的页面中登录百度账号（若当前浏览器已登录其他号，请先退出或使用无痕窗口）…');

  await waitLoginTrace(baseline);

  current.status = 'handshake';
  notifyPopup('handshake', '检测到新登录，正在完成会话握手（约 30~40 秒）…');
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

const KEY_PENDING = 'dumate_pending_login';
let starting = false; // 同步互斥：覆盖 runLogin 首个 await 之前的窗口

/**
 * 统一入口。两条触发路径（弹窗回调 / permissions.onAdded）都会走到这里，
 * 用同步标志位去重，避免权限确认框关闭弹窗后重复启动。
 */
function startLogin(cfg) {
  if (current) return Promise.reject(new Error('已有登录进行中，请先完成或取消'));
  if (starting) return Promise.resolve({ ok: true, deduped: true });
  starting = true;
  chrome.storage.session.remove(KEY_PENDING).catch(() => {});
  return runLogin(cfg).finally(() => { starting = false; });
}

/**
 * 安全网：权限确认框是模态的，会关掉扩展弹窗，弹窗里的回调随之丢失。
 * 权限授予成功后由这里接手，保证登录流程一定启动。
 */
chrome.permissions.onAdded.addListener(async () => {
  let pending = null;
  try {
    const v = await chrome.storage.session.get(KEY_PENDING);
    pending = v[KEY_PENDING] || null;
  } catch (e) {
    return;
  }
  if (!pending || !pending.nasOrigin) return;
  try {
    await startLogin({ nasOrigin: pending.nasOrigin, adminToken: pending.adminToken });
  } catch (e) {
    if (e && e.message) notifyPopup('error', e.message);
  }
});

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === 'start-login') {
    startLogin({ nasOrigin: msg.nasOrigin, adminToken: msg.adminToken })
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
