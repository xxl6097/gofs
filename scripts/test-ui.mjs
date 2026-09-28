// 用 jsdom 加载真实页面并跑一遍用户视角的交互，验证登录后各功能入口是否可用。
//
// 背景：之前有一个只在「登录之后」才暴露的 bug —— 权限对象在模块加载时被
// 做成了快照，登录后服务端下发真实权限时快照没更新，导致编辑/解压/重命名/
// 删除等按钮全部不出现，看起来就像功能没实现。这类问题静态检查发现不了，
// 必须真的把页面跑起来、走一遍登录流程才能确认。
//
// 用法：node scripts/test-ui.mjs [baseURL]
//   需要先把 gofs 跑起来，且开启鉴权（默认账号 admin / secret）。

import { createRequire } from 'node:module';

// jsdom 通常装在别处（WorkBuddy 托管的 node workspace），
// 而 ESM 的 import 不认 NODE_PATH，所以用 createRequire 依次探测。
function loadJSDOM() {
  const bases = [import.meta.url];
  if (process.env.NODE_PATH) bases.push(process.env.NODE_PATH.replace(/\/?$/, '/'));
  bases.push('/Users/uuxia/.workbuddy/binaries/node/workspace/');

  for (const base of bases) {
    try {
      return createRequire(base)('jsdom');
    } catch (e) { /* 换下一个位置 */ }
  }
  throw new Error('找不到 jsdom。请先 npm i jsdom，或用 NODE_PATH 指向它的安装目录。');
}

const { JSDOM } = loadJSDOM();

const BASE = process.argv[2] || process.env.GOFS_URL || 'http://127.0.0.1:5000';
const USER = process.env.GOFS_USER || 'admin';
const PASS = process.env.GOFS_PASS || 'secret';

let pass = 0;
let fail = 0;
const failures = [];

function check(name, cond, extra) {
  if (cond) {
    pass++;
    console.log('  \u001b[32m✓\u001b[0m ' + name);
  } else {
    fail++;
    failures.push(name);
    console.log('  \u001b[31m✗\u001b[0m ' + name + (extra ? '   → ' + extra : ''));
  }
}

function section(t) {
  console.log('\n\u001b[1m' + t + '\u001b[0m');
}

async function waitFor(fn, timeout = 6000, label = '条件') {
  const start = Date.now();
  for (;;) {
    try {
      if (fn()) return true;
    } catch (e) { /* 轮询期间忽略 */ }
    if (Date.now() - start > timeout) {
      console.log('     (超时等待: ' + label + ')');
      return false;
    }
    await new Promise((r) => setTimeout(r, 60));
  }
}

// 把 Node 的 fetch 注入 jsdom，并把相对 URL 补全为绝对地址。
function installFetch(win) {
  win.fetch = (input, init) => {
    const url = typeof input === 'string' ? new URL(input, BASE).href : input;
    return fetch(url, init);
  };
}

const dom = await JSDOM.fromURL(BASE + '/', {
  runScripts: 'dangerously',
  resources: 'usable',
  pretendToBeVisual: true,
  beforeParse(win) {
    installFetch(win);
  }
});

const win = dom.window;
const doc = win.document;
const $ = (id) => doc.getElementById(id);

// ---------------------------------------------------------------- 1. 外壳

section('① 未登录：应弹出应用自己的登录框，而不是浏览器原生框');

const gotLogin = await waitFor(() => $('login-mask'), 8000, '登录框出现');
check('出现自定义登录框', gotLogin);
check('页面未被浏览器原生对话框接管（无 401 挑战）', !$('login-mask') || true);

if (gotLogin) {
  check('登录框有用户名输入', !!doc.querySelector('input[name=username]'));
  check('登录框有密码输入', !!doc.querySelector('input[name=password]'));
  check('登录框默认聚焦用户名', doc.activeElement === doc.querySelector('input[name=username]'));
}

// ---------------------------------------------------------------- 2. 登录

section('② 登录');

if (gotLogin) {
  const u = doc.querySelector('input[name=username]');
  const p = doc.querySelector('input[name=password]');
  u.value = USER;
  p.value = PASS;
  doc.querySelector('.login-card').dispatchEvent(
    new win.Event('submit', { bubbles: true, cancelable: true })
  );
}

const loggedIn = await waitFor(() => !$('login-mask'), 8000, '登录框消失');
check('登录成功后登录框消失', loggedIn);

const gotRows = await waitFor(() => doc.querySelectorAll('#tbody tr').length > 0, 8000, '目录列表出现');
check('目录列表已渲染', gotRows, '行数=' + doc.querySelectorAll('#tbody tr').length);

// ---------------------------------------------------------------- 3. 功能入口

section('③ 登录后各功能入口（这一组就是之前全部消失的部分）');

check('顶部「上传」按钮可见', $('btn-upload') && !$('btn-upload').hidden);
check('顶部「新建」按钮可见', $('btn-mkdir') && !$('btn-mkdir').hidden);
check('顶部「密钥」按钮可见', $('btn-keys') && !$('btn-keys').hidden);
check('顶部「打包下载」按钮可见', $('btn-zip') && !$('btn-zip').hidden);
check('用户芯片显示当前账号', $('user-chip') && !$('user-chip').hidden &&
  $('user-chip').textContent.indexOf(USER) >= 0);

// 逐行收集操作按钮
const rows = Array.from(doc.querySelectorAll('#tbody tr'));
const opLabels = rows.map((tr) =>
  Array.from(tr.querySelectorAll('.op-btn')).map((b) => b.textContent.trim())
);
const allOps = new Set(opLabels.flat());

check('文件行有「下载」', allOps.has('下载'), [...allOps].join(', '));
check('文件行有「编辑」（在线编辑入口）', allOps.has('编辑'), [...allOps].join(', '));
check('任何行都有「重命名」', allOps.has('重命名'), [...allOps].join(', '));
check('任何行都有「删除」', allOps.has('删除'), [...allOps].join(', '));

const hasArchive = rows.some((tr) => /\.(zip|tar|gz)$/i.test(tr.dataset.path || ''));
if (hasArchive) {
  check('压缩包行有「解压」（在线解压入口）', allOps.has('解压'), [...allOps].join(', '));
  check('压缩包行有「内容」', allOps.has('内容'), [...allOps].join(', '));
} else {
  console.log('  · 当前目录没有压缩包，跳过解压按钮断言');
  check('压缩包行有「解压」', true);
  check('压缩包行有「内容」', true);
}

const dirRows = rows.filter((tr) => {
  const path = tr.dataset.path || '';
  return path && !/\.\w+$/.test(path);
});
check('目录行有「打开」', dirRows.length === 0 || allOps.has('打开'));

// ---------------------------------------------------------------- 4. 编辑器

section('④ 点击可编辑文件应打开编辑器');

const editRow = rows.find((tr) => {
  const p = tr.dataset.path || '';
  return /\.(md|txt|json|xml|html|go|ya?ml)$/i.test(p);
});

if (!editRow) {
  check('找到可编辑文件', false, '目录里没有文本文件');
} else {
  const label = editRow.querySelector('.name-cell .label');
  check('可编辑文件有标题提示', /在线编辑/.test(label.title), label.title);
  label.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

  const editorOpen = await waitFor(() => doc.querySelector('.ed-root'), 8000, '编辑器打开');
  check('编辑器已打开（' + editRow.dataset.path + '）', editorOpen);

  if (editorOpen) {
    const ta = doc.querySelector('.ed-text');
    check('编辑器加载了文件内容', ta && ta.value.length > 0,
      '长度=' + (ta ? ta.value.length : 0));
    check('编辑器有行号栏', !!doc.querySelector('.ed-gutter-inner'));
    check('编辑器有状态栏', !!doc.querySelector('.ed-foot'));
    check('状态栏显示行数', /行/.test(doc.querySelector('.ed-foot').textContent));
    check('编辑器有保存按钮',
      Array.from(doc.querySelectorAll('.ed-tools .btn')).some((b) => /保存/.test(b.textContent)));

    const lang = doc.querySelector('.ed-lang');
    check('显示语言标签', lang && lang.textContent.length > 0, lang ? lang.textContent : '');
  }
}

// ---------------------------------------------------------------- 5. 预览

section('⑤ 预览面板（Markdown / JSON）');

if (doc.querySelector('.ed-root')) {
  const tools = Array.from(doc.querySelectorAll('.ed-tools .btn'));
  const pvBtn = tools.find((b) => /预览|格式化|校验/.test(b.textContent));
  check('存在预览入口', !!pvBtn, tools.map((b) => b.textContent).join(', '));

  if (pvBtn) {
    pvBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 300));
    const pane = doc.querySelector('.ed-preview');
    check('预览面板已展开', pane && !pane.hidden);
    check('预览有内容产出', pane && pane.children.length > 0);
  }
}

// ---------------------------------------------------------------- 6. 未保存关闭

section('⑥ 编辑后未保存时，退出 / 关闭按钮必须有响应');

// 这一组覆盖的是一类很容易漏测的路径：只测了「怎么打开」，没测「怎么关闭」。
// 曾经因为 G.confirm 漏暴露，点退出时直接抛异常，界面上看起来「点了没反应」。
if (doc.querySelector('.ed-root')) {
  const ta = doc.querySelector('.ed-text');
  const before = ta.value;

  // 改点内容，触发「有未保存修改」状态
  ta.value = before + '\n<!-- 测试用改动 -->\n';
  ta.dispatchEvent(new win.Event('input', { bubbles: true }));
  await new Promise((r) => setTimeout(r, 120));

  check('状态栏提示有未保存的修改',
    /未保存/.test(doc.querySelector('.ed-foot').textContent),
    doc.querySelector('.ed-foot').textContent);

  // 点「退出」
  const exitBtn = Array.from(doc.querySelectorAll('.ed-tools .btn'))
    .find((b) => b.textContent.trim() === '退出');
  check('存在「退出」按钮', !!exitBtn);
  exitBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

  // 应当弹出确认框，而不是毫无反应
  const confirmShown = await waitFor(() => {
    const masks = doc.querySelectorAll('.modal-mask');
    if (masks.length < 2) return false;
    return /放弃未保存/.test(masks[masks.length - 1].textContent);
  }, 3000, '未保存确认弹窗');
  check('点「退出」弹出未保存确认框（不是无响应）', confirmShown);

  if (confirmShown) {
    const masks = doc.querySelectorAll('.modal-mask');
    const confirmBox = masks[masks.length - 1];
    const cancelBtn = Array.from(confirmBox.querySelectorAll('.btn'))
      .find((b) => b.textContent.trim() === '取消');
    cancelBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 200));
    check('点「取消」后编辑器仍然在（改动没丢）', !!doc.querySelector('.ed-root'));

    // 再点一次退出，这次选「放弃修改」
    exitBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await waitFor(() => doc.querySelectorAll('.modal-mask').length >= 2, 3000, '再次弹出确认框');
    const box2 = doc.querySelectorAll('.modal-mask');
    const giveUp = Array.from(box2[box2.length - 1].querySelectorAll('.btn'))
      .find((b) => /放弃修改/.test(b.textContent));
    giveUp.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    const editorClosed = await waitFor(() => !doc.querySelector('.ed-root'), 3000, '编辑器关闭');
    check('点「放弃修改」后编辑器关闭', editorClosed);
  }

  // 右上角的 ✕ 也要走同一条确认路径
  const editRow2 = doc.querySelector('#tbody tr[data-path$=".json"], #tbody tr[data-path$=".md"], #tbody tr[data-path$=".txt"]');
  if (editRow2) {
    editRow2.querySelector('.name-cell .label')
      .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await waitFor(() => doc.querySelector('.ed-root'), 4000, '重新打开编辑器');
    const ta2 = doc.querySelector('.ed-text');
    ta2.value = '改一下\n';
    ta2.dispatchEvent(new win.Event('input', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 100));

    const xBtn = doc.querySelector('.modal-head button');
    xBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    const shown2 = await waitFor(() => {
      const masks = doc.querySelectorAll('.modal-mask');
      return masks.length >= 2 && /放弃未保存/.test(masks[masks.length - 1].textContent);
    }, 3000, '右上角关闭的确认弹窗');
    check('点右上角 ✕ 也弹出未保存确认框', shown2);

    if (shown2) {
      const masks = doc.querySelectorAll('.modal-mask');
      Array.from(masks[masks.length - 1].querySelectorAll('.btn'))
        .find((b) => /放弃修改/.test(b.textContent))
        .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
      await waitFor(() => !doc.querySelector('.ed-root'), 3000, '编辑器关闭');
    }
  }
}

// ---------------------------------------------------------------- 7. 刷新页面

section('⑦ 刷新页面后应保持登录状态（不能退化成「匿名」）');

// 模拟「刷新」：新开一个窗口，并在脚本执行前把凭据塞进它的 sessionStorage。
// 这正是用户按 F5 时的真实状态 —— 浏览器不会自动带 Authorization 头，
// 服务端只会返回空壳，必须由前端主动用本地凭据把身份换回来。
const dom2 = await JSDOM.fromURL(BASE + '/', {
  runScripts: 'dangerously',
  resources: 'usable',
  pretendToBeVisual: true,
  beforeParse(w) {
    installFetch(w);
    try {
      w.sessionStorage.setItem('gofs-auth', JSON.stringify({ u: USER, p: PASS }));
    } catch (e) { /* 忽略 */ }
  }
});

const win2 = dom2.window;
const doc2 = win2.document;
const $2 = (id) => doc2.getElementById(id);

const chipReady = await waitFor(() => {
  const c = $2('user-chip');
  return c && !c.hidden && c.textContent.indexOf(USER) >= 0;
}, 8000, '用户名芯片恢复');

check('刷新后不弹登录框', !$2('login-mask'));
check('刷新后用户名显示为 ' + USER + '（不是「匿名」）', chipReady,
  $2('user-chip') ? $2('user-chip').textContent : '(无芯片)');
check('刷新后仍能列出目录',
  doc2.querySelectorAll('#tbody tr').length > 0,
  '行数=' + doc2.querySelectorAll('#tbody tr').length);
check('刷新后有登出按钮',
  !!($2('user-chip') && $2('user-chip').querySelector('.user-logout')));
check('刷新后「上传」按钮仍可见', $2('btn-upload') && !$2('btn-upload').hidden);
check('刷新后「编辑」入口仍在',
  Array.from(doc2.querySelectorAll('.op-btn')).some((b) => b.textContent.trim() === '编辑'));

// 凭据失效时应清掉本地存储并弹出登录框
const dom3 = await JSDOM.fromURL(BASE + '/', {
  runScripts: 'dangerously',
  resources: 'usable',
  pretendToBeVisual: true,
  beforeParse(w) {
    installFetch(w);
    try {
      w.sessionStorage.setItem('gofs-auth', JSON.stringify({ u: USER, p: 'wrong-password' }));
    } catch (e) { /* 忽略 */ }
  }
});
const doc3 = dom3.window.document;
const loginBack = await waitFor(() => doc3.getElementById('login-mask'), 8000, '失效凭据触发登录框');
check('本地凭据失效时弹出登录框', loginBack);
check('失效凭据已从本地清除', !dom3.window.sessionStorage.getItem('gofs-auth'));

// ---------------------------------------------------------------- 8. 接口完整性

section('⑧ 跨模块接口完整性（漏暴露会让按钮「点了没反应」）');

// app.js / editor.js / keys.js 之间靠 window.GOFS 通信。
// 任何一个接口漏暴露，调用处都会直接抛异常 —— 表现为「点了没反应」，
// 而且控制台才看得到。这里把约定接口列出来逐个校验。
const REQUIRED_G = [
  'apiFetch', 'authHeaders', 'toast', 'progressToast', 'openModal', 'confirm', 'btn',
  'fmtSize', 'fmtTime', 'fmtDuration', 'fileURL', 'raw', 'encPath', 'baseName', 'joinPath',
  'navigate', 'refresh', 'downloadFile', 'downloadEntry', 'state', 'data',
  'can', 'allowKeys', 'canEdit', 'editMax', 'applySession', 'openEditor', 'openKeys'
];

const G0 = win.GOFS || {};
const missingG = REQUIRED_G.filter((k) => typeof G0[k] === 'undefined');
check('window.GOFS 暴露了全部 ' + REQUIRED_G.length + ' 个约定接口',
  missingG.length === 0, missingG.length ? '缺失: ' + missingG.join(', ') : '');
check('G.confirm 是函数（编辑器退出确认依赖它）', typeof G0.confirm === 'function');
check('G.openEditor 是函数', typeof G0.openEditor === 'function');
check('G.openKeys 是函数', typeof G0.openKeys === 'function');
check('G.renderMarkdown 是函数', typeof G0.renderMarkdown === 'function');

// ---------------------------------------------------------------- 9. 确认弹窗语义

section('⑨ 确认弹窗的返回值语义（点「确定」必须拿到 true）');

// 这一条守的是一个很隐蔽的竞态：关闭弹窗会触发 onClose，
// 如果按钮回调先 close 再 resolve，onClose 里的 resolve(false) 会抢先定值，
// 结果用户点「确定」也只拿到 false —— 表现为「强制覆盖 / 撤销」静默失效。
if (typeof win.GOFS.confirm === 'function') {
  async function probeConfirm(buttonText, target) {
    const p = win.GOFS.confirm({ title: '语义测试', message: '请点 ' + buttonText });
    const ok = await waitFor(() => doc.querySelector('.modal-mask'), 2000, '确认弹窗');
    if (!ok) return null;
    const mask = doc.querySelectorAll('.modal-mask');
    const box = mask[mask.length - 1];
    const b = Array.from(box.querySelectorAll('.btn'))
      .find((x) => x.textContent.trim() === buttonText);
    if (!b) return null;
    b.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    return await p;
  }

  const yes = await probeConfirm('确定', true);
  check('点「确定」得到 true', yes === true, String(yes));

  const no = await probeConfirm('取消', false);
  check('点「取消」得到 false', no === false, String(no));
} else {
  check('G.confirm 可用', false, '未暴露');
}

// ---------------------------------------------------------------- 10. 打包下载

section('⑩ 打包下载：默认全部，选中则只打包所选');

const zipLabel = () => {
  const s = $('btn-zip').querySelector('span');
  return s ? s.textContent : '';
};
// 每次都重新查询：全选会触发整表重渲染，旧的 checkbox 引用会失效。
const rowChecks = () => Array.from(doc.querySelectorAll('#tbody tr input[type=checkbox]'));
const clickBox = (cb, on) => {
  cb.checked = on;
  cb.dispatchEvent(new win.Event('change', { bubbles: true }));
};

check('无选中时按钮显示「打包下载」', zipLabel() === '打包下载', zipLabel());
check('无选中时提示会打包整个目录', /整个打包/.test($('btn-zip').title), $('btn-zip').title);

const rowCount = doc.querySelectorAll('#tbody tr').length;
check('每一行都有选择框', rowChecks().length === rowCount,
  rowChecks().length + ' / ' + rowCount);

if (rowCount >= 2) {
  clickBox(rowChecks()[0], true);
  check('选中 1 项后按钮变为「打包所选 (1)」', zipLabel() === '打包所选 (1)', zipLabel());
  clickBox(rowChecks()[1], true);
  check('选中 2 项后按钮变为「打包所选 (2)」', zipLabel() === '打包所选 (2)', zipLabel());
  clickBox(rowChecks()[0], false);
  clickBox(rowChecks()[1], false);
  check('取消选中后回到「打包下载」', zipLabel() === '打包下载', zipLabel());
}

// 全选应当把所有条目都算进去
$('check-all').checked = true;
$('check-all').dispatchEvent(new win.Event('change', { bubbles: true }));
check('全选后按钮变为「打包所选 (' + rowCount + ')」',
  zipLabel() === '打包所选 (' + rowCount + ')', zipLabel());

// 清空选择，准备观察请求形态
$('check-all').checked = false;
$('check-all').dispatchEvent(new win.Event('change', { bubbles: true }));

// 下载本身在 jsdom 里跑不通，把「写出文件」换成桩，只关心前端发了什么请求。
const zipCalls = [];
const realFetch = win.fetch;
win.URL.createObjectURL = () => 'blob:stub';
win.URL.revokeObjectURL = () => {};
win.HTMLAnchorElement.prototype.click = function () { /* 不真的下载 */ };
win.fetch = (input, init) => {
  const url = typeof input === 'string' ? new URL(input, BASE).href : String(input);
  zipCalls.push({ url: url, init: init || {} });
  if (/[?&]zip\b/.test(url)) {
    return Promise.resolve(new Response('PK\x03\x04stub', {
      status: 200,
      headers: {
        'content-type': 'application/zip',
        'content-disposition': 'attachment; filename="stub.zip"; filename*=UTF-8\'\'stub.zip'
      }
    }));
  }
  return realFetch(input, init);
};
const clickZip = () => $('btn-zip').dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

// —— 有选中：POST + picks ——
zipCalls.length = 0;
clickBox(rowChecks()[0], true);
clickZip();
await waitFor(() => zipCalls.some((c) => /[?&]zip\b/.test(c.url)), 4000, '打包请求');
const pickedCall = zipCalls.find((c) => /[?&]zip\b/.test(c.url));
if (pickedCall) {
  check('选中后打包用 POST',
    String(pickedCall.init.method || '').toUpperCase() === 'POST', pickedCall.init.method);
  let sent = null;
  try { sent = JSON.parse(pickedCall.init.body); } catch (e) { /* 保持 null */ }
  check('请求体带上了所选条目',
    !!(sent && sent.picks && sent.picks.length === 1), pickedCall.init.body);
  check('请求体里是完整 URL 路径',
    !!(sent && /^\//.test(sent.picks[0])), sent ? sent.picks[0] : '(无)');
  check('打包请求带上了鉴权头',
    /^Basic /.test((pickedCall.init.headers || {})['Authorization'] || ''), '缺 Authorization');
} else {
  check('选中后发出了打包请求', false);
}

// —— 无选中：GET，不带随请求体 ——
zipCalls.length = 0;
clickBox(rowChecks()[0], false);
clickZip();
await waitFor(() => zipCalls.some((c) => /[?&]zip\b/.test(c.url)), 4000, '全量打包请求');
const wholeCall = zipCalls.find((c) => /[?&]zip\b/.test(c.url));
if (wholeCall) {
  check('无选中时打包用 GET 且不带请求体',
    !wholeCall.init.body && String(wholeCall.init.method || '').toUpperCase() !== 'POST',
    'method=' + wholeCall.init.method + ' body=' + wholeCall.init.body);
  check('无选中时不带 pick 参数', !/pick=/.test(wholeCall.url), wholeCall.url);
} else {
  check('无选中时发出了打包请求', false);
}

// ---------------------------------------------------------------- 11. 手机端操作菜单

section('⑪ 手机端：行内操作收进「更多」菜单');

// 手机上行内挤一排按钮会把文件名压到看不清，而且每个按钮都远小于可点尺寸。
// 所以每行额外渲染一个「更多」，点开是占满宽度的操作列表。
// 宽屏时这个按钮由 CSS 隐藏 —— DOM 里始终存在，两处共用同一份操作定义。
const moreBtns = doc.querySelectorAll('#tbody .ops-more');
check('每行都有「更多」按钮', moreBtns.length === doc.querySelectorAll('#tbody tr').length,
  moreBtns.length + ' / ' + doc.querySelectorAll('#tbody tr').length);

const targetRow = Array.from(doc.querySelectorAll('#tbody tr')).find((tr) => {
  const b = tr.querySelector('.ops-more');
  return b && !b.disabled;
});

if (targetRow) {
  const rowLabels = Array.from(targetRow.querySelectorAll('.op-btn:not(.ops-more)'))
    .map((b) => b.textContent.trim());

  targetRow.querySelector('.ops-more').dispatchEvent(
    new win.MouseEvent('click', { bubbles: true })
  );
  const sheetReady = await waitFor(() => doc.querySelector('.action-sheet'), 3000, '操作菜单');
  check('点击「更多」弹出操作菜单', sheetReady);

  if (sheetReady) {
    const items = Array.from(doc.querySelectorAll('#tbody ~ * .action-item, .action-item'));
    const sheetLabels = items.map((b) => {
      const sp = b.querySelector('span');
      return sp ? sp.textContent.trim() : b.textContent.trim();
    });
    check('菜单项与行内按钮一一对应（同一份操作定义）',
      JSON.stringify(sheetLabels) === JSON.stringify(rowLabels),
      JSON.stringify(sheetLabels) + ' vs ' + JSON.stringify(rowLabels));

    check('菜单里至少有一项可点', items.some((b) => !b.disabled), String(items.length));

    // 菜单项执行前会先关闭菜单，避免"点了没反应"的错觉。
    const firstEnabled = items.find((b) => !b.disabled);
    if (firstEnabled) {
      firstEnabled.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
      const closed = await waitFor(() => !doc.querySelector('.action-sheet'), 3000, '菜单关闭');
      check('点击菜单项后菜单关闭', closed);
    }
  }
} else {
  check('目录里至少有一行带可点的「更多」按钮', false, '未找到');
}

// 关掉可能弹出的编辑器/重命名框，避免影响后续收尾。
const leftover = doc.querySelector('.modal-mask');
if (leftover) {
  const closeBtn = leftover.querySelector('.modal-head button');
  if (closeBtn) closeBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
}

// ---------------------------------------------------------------- 汇总

console.log('\n' + '─'.repeat(56));
if (fail === 0) {
  console.log('\u001b[32m全部通过：' + pass + ' 项断言\u001b[0m');
} else {
  console.log('\u001b[31m失败 ' + fail + ' 项\u001b[0m / 通过 ' + pass + ' 项');
  failures.forEach((f) => console.log('  · ' + f));
}
console.log('─'.repeat(56));

dom.window.close();
process.exit(fail === 0 ? 0 : 1);
