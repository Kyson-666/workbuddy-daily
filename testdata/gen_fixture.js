// 用上游 worker.js 的同一套算法生成基准值，供 Go 测试逐字节比对。
const { webcrypto } = require('crypto');
const crypto = webcrypto;

function qsQuote(v) {
  return encodeURIComponent(String(v)).replace(/%2F/g, '/');
}

const OAUTH_APP_VERSION = '3.3.100';
const OAUTH_PLUGIN_VERSION = '2.3.83560';

function buildAuthorizeUrl(o) {
  const params = [
    ['login_version', '1'],
    ['auth_from', o.authFrom],
    ['login_channel', 'native_ide'],
    ['plugin_version', OAUTH_PLUGIN_VERSION],
    ['auth_type', 'local'],
    ['client_id', o.clientId],
    ['redirect', '0'],
    ['login_trace_id', o.trace],
    ['auth_callback_url', o.callback],
    ['machine_id', o.machineId],
    ['device_id', o.deviceId],
    ['x_device_id', o.deviceId],
    ['x_machine_id', o.machineId],
    ['x_device_brand', 'PC'],
    ['x_device_type', 'Windows'],
    ['x_os_version', '10.0.19045'],
    ['x_env', ''],
    ['x_app_version', OAUTH_APP_VERSION],
    ['x_app_type', 'stable'],
    ['code_challenge', o.challenge],
    ['code_challenge_method', 'S256'],
    ['channel_name', 'common'],
  ];
  return 'https://www.trae.cn/authorization?' + params.map(([k, v]) => k + '=' + qsQuote(v)).join('&');
}

function b64encode(buf) {
  return Buffer.from(new Uint8Array(buf)).toString('base64');
}

async function seal(masterB64, info, obj, saltHex, ivHex) {
  const master = Buffer.from(masterB64, 'base64');
  const salt = Buffer.from(saltHex, 'hex');
  const iv = Buffer.from(ivHex, 'hex');
  const base = await crypto.subtle.importKey('raw', master, 'HKDF', false, ['deriveKey']);
  const key = await crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt, info: new TextEncoder().encode(info) },
    base,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt']
  );
  const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, new TextEncoder().encode(JSON.stringify(obj)));
  return JSON.stringify({ v: 1, s: b64encode(salt), i: b64encode(iv), d: b64encode(ct) });
}

(async () => {
  const out = {};
  out.authorize_url = buildAuthorizeUrl({
    authFrom: 'trae',
    clientId: 'ono9krqynydwx5',
    trace: 'abc123',
    callback: 'http://127.0.0.1:17388/authorize',
    deviceId: '123456789012345',
    machineId: 'deadbeefdeadbeef',
    challenge: 'CHAL_xyz-1',
  });
  out.encoded = encodeURIComponent("a b/中!*'()~._-:@&=+$,#?");
  out.master = 'MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=';
  out.sealed = await seal(
    out.master,
    'trae-signin/v1/account',
    { id: 'x1', label: '测试', refreshToken: 'RT-abc' },
    '000102030405060708090a0b0c0d0e0f',
    '0a0b0c0d0e0f101112131415'
  );
  console.log(JSON.stringify(out, null, 2));
})();
