/**
 * WorkBuddy 每日签到 —— Cloudflare Worker
 *
 * 整个项目就这一个文件，没有构建步骤、没有 npm 依赖、没有 import。
 * 复制粘贴到 Cloudflare 控制台的在线编辑器也能直接跑。
 *
 * 设计要点：
 *  1. 不依赖任何客户端。用 refresh_token 自己换 access_token，自续期。
 *  2. 账号可以用微信扫码登录直接获得，不需要手动提取令牌。
 *  3. 账号数据存 KV（加密后存），KV 里永远不出现明文令牌。
 *  4. 签到接口幂等，重复跑只会有一次真正生效，定时器多触发几次无所谓。
 *  5. 登录密码（ADMIN_TOKEN）与账号令牌分离，泄露一个不会连坐。
 *
 * 必需的环境绑定：
 *   KV 绑定名   WB_KV
 *   Secret      WB_MASTER_KEY   32 字节随机值的 base64（密钥加密密钥）
 *   Secret      ADMIN_TOKEN     页面登录密码，随便一串长随机
 *   Secret      CRON_SECRET     （可选）给 /cron 用的独立口令，缺省回落到 ADMIN_TOKEN
 *
 * 部署：Cloudflare 控制台 → Workers → 新建 → 粘贴本文件全文 → 绑定 KV 与 Secrets
 *
 * 添加账号只有一种方式：页面上内嵌官方登录页，微信扫码后自动存账号。
 */

const API = 'https://copilot.tencent.com';

/* ------------------------------------------------------------------ */
/* 编码工具                                                            */
/* ------------------------------------------------------------------ */

const enc = new TextEncoder();
const dec = new TextDecoder();

function b64encode(buf) {
  const bytes = new Uint8Array(buf);
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
}

function b64decode(str) {
  const bin = atob(str);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function b64urlEncode(buf) {
  return b64encode(buf).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function b64urlDecode(str) {
  const s = str.replace(/-/g, '+').replace(/_/g, '/');
  return b64decode(s + '='.repeat((4 - (s.length % 4)) % 4));
}

function randomId(bytes = 12) {
  return b64urlEncode(crypto.getRandomValues(new Uint8Array(bytes)));
}

/** 定长比较，避免口令被时序侧信道猜出来 */
function timingSafeEqual(a, b) {
  const x = enc.encode(String(a ?? ''));
  const y = enc.encode(String(b ?? ''));
  if (x.length !== y.length) return false;
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}

/* ------------------------------------------------------------------ */
/* 加密：AES-256-GCM + 每条记录随机 salt，密钥由主密钥 HKDF 派生        */
/* ------------------------------------------------------------------ */

async function deriveKey(env, salt) {
  const raw = env.WB_MASTER_KEY;
  if (!raw) throw new Error('缺少 WB_MASTER_KEY');

  let master;
  try {
    master = b64decode(raw.trim());
  } catch {
    throw new Error('WB_MASTER_KEY 不是合法 base64');
  }
  if (master.length !== 32) throw new Error('WB_MASTER_KEY 必须是 32 字节（base64 后 44 字符）');

  const base = await crypto.subtle.importKey('raw', master, 'HKDF', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt, info: enc.encode('wb-signin/v1/account') },
    base,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt']
  );
}

async function sealRecord(env, obj) {
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const key = await deriveKey(env, salt);
  const ct = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv },
    key,
    enc.encode(JSON.stringify(obj))
  );
  return JSON.stringify({ v: 1, s: b64encode(salt), i: b64encode(iv), d: b64encode(ct) });
}

async function openRecord(env, text) {
  const box = JSON.parse(text);
  if (box.v !== 1) throw new Error('不认识的记录版本');
  const key = await deriveKey(env, b64decode(box.s));
  const pt = await crypto.subtle.decrypt(
    { name: 'AES-GCM', iv: b64decode(box.i) },
    key,
    b64decode(box.d)
  );
  return JSON.parse(dec.decode(pt));
}

/* ------------------------------------------------------------------ */
/* KV 读写                                                             */
/* ------------------------------------------------------------------ */

const acctKey = (id) => `acct:${id}`;
const INDEX_KEY = 'index:accounts';

async function listAccounts(env) {
  const raw = await env.WB_KV.get(INDEX_KEY);
  return raw ? JSON.parse(raw) : [];
}

async function saveIndex(env, list) {
  await env.WB_KV.put(INDEX_KEY, JSON.stringify(list));
}

async function getAccount(env, id) {
  const raw = await env.WB_KV.get(acctKey(id));
  if (!raw) return null;
  return openRecord(env, raw);
}

async function putAccount(env, acct) {
  await env.WB_KV.put(acctKey(acct.id), await sealRecord(env, acct));
}

/* ------------------------------------------------------------------ */
/* 与 WorkBuddy 服务端交互                                              */
/* ------------------------------------------------------------------ */

async function wbFetch(path, { method = 'POST', headers = {}, body = '{}', timeout = 20000 } = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeout);
  try {
    const res = await fetch(API + path, {
      method,
      headers: {
        'Content-Type': 'application/json',
        'User-Agent': 'workbuddy-signin-worker/1.0',
        ...headers,
      },
      body: method === 'GET' ? undefined : body,
      signal: ctrl.signal,
    });
    const text = await res.text();
    let json;
    try {
      json = JSON.parse(text);
    } catch {
      throw new Error(`响应不是 JSON (HTTP ${res.status}): ${text.slice(0, 120)}`);
    }
    return { status: res.status, json };
  } finally {
    clearTimeout(timer);
  }
}

/**
 * 用 refresh_token 换新的双令牌。
 * 注意：refreshToken 会轮换，旧的可能失效，所以调用方必须立刻保存新的。
 */
async function refreshTokens(refreshToken) {
  const { status, json } = await wbFetch('/v2/plugin/auth/token/refresh', {
    headers: {
      'X-Refresh-Token': refreshToken,
      'X-Auth-Refresh-Source': 'plugin',
      'X-Domain': 'copilot.tencent.com',
    },
  });

  if (json?.code !== 0 || !json?.data) {
    const err = new Error(
      `刷新令牌失败 code=${json?.code} ${json?.msg ?? ''} (HTTP ${status})`
    );
    err.code = json?.code;
    err.fatal = true; // 令牌链路断了，需要人工重新抓
    throw err;
  }

  const d = json.data;
  return {
    accessToken: d.accessToken,
    refreshToken: d.refreshToken || refreshToken,
    expiresIn: Number(d.expiresIn) || 0,
    refreshExpiresIn: Number(d.refreshExpiresIn) || 0,
  };
}

/**
 * 扫码登录第一步：向服务端申请一个登录态。
 * 返回的 authUrl 就是官方登录页（微信扫码页），state 用来轮询结果。
 */
async function requestLoginState() {
  const { status, json } = await wbFetch('/v2/plugin/auth/state?platform=workbuddy', {
    headers: {
      'X-No-Authorization': 'true',
      'X-No-User-Id': 'true',
      'X-No-Enterprise-Id': 'true',
      'X-No-Department-Info': 'true',
      'X-Domain': 'copilot.tencent.com',
    },
  });
  if (json?.code !== 0 || !json?.data?.state) {
    throw new Error(`申请登录态失败 code=${json?.code} ${json?.msg ?? ''} (HTTP ${status})`);
  }
  return { state: json.data.state, authUrl: json.data.authUrl };
}

/**
 * 扫码登录第二步：轮询这个 state 有没有被授权。
 *   code 11217 → 还在等扫码/确认
 *   code 0     → 已经拿到令牌
 */
async function pollLoginToken(state) {
  const { status, json } = await wbFetch(
    `/v2/plugin/auth/token?state=${encodeURIComponent(state)}`,
    {
      method: 'GET',
      headers: {
        'X-No-Authorization': 'true',
        'X-No-User-Id': 'true',
        'X-No-Enterprise-Id': 'true',
        'X-No-Department-Info': 'true',
        'X-Domain': 'copilot.tencent.com',
      },
    }
  );
  if (json?.code === 11217) return { pending: true };
  if (json?.code === 0 && json?.data?.accessToken) return { tokens: json.data };
  throw new Error(`轮询登录结果失败 code=${json?.code} ${json?.msg ?? ''} (HTTP ${status})`);
}

/** 尽量取一个昵称当备注名，取不到就算了，不影响登录 */
async function fetchNickname(at) {
  try {
    const { json } = await wbFetch('/v2/accounts', {
      method: 'GET',
      headers: { ...authHeader(at), 'X-Domain': 'copilot.tencent.com' },
    });
    const list = json?.data?.accounts ?? json?.data;
    const first = Array.isArray(list) ? list[0] : null;
    return String(first?.nickname ?? first?.nickName ?? '').trim();
  } catch {
    return '';
  }
}

/** 把一条令牌落成账号记录（加密存 KV + 更新索引） */
async function persistAccount(env, { refreshToken, label, accessToken }) {
  const id = randomId(9);
  let name = String(label ?? '').trim().slice(0, 40);
  if (!name && accessToken) name = await fetchNickname(accessToken);
  if (!name) name = `账号-${id.slice(0, 4)}`;

  const acct = {
    id,
    label: name,
    refreshToken,
    createdAt: Date.now(),
    lastState: 'new',
    lastMsg: '尚未运行',
    verified: true,
    tokenRefreshedAt: Date.now(),
  };
  await putAccount(env, acct);

  const list = await listAccounts(env);
  list.push({ id, label: acct.label, createdAt: acct.createdAt });
  await saveIndex(env, list);
  return acct;
}

const authHeader = (at) => ({ Authorization: `Bearer ${at}` });

async function checkinStatus(at) {
  const { json } = await wbFetch('/v2/billing/meter/checkin-activity-status', {
    headers: authHeader(at),
  });
  if (json?.code !== 0 || !json?.data) {
    return { ok: false, code: json?.code, msg: json?.msg };
  }
  return { ok: true, data: json.data };
}

async function doCheckin(at) {
  const { json } = await wbFetch('/v2/billing/meter/daily-checkin', {
    headers: authHeader(at),
  });
  if (json?.code === 0) {
    return { state: 'checked', credit: json.data?.credit ?? 100, raw: json.data };
  }
  if (json?.code === 10001) {
    return { state: 'already', credit: 0 };
  }
  return { state: 'error', code: json?.code, msg: json?.msg };
}

/* ------------------------------------------------------------------ */
/* 单个账号的完整签到流程                                               */
/* ------------------------------------------------------------------ */

async function signinOne(env, acct, { force = false } = {}) {
  const now = Date.now();
  const today = new Date(now + 8 * 3600 * 1000).toISOString().slice(0, 10); // 按北京时间算日期

  // 今天已成功过、且没要求强制，直接跳过（省一次请求，也避免无谓刷新）
  if (!force && acct.lastOkDate === today && acct.lastState !== 'error') {
    return { id: acct.id, label: acct.label, state: 'skipped', msg: '今日已完成' };
  }

  let tokens;
  try {
    tokens = await refreshTokens(acct.refreshToken);
  } catch (e) {
    acct.lastState = e.fatal ? 'token_dead' : 'error';
    acct.lastMsg = e.message;
    acct.lastRun = now;
    await putAccount(env, acct);
    return { id: acct.id, label: acct.label, state: acct.lastState, msg: e.message };
  }

  // 先落盘新令牌。哪怕后面签到失败，令牌链也不能断。
  acct.refreshToken = tokens.refreshToken;
  acct.tokenRefreshedAt = now;

  let result;
  try {
    result = await doCheckin(tokens.accessToken);
  } catch (e) {
    acct.lastState = 'error';
    acct.lastMsg = `签到请求异常: ${e.message}`;
    acct.lastRun = now;
    await putAccount(env, acct);
    return { id: acct.id, label: acct.label, state: 'error', msg: acct.lastMsg };
  }

  let streak = null;
  let total = null;
  try {
    const st = await checkinStatus(tokens.accessToken);
    if (st.ok) {
      streak = st.data.streak_days ?? null;
      total = st.data.total_credits ?? null;
      // 顺手把最新 accessToken 的服务端视图对上，用于判断是否真的已签
      if (st.data.checked_in_today === true && result.state === 'error') {
        result = { state: 'already', credit: 0 };
      }
    }
  } catch {
    /* 状态查询失败不影响主流程 */
  }

  acct.lastRun = now;
  acct.streak = streak;
  acct.total = total;

  if (result.state === 'error') {
    acct.lastState = 'error';
    acct.lastMsg = `签到失败 code=${result.code} ${result.msg ?? ''}`;
  } else {
    acct.lastState = result.state; // 'checked' | 'already'
    acct.lastMsg = result.state === 'checked' ? `+${result.credit} 积分` : '今日已签';
    acct.lastOkDate = today;
    if (result.state === 'checked') {
      acct.lastCredit = result.credit;
      acct.totalSigned = (acct.totalSigned ?? 0) + 1;
    }
  }

  await putAccount(env, acct);
  return {
    id: acct.id,
    label: acct.label,
    state: acct.lastState,
    msg: acct.lastMsg,
    streak,
    total,
  };
}

/* ------------------------------------------------------------------ */
/* 页面与鉴权辅助                                                       */
/* ------------------------------------------------------------------ */

function json(data, status = 200) {
  return new Response(JSON.stringify(data, null, 2), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' },
  });
}

function requireAdmin(request, env) {
  const url = new URL(request.url);
  const given =
    request.headers.get('X-Admin-Token') ||
    (request.headers.get('Authorization') || '').replace(/^Bearer\s+/i, '') ||
    url.searchParams.get('token');
  if (!env.ADMIN_TOKEN) return { ok: false, res: json({ error: '未配置 ADMIN_TOKEN' }, 500) };
  if (!timingSafeEqual(given, env.ADMIN_TOKEN)) {
    return { ok: false, res: json({ error: '登录密码不正确' }, 401) };
  }
  return { ok: true };
}

function requireCron(request, env) {
  const secret = env.CRON_SECRET || env.ADMIN_TOKEN;
  const url = new URL(request.url);
  const given = request.headers.get('X-Cron-Secret') || url.searchParams.get('key');
  if (!secret) return { ok: false, res: json({ error: '未配置 CRON_SECRET / ADMIN_TOKEN' }, 500) };
  if (!timingSafeEqual(given, secret)) return { ok: false, res: json({ error: '口令不对' }, 401) };
  return { ok: true };
}

async function readBody(request) {
  try {
    return await request.json();
  } catch {
    return {};
  }
}

/* ------------------------------------------------------------------ */
/* 路由                                                                */
/* ------------------------------------------------------------------ */

/** 扫码登录：申请二维码（其实是一个官方登录页地址） */
async function handleLoginStart(request, env) {
  const { state, authUrl } = await requestLoginState();
  // 记一下 state 是我们发出去的，避免有人拿别人的 state 来轮询
  await env.WB_KV.put(`login:${state}`, String(Date.now()), { expirationTtl: 900 });
  return json({ ok: true, state, authUrl });
}

/** 扫码登录：轮询授权结果，拿到令牌就顺手存成账号 */
async function handleLoginPoll(request, env) {
  const url = new URL(request.url);
  const state = String(url.searchParams.get('state') ?? '').trim();
  if (!state) return json({ error: '缺少 state' }, 400);

  const known = await env.WB_KV.get(`login:${state}`);
  if (!known) return json({ error: '登录会话已过期，请重新获取二维码' }, 400);

  const r = await pollLoginToken(state);
  if (r.pending) return json({ status: 'pending' });

  const rt = String(r.tokens.refreshToken ?? '').trim();
  if (!rt) return json({ error: '登录成功但服务端没返回 refreshToken' }, 502);

  const acct = await persistAccount(env, { refreshToken: rt, accessToken: r.tokens.accessToken });
  await env.WB_KV.delete(`login:${state}`);
  return json({ status: 'ok', id: acct.id, label: acct.label });
}

async function handleList(request, env) {
  const list = await listAccounts(env);
  const out = [];
  for (const meta of list) {
    const a = await getAccount(env, meta.id);
    if (!a) continue;
    out.push({
      id: a.id,
      label: a.label,
      lastState: a.lastState,
      lastMsg: a.lastMsg,
      lastRun: a.lastRun ? new Date(a.lastRun + 8 * 3600e3).toISOString().replace('Z', ' +08:00') : null,
      lastOkDate: a.lastOkDate ?? null,
      streak: a.streak ?? null,
      total: a.total ?? null,
      totalSigned: a.totalSigned ?? 0,
      // 永远不返回令牌，连指纹都不给
    });
  }
  return json({ count: out.length, accounts: out });
}

async function handleRemove(request, env) {
  const body = await readBody(request);
  const id = String(body.id ?? '').trim();
  if (!id) return json({ error: '缺少 id' }, 400);
  await env.WB_KV.delete(acctKey(id));
  const list = (await listAccounts(env)).filter((x) => x.id !== id);
  await saveIndex(env, list);
  return json({ ok: true, removed: id });
}

async function handleRun(request, env, force) {
  const list = await listAccounts(env);
  const results = [];
  for (const meta of list) {
    const a = await getAccount(env, meta.id);
    if (!a) continue;
    try {
      results.push(await signinOne(env, a, { force }));
    } catch (e) {
      results.push({ id: a.id, label: a.label, state: 'error', msg: e.message });
    }
  }
  const summary = {
    at: new Date(Date.now() + 8 * 3600e3).toISOString().replace('Z', ' +08:00'),
    accounts: results.length,
    checked: results.filter((r) => r.state === 'checked').length,
    already: results.filter((r) => r.state === 'already' || r.state === 'skipped').length,
    failed: results.filter((r) => r.state === 'error' || r.state === 'token_dead').length,
    results,
  };
  return json(summary);
}

/* ------------------------------------------------------------------ */
/* 控制台（单页，内嵌，不依赖任何 CDN）                                  */
/* ------------------------------------------------------------------ */

const PAGE = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>WorkBuddy 签到控制台</title>
<style>
  :root{
    --bg:#eef1f5;--glow-a:rgba(16,160,110,.11);--glow-b:rgba(56,110,220,.09);--grid:rgba(15,23,42,.035);
    --surface:#fff;--surface-2:#f6f8fa;--line:rgba(15,23,42,.09);--line-2:rgba(15,23,42,.16);
    --txt:#0c141d;--dim:#5e6a78;--pri:#0d8a61;--pri-ink:#fff;--ring:rgba(13,138,97,.16);
    --ok:#0d8a61;--ok-bg:rgba(13,138,97,.10);--warn:#9a6407;--warn-bg:rgba(190,124,15,.13);
    --bad:#bf3529;--bad-bg:rgba(191,53,41,.10);--idle:#5e6a78;--idle-bg:rgba(15,23,42,.055);
    --bar:rgba(238,241,245,.78);--sh-1:0 1px 2px rgba(15,23,42,.04),0 10px 26px -16px rgba(15,23,42,.22);
    --sh-2:0 18px 44px -22px rgba(15,23,42,.35);--r:16px;
    --mono:ui-monospace,"SF Mono",SFMono-Regular,Menlo,Consolas,monospace;
  }
  @media (prefers-color-scheme:dark){
    :root{
      --bg:#070a0e;--glow-a:rgba(45,208,126,.16);--glow-b:rgba(70,110,255,.13);--grid:rgba(255,255,255,.035);
      --surface:#101822;--surface-2:#0c131b;--line:rgba(255,255,255,.075);--line-2:rgba(255,255,255,.14);
      --txt:#e8eef5;--dim:#8b98a7;--pri:#2fd07e;--pri-ink:#04150d;--ring:rgba(47,208,126,.20);
      --ok:#3ddc97;--ok-bg:rgba(61,220,151,.13);--warn:#e6ae4a;--warn-bg:rgba(230,174,74,.13);
      --bad:#ff7d6e;--bad-bg:rgba(255,125,110,.13);--idle:#8b98a7;--idle-bg:rgba(255,255,255,.06);
      --bar:rgba(7,10,14,.72);--sh-1:0 1px 0 rgba(255,255,255,.03),0 14px 34px -22px rgba(0,0,0,.95);
      --sh-2:0 26px 60px -30px rgba(0,0,0,1);
    }
  }
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--txt);-webkit-font-smoothing:antialiased;
       font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif}
  body::before{content:"";position:fixed;inset:0;pointer-events:none;z-index:0;
    background:radial-gradient(58% 46% at 8% -6%,var(--glow-a),transparent 62%),
               radial-gradient(46% 42% at 96% 2%,var(--glow-b),transparent 62%)}
  body::after{content:"";position:fixed;inset:0;pointer-events:none;z-index:0;background-image:
      linear-gradient(var(--grid) 1px,transparent 1px),linear-gradient(90deg,var(--grid) 1px,transparent 1px);
    background-size:46px 46px;
    -webkit-mask-image:radial-gradient(72% 58% at 50% 0%,#000,transparent 78%);
            mask-image:radial-gradient(72% 58% at 50% 0%,#000,transparent 78%)}
  .wrap{position:relative;z-index:1;max-width:1920px;margin:0 auto;
        padding:clamp(16px,2.6vw,36px) clamp(16px,3vw,44px) 68px}

  .brand{display:flex;align-items:center;gap:13px}
  .mark{width:44px;height:44px;flex:0 0 auto;border-radius:14px;display:grid;place-items:center;
        background:linear-gradient(155deg,var(--pri),#0a6a4a);
        box-shadow:var(--sh-2),inset 0 1px 0 rgba(255,255,255,.3)}
  .mark svg{width:21px;height:21px;fill:none;stroke:#fff;stroke-width:2.6;stroke-linecap:round;stroke-linejoin:round}
  .eyebrow{font-family:var(--mono);font-size:10px;letter-spacing:.18em;text-transform:uppercase;color:var(--dim)}
  h1{font-size:clamp(19px,2.6vw,23px);margin:1px 0 0;font-weight:700;letter-spacing:-.02em;line-height:1.25}

  #loginView{min-height:min(74dvh,640px);display:grid;place-content:center;gap:26px}
  .login-card{width:min(470px,92vw);padding:26px;background:var(--surface);border:1px solid var(--line);
              border-radius:var(--r);box-shadow:var(--sh-1)}
  .lbl{display:block;font-family:var(--mono);font-size:10.5px;letter-spacing:.14em;
       text-transform:uppercase;color:var(--dim);margin-bottom:9px}
  .field{display:flex;gap:10px}
  .field input{flex:1;min-width:0}

  input{font:inherit;padding:12px 14px;border:1px solid var(--line-2);border-radius:12px;background:var(--surface);
        color:var(--txt);outline:none;transition:border-color .18s,box-shadow .18s}
  input::placeholder{color:var(--dim)}
  input:focus{border-color:var(--pri);box-shadow:0 0 0 4px var(--ring)}
  button{font:inherit;font-weight:600;display:inline-flex;align-items:center;justify-content:center;gap:8px;
         padding:11px 18px;border-radius:12px;border:1px solid var(--line-2);background:var(--surface);color:var(--txt);
         cursor:pointer;transition:transform .12s ease,box-shadow .18s ease,border-color .18s ease,filter .18s ease}
  button:hover{border-color:var(--txt);transform:translateY(-1px);box-shadow:var(--sh-1)}
  button:active{transform:none}
  button:focus-visible{outline:2px solid var(--pri);outline-offset:2px}
  button.pri{background:var(--pri);border-color:transparent;color:var(--pri-ink);
             box-shadow:inset 0 1px 0 rgba(255,255,255,.22),0 12px 26px -14px var(--pri)}
  button.pri:hover{border-color:transparent;filter:brightness(1.06)}
  button:disabled{opacity:.5;cursor:not-allowed;transform:none;box-shadow:none}
  .row{display:flex;gap:10px;flex-wrap:wrap}

  .topbar{position:sticky;top:0;z-index:4;display:flex;align-items:center;justify-content:space-between;
          gap:14px;flex-wrap:wrap;padding:12px 0 14px;margin-bottom:18px;background:var(--bar);
          backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);border-bottom:1px solid var(--line)}
  .chip{display:inline-flex;align-items:center;gap:8px;padding:7px 13px;border-radius:999px;border:1px solid var(--line);
        background:var(--surface);font-size:12.5px;color:var(--dim);white-space:nowrap;box-shadow:var(--sh-1)}
  .chip b{color:var(--txt);font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:12px}
  .chip .dot{width:7px;height:7px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 3px var(--ok-bg)}

  .grid{display:grid;gap:18px;grid-template-columns:minmax(0,1fr) clamp(420px,34vw,600px);align-items:start}
  .col{display:grid;gap:18px;min-width:0}
  .card{background:var(--surface);border:1px solid var(--line);border-radius:var(--r);padding:20px;box-shadow:var(--sh-1)}
  .card-head{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:14px}
  .card-head h2{margin:0;font-family:var(--mono);font-size:10.5px;letter-spacing:.16em;
                text-transform:uppercase;color:var(--dim);font-weight:600}
  .actions{display:flex;flex-wrap:wrap;align-items:center;gap:10px}
  .actions .err{flex:1 1 100%;margin-top:2px}

  #list table{width:100%;border-collapse:separate;border-spacing:0;font-size:13.5px}
  #list th{text-align:left;padding:0 10px 9px;border-bottom:1px solid var(--line);color:var(--dim);
           font-family:var(--mono);font-size:10px;letter-spacing:.12em;text-transform:uppercase;
           font-weight:600;white-space:nowrap}
  #list td{padding:11px 10px;border-bottom:1px solid var(--line);vertical-align:middle}
  #list tbody tr{transition:background .16s ease}
  #list tbody tr:hover{background:var(--surface-2)}
  #list tbody tr:last-child td{border-bottom:0}
  #list td:first-child{font-weight:600}
  #list td:nth-child(3),#list td:nth-child(4){font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:13px}
  #list td:nth-child(5){font-family:var(--mono);font-size:11.5px;color:var(--dim)}
  #list table button{padding:6px 12px;border-radius:10px;font-size:12.5px;box-shadow:none}
  #list table button:hover{color:var(--bad);border-color:var(--bad);background:var(--bad-bg)}

  .tag{display:inline-flex;align-items:center;gap:6px;padding:3px 10px;border-radius:999px;
       font-size:12px;font-weight:600;white-space:nowrap}
  .tag::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;flex:0 0 auto}
  .t-ok{background:var(--ok-bg);color:var(--ok)}
  .t-warn{background:var(--warn-bg);color:var(--warn)}
  .t-bad{background:var(--bad-bg);color:var(--bad)}
  .t-idle{background:var(--idle-bg);color:var(--idle)}
  .hint{color:var(--dim);font-size:12.5px;margin-top:9px}
  code{font-family:var(--mono);font-size:11.5px;background:var(--idle-bg);color:var(--txt);padding:2px 6px;border-radius:6px}
  .empty{color:var(--dim);text-align:center;padding:26px 16px;font-size:13px}
  .err{color:var(--bad);font-size:13px;margin-top:10px;white-space:pre-wrap}

  .qr-frame{margin-top:14px;padding:8px;border:1px solid var(--line);border-radius:14px;
            background:var(--surface-2);box-shadow:var(--sh-1)}
  #qrframe{display:block;width:100%;height:clamp(560px,72vh,860px);border:0;border-radius:10px;background:#fff}

  @keyframes rise{from{opacity:0;transform:translateY(12px)}to{opacity:1;transform:none}}
  .card,.login-card,.topbar{animation:rise .55s cubic-bezier(.22,.9,.28,1) both}
  .col>.card:nth-child(2){animation-delay:.06s}
  #mainView .col:nth-child(2)>.card{animation-delay:.12s}
  @media (prefers-reduced-motion:reduce){*{animation:none!important;transition:none!important}}

  @media (max-width:1080px){
    .grid{grid-template-columns:minmax(0,1fr)}
    .topbar{position:static}
    #qrframe{max-width:560px;margin:0 auto;height:clamp(480px,70vh,680px)}
  }
  @media (max-width:640px){
    #loginView{min-height:auto;padding:36px 0}
    #list table,#list tbody,#list tr,#list td{display:block}
    #list thead{display:none}
    #list tbody{display:grid;gap:10px}
    #list tr{padding:12px 14px;border:1px solid var(--line);border-radius:14px;background:var(--surface-2)}
    #list td{display:flex;align-items:center;justify-content:space-between;gap:14px;
             padding:5px 0;border:0;text-align:right}
    #list td::before{content:attr(data-label);flex:0 0 auto;text-align:left;font-family:var(--mono);
             font-size:10px;letter-spacing:.12em;text-transform:uppercase;color:var(--dim)}
    #list td:first-child{font-size:15px;padding-bottom:9px;margin-bottom:5px;border-bottom:1px solid var(--line)}
    #list td:first-child::before{display:none}
    #list td.st{flex-wrap:wrap;justify-content:flex-end}
    #list td.st>div{flex:1 1 100%;text-align:right;margin-top:4px}
    #list td:last-child{padding-top:9px}
  }
</style>
</head>
<body>
<div class="wrap">
  <div id="loginView">
    <div class="brand">
      <span class="mark"><svg viewBox="0 0 24 24"><path d="M4.5 12.6l4.6 4.6L19.5 6.8"/></svg></span>
      <div>
        <div class="eyebrow">WorkBuddy · Daily</div>
        <h1>签到控制台</h1>
      </div>
    </div>

    <div class="login-card">
      <label class="lbl" for="pwd">登录密码</label>
      <div class="field">
        <input id="pwd" type="password" placeholder="输入登录密码" autocomplete="current-password">
        <button id="loginbtn" class="pri">登录</button>
      </div>
      <div class="hint">密码即后台的 <code>ADMIN_TOKEN</code>。登录状态存在本机浏览器，公共电脑用完请点「退出登录」。</div>
      <div class="err" id="loginErr"></div>
    </div>
  </div>

  <div id="mainView" style="display:none">
    <header class="topbar">
      <div class="brand">
        <span class="mark"><svg viewBox="0 0 24 24"><path d="M4.5 12.6l4.6 4.6L19.5 6.8"/></svg></span>
        <div>
          <div class="eyebrow">WorkBuddy · Daily</div>
          <h1>签到控制台</h1>
        </div>
      </div>
      <div class="row">
        <span class="chip"><i class="dot"></i>最近一次签到 <b id="lastRun">—</b></span>
        <button id="logout">退出登录</button>
      </div>
    </header>

    <div class="grid">
      <div class="col">
        <div class="card actions">
          <button id="run" class="pri">立即签到</button>
          <button id="refresh">刷新</button>
          <div class="err" id="err"></div>
        </div>

        <div class="card">
          <div class="card-head"><h2>账号</h2></div>
          <div id="list"><div class="empty">加载中…</div></div>
        </div>
      </div>

      <div class="col">
        <div class="card">
          <div class="card-head"><h2>扫码登录</h2></div>
          <div class="row">
            <button id="qrbtn" class="pri">获取登录二维码</button>
            <button id="qrlink" style="display:none">在新窗口打开登录页</button>
          </div>
          <div id="qrbox" class="qr-frame" style="display:none">
            <div id="qrstatus" class="hint" style="margin:0 0 10px">等待扫码…</div>
            <iframe id="qrframe" title="微信扫码登录" sandbox="allow-scripts allow-same-origin allow-forms allow-popups"></iframe>
          </div>
          <div class="hint">用微信扫这里的码，手机上点确认，账号会自动保存。二维码由官方登录页渲染，本服务不接触你的微信信息。</div>
        </div>
      </div>
    </div>
  </div>

</div>
<script>
const $ = (s) => document.querySelector(s);
const PWD_KEY = 'wb-admin-pwd';
const state = { tok: '' };

function esc(s){return String(s??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}

/** 按北京时间取当天日期串 YYYY-MM-DD（与 Worker 端的 lastOkDate 口径一致） */
function bjToday(){
  return new Date(Date.now() + 8 * 3600e3).toISOString().slice(0, 10);
}

/**
 * 状态一律现算，不再信任 lastState 这个历史快照。
 * lastState 只是「上一次运行的结果」，跨天不会自己失效，所以不能直接拿来渲染。
 * 判断依据只剩两个当天事实：
 *   lastOkDate 是否等于今天、lastRun 是否落在今天（lastRun 已是 +08:00 格式串，取前 10 位即可）。
 */
function tagOf(a){
  const today = bjToday();
  const ranToday = !!a.lastRun && a.lastRun.slice(0, 10) === today;
  if (a.lastOkDate === today) return '<span class="tag t-ok">已签</span>';
  if (a.lastState === 'token_dead') return '<span class="tag t-bad">令牌失效</span>';
  if (a.lastState === 'error' && ranToday) return '<span class="tag t-bad">签到失败</span>';
  return '<span class="tag t-idle">今日未签</span>';
}

/** 同上：只有属于今天的、仍有信息量的小字才显示，避免昨天的文案挂到今天头上 */
function msgOf(a){
  const today = bjToday();
  const ranToday = !!a.lastRun && a.lastRun.slice(0, 10) === today;
  if (a.lastOkDate === today) return a.lastState === 'checked' ? (a.lastMsg || '') : '';
  if (a.lastState === 'token_dead') return a.lastMsg || '';
  if (a.lastState === 'error' && ranToday) return a.lastMsg || '';
  return '';
}

async function api(path, opts={}){
  const res = await fetch(path, {
    ...opts,
    headers: { 'Content-Type':'application/json', 'X-Admin-Token': state.tok, ...(opts.headers||{}) }
  });
  const data = await res.json().catch(()=>({error:'响应无法解析'}));
  if(res.status === 401){
    const msg = data.error || '登录已失效，请重新登录';
    logout(msg);
    throw new Error(msg);
  }
  if(!res.ok) throw new Error(data.error || ('HTTP '+res.status));
  return data;
}

/** 用登录密码换一次账号列表，成功即说明密码正确 */
async function probe(password){
  const res = await fetch('/api/list', {
    headers: { 'Content-Type':'application/json', 'X-Admin-Token': password }
  });
  const data = await res.json().catch(()=>({error:'响应无法解析'}));
  if(!res.ok) throw new Error(data.error || ('HTTP '+res.status));
  return data;
}

function setLoginErr(m){ $('#loginErr').textContent = m ? String(m) : ''; }
function showErr(e){ $('#err').textContent = e ? String(e.message||e) : ''; }

/** 登录：验证密码 → 记进本次浏览器会话 → 自动载入账号列表 */
async function login(password){
  const v = String(password ?? '').trim();
  if(!v){ setLoginErr('请输入登录密码'); return; }
  const btn = $('#loginbtn');
  btn.disabled = true; btn.textContent = '登录中…';
  try{
    const d = await probe(v);
    state.tok = v;
    localStorage.setItem(PWD_KEY, v);
    setLoginErr('');
    renderList(d);
    $('#loginView').style.display = 'none';
    $('#mainView').style.display = '';
  }catch(e){
    setLoginErr(e.message || e);
  }finally{
    btn.disabled = false; btn.textContent = '登录';
  }
}

function logout(msg){
  stopQr();
  state.tok = '';
  localStorage.removeItem(PWD_KEY);
  $('#pwd').value = '';
  $('#list').innerHTML = '<div class="empty">加载中…</div>';
  showErr('');
  setLoginErr(msg || '');
  $('#mainView').style.display = 'none';
  $('#loginView').style.display = '';
}

/** 顶栏显示最近一次签到时间（取所有账号里最新的一次运行） */
function renderLastRun(list){
  const times = list.map(a=>a.lastRun).filter(Boolean).sort();
  $('#lastRun').textContent = times.length
    ? times[times.length-1].replace('T',' ').slice(0,16)
    : (list.length ? '暂无记录' : '暂无账号');
}

function renderList(d){
  const box = $('#list');
  renderLastRun(d.accounts);
  if(!d.accounts.length){ box.innerHTML = '<div class="empty">还没有账号</div>'; return; }
  box.innerHTML = '<table><thead><tr><th>备注</th><th>状态</th><th>连签</th><th>本期</th><th>上次运行</th><th></th></tr></thead><tbody>'
    + d.accounts.map(a=>{
        const m = msgOf(a);
        return '<tr>'
          + '<td data-label="备注">'+esc(a.label)+'</td>'
          + '<td class="st" data-label="状态">'+tagOf(a)+(m?'<div class="hint" style="margin:2px 0 0">'+esc(m)+'</div>':'')+'</td>'
          + '<td data-label="连签">'+(a.streak==null?'—':a.streak+' 天')+'</td>'
          + '<td data-label="本期">'+(a.total==null?'—':a.total)+'</td>'
          + '<td data-label="上次运行">'+(a.lastRun? esc(a.lastRun.replace('T',' ').slice(0,16)) : '—')+'</td>'
          + '<td data-label="操作"><button data-del="'+esc(a.id)+'">删除</button></td>'
          + '</tr>';
      }).join('') + '</tbody></table>';
  box.querySelectorAll('[data-del]').forEach(b=>{
    b.onclick = async ()=>{
      if(!confirm('删除这个账号？')) return;
      try{ await api('/api/remove',{method:'POST',body:JSON.stringify({id:b.dataset.del})}); await load(); }
      catch(e){ showErr(e); }
    };
  });
}

async function load(){
  try{
    showErr('');
    renderList(await api('/api/list'));
  }catch(e){ showErr(e); }
}

$('#loginbtn').onclick = ()=> login($('#pwd').value);
$('#pwd').onkeydown = (e)=>{ if(e.key === 'Enter') login($('#pwd').value); };
$('#logout').onclick = ()=>{ if(confirm('退出登录？')) logout(''); };
$('#refresh').onclick = load;

$('#run').onclick = async ()=>{
  const btn = $('#run'); btn.disabled = true; btn.textContent = '签到中…';
  try{
    showErr('');
    const d = await api('/api/run?force=1',{method:'POST',body:'{}'});
    const bad = d.results.filter(r=>r.state==='error'||r.state==='token_dead');
    if(bad.length) showErr(bad.map(r=>r.label+': '+r.msg).join('\\n'));
    await load();
  }catch(e){ showErr(e); }
  finally{ btn.disabled=false; btn.textContent='立即签到'; }
};

let qrTimer = null, qrState = '';
function stopQr(){ if(qrTimer){ clearInterval(qrTimer); qrTimer = null; } }

$('#qrbtn').onclick = async ()=>{
  stopQr();
  const btn = $('#qrbtn');
  btn.disabled = true; btn.textContent = '获取中…';
  try{
    showErr('');
    const d = await api('/api/login/start',{method:'POST',body:'{}'});
    qrState = d.state;
    const fr = $('#qrframe');
    fr.style.display = 'block';
    fr.src = d.authUrl;
    $('#qrbox').style.display = 'block';
    const lk = $('#qrlink');
    lk.style.display = 'inline-block';
    lk.onclick = ()=>{ window.open(d.authUrl,'wb-login','width=640,height=880'); };
    $('#qrstatus').textContent = '等待扫码…（有效期约 5 分钟）';
    btn.textContent = '重新获取二维码';
    btn.disabled = false;

    let n = 0;
    qrTimer = setInterval(async ()=>{
      if(n++ > 150){
        stopQr();
        $('#qrstatus').textContent = '二维码已过期，请重新获取';
        return;
      }
      try{
        const r = await api('/api/login/poll?state=' + encodeURIComponent(qrState));
        if(r.status === 'ok'){
          stopQr();
          fr.style.display = 'none';
          fr.src = 'about:blank';
          $('#qrstatus').textContent = '登录成功，已保存为「' + r.label + '」';
          await load();
        }
      }catch(e){
        stopQr();
        $('#qrstatus').textContent = String(e.message || e);
      }
    }, 2500);
  }catch(e){
    showErr(e);
    btn.textContent = '获取登录二维码';
    btn.disabled = false;
  }
};

/** 页面挂着过夜时自动翻篇：只在日期真的变了才重新拉一次列表 */
let dayMark = bjToday();
setInterval(()=>{
  const d = bjToday();
  if(d !== dayMark){
    dayMark = d;
    if(state.tok) load();
  }
}, 60000);

// 本机登录过就直接进控制台，省掉每次都要输密码
const saved = localStorage.getItem(PWD_KEY);
if(saved) login(saved);
</script>
</body>
</html>`;

/* ------------------------------------------------------------------ */
/* 入口                                                                */
/* ------------------------------------------------------------------ */

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, '') || '/';
    const method = request.method.toUpperCase();

    try {
      if (path === '/' || path === '/index.html') {
        return new Response(PAGE, {
          headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' },
        });
      }

      // 定时任务入口：口令走 header/query，不需要登录密码
      if (path === '/cron') {
        const c = requireCron(request, env);
        if (!c.ok) return c.res;
        return handleRun(request, env, false);
      }

      if (!path.startsWith('/api/')) return json({ error: 'not found' }, 404);

      const a = requireAdmin(request, env);
      if (!a.ok) return a.res;

      if (path === '/api/list' && method === 'GET') return handleList(request, env);
      if (path === '/api/login/start' && method === 'POST') return handleLoginStart(request, env);
      if (path === '/api/login/poll' && method === 'GET') return handleLoginPoll(request, env);
      if (path === '/api/remove' && method === 'POST') return handleRemove(request, env);
      if (path === '/api/run' && method === 'POST') {
        const force = url.searchParams.get('force') === '1';
        return handleRun(request, env, force);
      }

      return json({ error: 'not found' }, 404);
    } catch (e) {
      return json({ error: e.message || String(e) }, 500);
    }
  },

  /** Cloudflare Cron Trigger 到点自动跑 */
  async scheduled(event, env, ctx) {
    ctx.waitUntil(
      (async () => {
        const list = await listAccounts(env);
        for (const meta of list) {
          const acct = await getAccount(env, meta.id);
          if (!acct) continue;
          try {
            await signinOne(env, acct, { force: false });
          } catch {
            /* 单账号失败不影响其他账号 */
          }
        }
      })()
    );
  },
};
