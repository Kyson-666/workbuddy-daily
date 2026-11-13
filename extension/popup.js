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

$btn.addEventListener('click', () => {
  let origin = $nas.value.trim().replace(/\/+$/, '');
  const token = $token.value.trim();
  if (!origin || !token) {
    setStatus('请填入签到服务地址与登录密码', 'bad');
    return;
  }
  if (!/^https?:\/\//.test(origin)) origin = 'http://' + origin;
  chrome.storage.local.set({ [KEY_NAS]: origin, [KEY_TOKEN]: token });
  setBusy(true);
  setStatus('正在连接…', '');
  chrome.runtime.sendMessage(
    { type: 'start-login', nasOrigin: origin, adminToken: token },
    res => {
      if (chrome.runtime.lastError) {
        setStatus('扩展通信失败：' + chrome.runtime.lastError.message, 'bad');
        setBusy(false);
        return;
      }
      if (res && res.ok === false) {
        setStatus(res.error || '启动失败', 'bad');
        setBusy(false);
      }
    }
  );
});

$cancel.addEventListener('click', () => {
  chrome.runtime.sendMessage({ type: 'cancel-login' }, () => setBusy(false));
});
