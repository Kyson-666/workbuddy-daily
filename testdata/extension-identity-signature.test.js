/**
 * 扩展「登录判据」的契约测试（不依赖 Chrome API）。
 *
 * 与扩展共享同一份实现：require('../extension/identity.js')，
 * 即 background.js 通过 importScripts 加载的那一个文件。
 * 因此本测试守护的是**实现代码**，而不是语义副本：
 * 改 IDENTITY_NAMES、改签名算法、改完成判据的时间语义，这里都会跟着动。
 *
 * 另有一道结构性守护（场景11）：若有人把判据又内联回 background.js、
 * 让它与本模块脱钩，测试直接失败。
 *
 * 背景（2026-11-13 线上问题）：原判据只看「有没有登录痕迹」，
 * 已登录账号 A 时再添加 B，第一轮轮询立刻命中 A 的残留 cookie，
 * 于是直接进握手并关掉标签页，回传的仍是 A。
 */
const path = require('path');
const fs = require('fs');

const EXT_DIR = path.join(__dirname, '..', 'extension');
const { IDENTITY_NAMES, identitySignature, createLoginDetector } = require(path.join(EXT_DIR, 'identity.js'));

const STABLE_MS = 2000;
const POLL_MS = 1500;

let pass = 0, fail = 0;
function check(name, cond) {
  if (cond) { pass++; console.log('  PASS', name); }
  else { fail++; console.log('  FAIL', name); }
}

const accountA = [
  { name: 'BDUSS', value: 'AAA' },
  { name: 'bce-session', value: 'sessA' },
  { name: 'bce-sessionid', value: 'sidA' },
  { name: 'bce-login-accountid', value: 'acctA' },
  { name: 'HMACCOUNT', value: 'h1' },
];
const accountB = [
  { name: 'BDUSS', value: 'BBB' },
  { name: 'bce-session', value: 'sessB' },
  { name: 'bce-sessionid', value: 'sidB' },
  { name: 'bce-login-accountid', value: 'acctB' },
  { name: 'HMACCOUNT', value: 'h2' },
];
/** 登录过程中的中间态：只换了一部分身份 cookie */
const intermediate = [
  { name: 'BDUSS', value: 'BBB' },
  { name: 'bce-session', value: 'sessA' },
  { name: 'bce-sessionid', value: 'sidA' },
  { name: 'bce-login-accountid', value: 'acctA' },
];

/**
 * 造一个「连续时钟」的采样器：调用之间时间不重置，模拟真实的 setTimeout 轮询。
 * 注意 POLL_MS(1500) < STABLE_MS(2000)，所以两次相同采样不足以满足稳定要求，
 * 实际需要 3 次（跨度 3000ms 才 ≥ 2000ms）。
 */
function makeFeeder(detector) {
  let t = 0;
  return function (...samples) {
    for (let i = 0; i < samples.length; i++) {
      if (detector.feed(identitySignature(samples[i]), t)) return i;
      t += POLL_MS;
    }
    return -1;
  };
}

console.log('=== 场景1: 共享模块导出完整 ===');
{
  check('IDENTITY_NAMES 是 Set', IDENTITY_NAMES instanceof Set);
  check('identitySignature 是函数', typeof identitySignature === 'function');
  check('createLoginDetector 是函数', typeof createLoginDetector === 'function');
}

console.log('\n=== 场景2: 身份 cookie 清单守护（误删会退化成「有痕迹即完成」）===');
{
  for (const n of ['bce-session', 'bce-sessionid', 'BDUSS', 'bce-login-accountid']) {
    check('清单包含 ' + n, IDENTITY_NAMES.has(n));
  }
  // 非身份类 cookie 不应进入签名，否则统计类抖动会被误判为换账号
  check('签名不含非身份类 cookie(HMACCOUNT)', !identitySignature(accountA).includes('HMACCOUNT'));
}

console.log('\n=== 场景3: 首登（浏览器无任何百度 cookie）===');
{
  const baseline = identitySignature([]);
  check('空 cookie 的基线为空串', baseline === '');
  const d = createLoginDetector(baseline, STABLE_MS);
  check('首个采样不立即判定', d.feed(identitySignature(accountA), 0) === false);
  check('稳定 2s 后判定完成', d.feed(identitySignature(accountA), STABLE_MS) === true);
}

console.log('\n=== 场景4: 已登录 A，再登录 B（本次修复的核心）===');
{
  const baseline = identitySignature(accountA);
  check('基线为账号A的签名', baseline.includes('acctA'));
  const feed = makeFeeder(createLoginDetector(baseline, STABLE_MS));
  check('A 的残留再多轮采样也不判完成', feed(accountA, accountA, accountA, accountA) === -1);
  check('换成 B：第 1 次采样不判完成', feed(accountB) === -1);
  check('换成 B：第 2 次采样跨度仅 1.5s，仍不判完成', feed(accountB) === -1);
  check('换成 B：第 3 次采样跨度到 3s，判完成', feed(accountB) === 0);
}

console.log('\n=== 场景5: 过渡态不得提前判定 ===');
{
  const baseline = identitySignature(accountA);
  const d = createLoginDetector(baseline, STABLE_MS);
  // 中间态出现 → 换回 A → 再出现中间态：每次变化都重置计时
  check('中间态只出现一轮不判定', d.feed(identitySignature(intermediate), 0) === false);
  check('回到基线后计时重置', d.feed(identitySignature(accountA), POLL_MS) === false);
  check('中间态重现但不足 2s 不判定', d.feed(identitySignature(intermediate), POLL_MS * 2) === false);
  check('中间态持续满 2s 才判定', d.feed(identitySignature(intermediate), POLL_MS * 2 + STABLE_MS) === true);
}

console.log('\n=== 场景6: 身份 cookie 被清空不得判为登录成功 ===');
{
  const baseline = identitySignature(accountA);
  const feed = makeFeeder(createLoginDetector(baseline, STABLE_MS));
  const empty = [{ name: 'HMACCOUNT', value: 'h9' }]; // 只剩非身份类
  check('签名变空不判定完成', feed(empty, empty, empty) === -1);
}

console.log('\n=== 场景7: 仅非身份类 cookie 变化不误判 ===');
{
  const baseline = identitySignature(accountA);
  const feed = makeFeeder(createLoginDetector(baseline, STABLE_MS));
  const noise = [...accountA, { name: 'HMACCOUNT', value: 'changed' }];
  check('统计类 cookie 抖动不判定完成', feed(noise, noise, noise) === -1);
}

console.log('\n=== 场景8: 签名顺序无关 ===');
{
  const reordered = [...accountA].reverse();
  check('cookie 顺序变化 签名不变', identitySignature(reordered) === identitySignature(accountA));
}

console.log('\n=== 场景9: 稳定性边界 ===');
{
  const baseline = '';
  const d1 = createLoginDetector(baseline, STABLE_MS);
  d1.feed(identitySignature(accountB), 0);
  check('差 1ms 不判定', d1.feed(identitySignature(accountB), STABLE_MS - 1) === false);
  const d2 = createLoginDetector(baseline, STABLE_MS);
  d2.feed(identitySignature(accountB), 0);
  check('刚好 stableMs 判定', d2.feed(identitySignature(accountB), STABLE_MS) === true);
}

console.log('\n=== 场景10: 对照旧判据（仅作反例，非发布代码）===');
{
  // v1.0.2 及更早的判据，保留在这里是为了说明「为什么必须比对基线」
  const TRACE_NAMES_OLD = new Set([
    'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
    'bce-user-info', 'BDUSS', 'bce-login-accountid',
  ]);
  const isCompleteOld = cs => cs.some(c => TRACE_NAMES_OLD.has(c.name));
  check('旧判据会把 A 的残留误判为完成', isCompleteOld(accountA));
  const feed = makeFeeder(createLoginDetector(identitySignature(accountA), STABLE_MS));
  check('新判据不会误判', feed(accountA, accountA, accountA) === -1);
}

console.log('\n=== 场景11: 结构性守护 —— 判据必须来自共享模块 ===');
{
  const bg = fs.readFileSync(path.join(EXT_DIR, 'background.js'), 'utf8');
  check('background.js 通过 importScripts 加载 identity.js',
    /importScripts\(\s*['"]identity\.js['"]\s*\)/.test(bg));
  check('background.js 从 self.DumateIdentity 取判据',
    /self\.DumateIdentity/.test(bg));
  check('background.js 未内联 identitySignature 实现',
    !/function\s+identitySignature\s*\(/.test(bg));
  check('background.js 未内联 IDENTITY_NAMES 清单',
    !/(const|let|var)\s+IDENTITY_NAMES\s*=/.test(bg));
  const id = fs.readFileSync(path.join(EXT_DIR, 'identity.js'), 'utf8');
  check('identity.js 用 IIFE 包裹（importScripts 注入全局时不泄漏顶层声明）',
    /\(function\s*\(root/.test(id));
}

console.log(`\n合计: ${pass} pass / ${fail} fail`);
if (fail) process.exit(1);
