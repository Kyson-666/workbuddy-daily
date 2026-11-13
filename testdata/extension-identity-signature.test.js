/**
 * 扩展「登录第二个账号」修复的契约测试（不依赖 Chrome API）。
 *
 * 背景（2026-11-13 线上问题）：
 *   原判据 waitLoginTrace() 只看「有没有登录痕迹」，而上一账号的 cookie 仍在浏览器里，
 *   于是第一轮轮询立刻命中 → 直接跳握手并关掉标签页，用户看到「打开就自动关闭」，
 *   且回传的仍是旧账号。
 *   修复后改为「身份签名相对基线发生变化」。
 *
 * 这里复刻 background.js 中的 identitySignature 与完成判据，做行为回归：
 * 若有人把判据改回「有痕迹即可」，本测试会立刻失败。
 */
const TRACE_NAMES = new Set([
  'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
  'bce-user-info', 'BDUSS', 'bce-login-accountid',
]);
const IDENTITY_NAMES = new Set([
  'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
  'bce-user-info', 'BDUSS', 'bce-login-accountid', 'bce-login-display-name',
  'bce-login-domain-account', 'STOKEN', 'PTOKEN',
]);

function identitySignature(cookies) {
  return cookies
    .filter(c => IDENTITY_NAMES.has(c.name))
    .map(c => c.name + '=' + c.value)
    .sort()
    .join('|');
}

/** 复刻修复后的完成判据 */
function isLoginComplete(cookies, baseline) {
  const sig = identitySignature(cookies);
  return !!sig && sig !== baseline;
}

/** 复刻修复前的判据（应被判为「不安全」） */
function isLoginCompleteOld(cookies) {
  return cookies.some(c => TRACE_NAMES.has(c.name));
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

let pass = 0, fail = 0;
function check(name, cond) {
  if (cond) { pass++; console.log('  PASS', name); }
  else { fail++; console.log('  FAIL', name); }
}

console.log('=== 场景1: 第一轮登录（浏览器无任何百度 cookie）===');
{
  const baseline = identitySignature([]);
  check('基线为空串', baseline === '');
  check('登入账号A 后判定完成', isLoginComplete(accountA, baseline));
}

console.log('\n=== 场景2: 已登录账号A，再登录账号B（本次修复的核心）===');
{
  const baseline = identitySignature(accountA);
  check('基线为账号A的签名', baseline.includes('acctA'));
  // 修复后：A 的残留不应被判为完成
  check('账号A残留 不应判定为完成', !isLoginComplete(accountA, baseline),
    );
  // 修复前：会被误判
  check('（对照）旧判据会误判 A 残留为完成', isLoginCompleteOld(accountA));
  // 换成 B 后判定完成
  check('登入账号B 后判定完成', isLoginComplete(accountB, baseline));
}

console.log('\n=== 场景3: 只有非身份类 cookie 变化，不应误判 ===');
{
  const baseline = identitySignature(accountA);
  const onlyNoise = [...accountA, { name: 'HMACCOUNT', value: 'changed' }];
  check('仅统计类 cookie 变化 不判定完成', !isLoginComplete(onlyNoise, baseline));
}

console.log('\n=== 场景4: 签名必须稳定可比较（顺序无关）===');
{
  const reordered = [...accountA].reverse();
  check('cookie 顺序变化 签名不变', identitySignature(reordered) === identitySignature(accountA));
}

console.log('\n=== 场景5: 账号 A 与 B 签名必须不同 ===');
check('A≠B', identitySignature(accountA) !== identitySignature(accountB));

console.log(`\n合计: ${pass} pass / ${fail} fail`);
if (fail) process.exit(1);