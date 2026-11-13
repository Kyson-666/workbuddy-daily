/** DuMate 登录助手 popup：填 NAS 地址 + 密码 → 触发登录 → 展示状态 */
const $status = document.getElementById('status');
const $btn = document.getElementById('btn');
const $cancel = document.getElementById('cancel');
const $nas = document.getElementById('nas');
const $token = document.getElementById('token');

const KEY_NAS = 'dumate_nas_origin';
const KEY_TOKEN = 'dumate_admin_token';

// 记住上次填的地址（密码也存本地，仅自用扩展）
chrome.storage.local.get([KEY_NAS, KEY_TOKEN]).then(v => {
  if (v[KEY_NAS]) $nas.value = v[KEY_NAS];
  if (v[KEY_TOKEN]) $token.value = v[KEY_TOKEN];
});

function setStatus(text, cls) {
  $status.textContent = text;
  $status.className = 'status' + (cls ? ' ' + cls : '');
}

function setBusy(busy) {
  $btn.disabled = busy;
  $cancel.style.display = busy ? '' : 'none';
}

/** 监听 background 的状态推送 */
chrome.runtime.onMessage.addListener(msg => {
  if (msg.type !== 'login-status') return;
  const cls = { ok: 'ok', error: 'bad', canceled: '' }[msg.status] || '';
  setStatus(msg.message || msg.status, cls);
  if (['ok', 'error', 'canceled'].includes(msg.status)) setBusy(false);
});

/**
 * 由 origin 生成 match pattern。
 * Chrome 的匹配模式不支持端口（写 :60005 会报 Invalid value），
 * 而 host 权限本身覆盖该主机的所有端口，所以这里只用 scheme + hostname。
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

$btn.addEventListener('click', () => {
  let origin = $nas.value.trim().replace(/\/+$/, '');
  const token = $token.value.trim();
  if (!origin || !token) {
    setStatus('请填入签到服务地址与登录密码', 'bad');
    return;
  }
  if (!/^https?:\/\//.test(origin)) origin = 'http://' + origin;
  const pattern = patternOf(origin);
  if (!pattern) {
    setStatus('地址格式不正确，请填完整地址，如 https://nas.example.com:17388', 'bad');
    return;
  }
  chrome.storage.local.set({ [KEY_NAS]: origin, [KEY_TOKEN]: token });
  setBusy(true);
  setStatus('正在申请访问权限…', '');

  // 权限确认框是模态的，可能关掉本弹窗、导致下面的回调不再执行。
  // 先把意图写进 storage.session，由 background 的 permissions.onAdded 兜底启动。
  chrome.storage.session.set({ dumate_pending_login: { nasOrigin: origin, adminToken: token } });

  // 关键：permissions.request 必须在用户手势内直接调用。
  // 不能转给 background（跨进程后手势失效），也不能先 await 其他异步 API。
  chrome.permissions.request({ origins: [pattern] }, granted => {
    const err = chrome.runtime.lastError;
    if (err) {
      setStatus('申请访问权限失败：' + err.message, 'bad');
      setBusy(false);
      return;
    }
    if (!granted) {
      chrome.storage.session.remove('dumate_pending_login');
      setStatus('未授权访问该地址，无法回传登录信息', 'bad');
      setBusy(false);
      return;
    }
    setStatus('正在连接…', '');
    chrome.runtime.sendMessage(
      { type: 'start-login', nasOrigin: origin, adminToken: token },
      res => {
        if (chrome.runtime.lastError) {
          // 弹窗即将关闭时的正常现象：流程已由 background 接手
          return;
        }
        if (res && res.ok === false) {
          setStatus(res.error || '启动失败', 'bad');
          setBusy(false);
        }
      }
    );
  });
});

$cancel.addEventListener('click', () => {
  chrome.runtime.sendMessage({ type: 'cancel-login' }, () => setBusy(false));
});
