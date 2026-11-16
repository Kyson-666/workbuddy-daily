/**
 * DuMate 登录身份判据 —— **扩展与契约测试共享的唯一实现**。
 *
 * 这里放的是「怎么判断本次登录真的换了一个账号」的全部判据：
 *   - IDENTITY_NAMES    身份类 cookie 清单（换账号时必然变化的那些）
 *   - identitySignature 把身份类 cookie 拼成可比较的签名
 *   - createLoginDetector 采样状态机：签名相对基线变化、且连续稳定若干毫秒才算完成
 *
 * 背景（2026-11-13 线上问题）：原判据只看「有没有登录痕迹」，已登录账号 A 时再添加 B，
 * 第一轮轮询就命中 A 的残留 cookie，直接进握手并关掉标签页，回传的仍是 A。
 *
 * 为什么要单独成文件：判据原先内联在 background.js 里，而 background.js 依赖 chrome.*
 * API，Node 里 require 不了，测试只能「复刻」一份 —— 实现改了、复刻没改，测试照样全绿。
 * 抽出来之后 background.js 与 testdata/extension-identity-signature.test.js 引用同一份代码，
 * 测试守护的是**实现**而非语义副本。
 *
 * 加载方式（两种，同一份文件）：
 *   - 扩展：manifest 未声明 "type": "module"，service worker 是 classic，
 *     由 background.js 顶层 `importScripts('identity.js')` 注入 `self.DumateIdentity`
 *   - 测试：`require('../extension/identity.js')`
 *
 * 约束：本文件**不允许有顶层声明**（全部包在 IIFE 内）。classic worker 里 importScripts
 * 是把脚本注入全局作用域，顶层 const 会与调用方的同名声明冲突。
 */
(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module && module.exports) {
    module.exports = api; // Node：契约测试
  } else {
    root.DumateIdentity = api; // classic service worker：self.DumateIdentity
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  /**
   * 身份类 cookie：换账号时这些值必然变化。
   * 仅用于签名比对，不参与「有没有登录痕迹」的判定。
   */
  const IDENTITY_NAMES = new Set([
    'bce-session', 'bce-sessionid', 'bce-long-term-sessionid',
    'bce-user-info', 'BDUSS', 'bce-login-accountid', 'bce-login-display-name',
    'bce-login-domain-account', 'STOKEN', 'PTOKEN',
  ]);

  /**
   * 身份签名：只取身份类 cookie，拼成顺序无关的可比较字符串。
   * 空数组 → 空串（表示「当前浏览器没有任何百度登录态」）。
   */
  function identitySignature(cookies) {
    return (cookies || [])
      .filter(c => IDENTITY_NAMES.has(c.name))
      .map(c => c.name + '=' + c.value)
      .sort()
      .join('|');
  }

  /**
   * 登录完成判据的状态机（纯函数式，时间由调用方注入，便于测试）。
   *
   * @param {string} baseline  启动时的身份签名
   * @param {number} stableMs  新签名需连续保持多久（**时间跨度**，非采样次数；
   *                           实际采样次数由调用方的轮询间隔决定）
   * @returns {{ feed: (sig: string, nowMs: number) => boolean }}
   *          feed 返回 true 表示判定「本次登录已完成」
   *
   * 语义（三条，缺一不可）：
   *   1. 新签名非空          —— 不能把「cookie 被清空」当成登录成功
   *   2. 与基线不同          —— 排除上一账号的残留
   *   3. 连续采样一致且跨度 ≥ stableMs —— 排除读到写一半的过渡态；
   *      中途回到基线或发生抖动会**重置**计时，需重新稳定
   */
  function createLoginDetector(baseline, stableMs) {
    const need = Number(stableMs) || 0;
    let pendingSig = null;
    let pendingAt = 0;

    return {
      feed(sig, nowMs) {
        if (!sig || sig === baseline) {
          pendingSig = null;
          pendingAt = 0;
          return false;
        }
        if (sig !== pendingSig) {
          pendingSig = sig;
          pendingAt = nowMs;
          return false;
        }
        return nowMs - pendingAt >= need;
      },
    };
  }

  return { IDENTITY_NAMES, identitySignature, createLoginDetector };
});
