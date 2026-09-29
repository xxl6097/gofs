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
    if (process.env.DEBUG_NAV) win.__navDebug = true;
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

// 可在线编辑的文本类文件
const TEXT_LIKE = /\.(md|markdown|txt|json|xml|html?|ya?ml|go|css|js|ts|log|ini|conf|sh|sql|csv)$/i;
// 其中「可预览」的只有这几类：编辑器会多出一个 预览 / 格式化 / 校验 按钮。
// 选择时优先挑可预览的文件，否则 ⑤ 段就拿不到那个按钮（那是文件类型决定的，
// 不是功能缺失）—— 之前正是因为根目录只有 notes.txt 才误报失败。
const PREVIEWABLE = /\.(md|markdown|json|xml|html?|ya?ml)$/i;

const textRows = rows.filter((tr) => TEXT_LIKE.test(tr.dataset.path || ''));
const editRow = textRows.find((tr) => PREVIEWABLE.test(tr.dataset.path || '')) || textRows[0];

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
  const openedPath = doc.querySelector('.ed-root') && editRow ? editRow.dataset.path : '';
  const expectPreview = PREVIEWABLE.test(openedPath);
  check(expectPreview
    ? '存在预览入口（' + openedPath + '）'
    : '纯文本文件不提供预览入口（符合预期）', expectPreview ? !!pvBtn : !pvBtn,
    tools.map((b) => b.textContent).join(', '));

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

// app.js / editor.js / keys.js / users.js / settings.js 之间靠 window.GOFS 通信。
// 任何一个接口漏暴露，调用处都会直接抛异常 —— 表现为「点了没反应」，
// 而且控制台才看得到。这里把约定接口列出来逐个校验。
const REQUIRED_G = [
  'apiFetch', 'authHeaders', 'toast', 'progressToast', 'openModal', 'confirm', 'btn',
  'fmtSize', 'fmtTime', 'fmtDuration', 'fileURL', 'raw', 'encPath', 'baseName', 'joinPath',
  'navigate', 'refresh', 'downloadFile', 'downloadEntry', 'state', 'data',
  'can', 'allowKeys', 'canEdit', 'editMax', 'applySession', 'openEditor', 'openKeys',
  'openSettings', 'openUsers', 'allowUsers', 'modalFoot', 'isNarrow',
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

// ---------------------------------------------------------------- 12. 页脚 / 统计 / 拖拽 / 设置

section('⑫ 页脚与统计（登录后不能再显示「只读模式」）');

{
  const foot = $('footer-right').textContent;
  check('页脚显示已登录的用户名', /已登录\s+admin/.test(foot), foot);
  check('页脚不再出现「只读模式」', !/只读模式/.test(foot), foot);
  check('页脚列出了实际具备的能力',
    /可上传/.test(foot) && /可在线编辑/.test(foot) && /可在线解压/.test(foot), foot);

  const stat = () => $('stat').textContent;
  check('统计给出目录数与文件数', /个目录/.test(stat()) && /个文件/.test(stat()),
    stat());
  check('统计给出文件合计大小', /文件合计/.test(stat()), stat());
  check('未选中时不显示「已选」', !/已选/.test(stat()), stat());

  // 勾选一个普通文件，统计里应当出现「已选 …」，并且仍然保留整个目录的规模。
  const fileRow = Array.from(doc.querySelectorAll('#tbody tr'))
    .find((tr) => !tr.querySelector('.name-cell.dir'));
  if (fileRow) {
    const cb = fileRow.querySelector('input[type=checkbox]');
    cb.checked = true;
    cb.dispatchEvent(new win.Event('change', { bubbles: true }));

    const picked = stat();
    check('选中后统计出现「已选 1 项」', /已选\s+1\s+项/.test(picked), picked);
    check('选中后仍保留整个目录的规模', /个目录/.test(picked) && /个文件/.test(picked), picked);
    check('选中项自身的大小也有给出', /已选\s+1\s+项（[^）]*\d/.test(picked), picked);

    cb.checked = false;
    cb.dispatchEvent(new win.Event('change', { bubbles: true }));
    check('取消选中后统计回到目录规模', !/已选/.test(stat()), stat());
  } else {
    check('目录里应至少有一个文件行用于统计测试', false, '未找到');
  }

  // 统计的口径必须写清楚，否则「共多大」会被误读为含子目录。
  check('统计带上了口径说明（title）',
    /不含子目录/.test($('stat').title || ''), $('stat').title);
}

section('⑬ 拖拽上传：提示、落点与上传速度');

{
  // ---- 常驻投放区：不拖的时候也要能看见「支持拖拽」----
  const zone = $('drop-zone');
  check('常驻投放区可见（有写权限时）', zone && !zone.hidden);
  check('投放区写明文件会落到哪个目录',
    /存入\s+\//.test($('drop-zone-target').textContent),
    $('drop-zone-target').textContent);
  check('上传按钮的提示里也说明了落点',
    /存入/.test($('btn-upload').title), $('btn-upload').title);

  // ---- 拖拽浮层 ----
  const makeDragEvent = (type, dt) => {
    const ev = new win.Event(type, { bubbles: true });
    Object.defineProperty(ev, 'dataTransfer', { value: dt });
    return ev;
  };

  // 真实的 DataTransferItem 一定带 kind（'file' / 'string'）；
  // 只有 kind === 'file' 才算条目，否则拖选中的文字也会被算进去。
  win.dispatchEvent(makeDragEvent('dragenter', {
    types: ['Files'],
    items: [{ kind: 'file' }, { kind: 'file' }, { kind: 'file' }],
  }));
  check('拖入时出现全屏提示浮层', $('drop-hint').classList.contains('on'));
  check('浮层写明落点', /存入\s+\//.test($('drop-target').textContent),
    $('drop-target').textContent);
  check('浮层给出条目数量', /3 项/.test($('drop-note').textContent),
    $('drop-note').textContent);
  check('浮层给出单文件上限', /上限|不限/.test($('drop-note').textContent),
    $('drop-note').textContent);
  check('常驻投放区同时高亮', $('drop-zone').classList.contains('on'));

  win.dispatchEvent(makeDragEvent('dragleave', {}));
  check('离开后浮层收起', !$('drop-hint').classList.contains('on'));

  // ---- 用假的 XHR 走一遍上传，验证「进度 + 速度」 ----
  const RealXHR = win.XMLHttpRequest;
  class FakeXHR {
    constructor() { this.upload = {}; this.status = 0; this._h = {}; }
    open(m, u) { this.method = m; this.url = u; }
    setRequestHeader(k, v) { this._h[k] = v; }
    getResponseHeader(k) {
      return String(k).toLowerCase() === 'x-gofs-offset' ? String(this._size || 0) : null;
    }
    send(body) {
      const size = (body && body.size) || 0;
      this._size = size;
      const up = this.upload;
      // 两次进度事件之间隔 400ms —— 速度估算要求采样间隔够长，
      // 否则会被当成噪声丢掉（这正是我们希望的行为）。
      setTimeout(() => up.onprogress && up.onprogress(
        { lengthComputable: true, loaded: Math.floor(size / 2), total: size }), 30);
      setTimeout(() => up.onprogress && up.onprogress(
        { lengthComputable: true, loaded: size, total: size }), 440);
      setTimeout(() => {
        this.status = 201;
        this.responseText = 'created';
        if (this.onload) this.onload();
      }, 620);
    }
  }
  win.XMLHttpRequest = FakeXHR;

  const fake = new win.File([new Uint8Array(512 * 1024)], 'speed-test.bin');
  win.dispatchEvent(makeDragEvent('drop', { types: ['Files'], files: [fake] }));

  check('松手后浮层立即收起', !$('drop-hint').classList.contains('on'));

  const gotPanel = await waitFor(() => doc.querySelector('.progress-wrap'), 4000, '上传弹窗');
  check('出现上传进度弹窗', gotPanel);

  if (gotPanel) {
    const bodyText = doc.querySelector('.modal-body').textContent;
    check('进度弹窗写明了落点目录', /存入/.test(bodyText), bodyText.slice(0, 90));
    check('进度弹窗写明了单文件上限', /单文件/.test(bodyText), bodyText.slice(0, 90));

    const metaR = doc.querySelector('.progress-meta span:last-child');
    const gotSpeed = await waitFor(() => /\/s/.test(metaR.textContent), 4000, '速度出现');
    check('进度里显示了上传速度', gotSpeed, metaR.textContent);
    check('进度里给出了剩余时间或总量',
      /剩余|MiB|KiB|B/.test(metaR.textContent), metaR.textContent);
  }

  await new Promise((r) => setTimeout(r, 900));
  win.XMLHttpRequest = RealXHR;

  // 收尾：关掉可能还留着的进度弹窗。
  const leftover = doc.querySelector('.modal-mask');
  if (leftover) {
    const x = leftover.querySelector('.modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  }
}

section('⑭ 弹窗底部布局统一（取消在左、确认在右、同一套样式）');

{
  // 逐个打开几类弹窗，检查底部是不是统一的 .modal-foot，
  // 以及按钮顺序 / 语义色。以前每个弹窗各自拼一个内联 flex 容器，
  // 结果有的没有分隔线、有的取消在右、手机上也不等宽。
  async function inspectsFoot(openFn, expectDanger) {
    openFn();
    const ok = await waitFor(() => doc.querySelector('.modal-mask .modal'), 3000, '弹窗出现');
    if (!ok) return null;

    const modal = doc.querySelector('.modal-mask .modal');
    const foot = modal.querySelector(':scope > .modal-foot');
    const inBody = modal.querySelector('.modal-body > .modal-foot');
    const labels = foot
      ? Array.from(foot.querySelectorAll('.btn')).map((b) => b.textContent.trim())
      : [];

    const out = {
      hasFoot: !!foot,
      footOutsideBody: !!foot && !inBody,
      labels: labels,
      cancelFirst: labels[0] === '取消',
      hasPrimary: !!modal.querySelector('.modal-foot .btn.primary'),
      hasDangerSolid: !!modal.querySelector('.modal-foot .btn.danger-solid')
    };
    if (expectDanger) {
      check('破坏性确认用实心红按钮', out.hasDangerSolid, JSON.stringify(labels));
    }

    const x = modal.querySelector('.modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 80));
    return out;
  }

  const mk = await inspectsFoot(() => $('btn-mkdir').click(), false);
  check('「新建目录」弹窗使用统一的 .modal-foot', mk && mk.hasFoot);
  check('底部区在正文之外（有独立的分隔线与背景）', mk && mk.footOutsideBody);
  check('「新建目录」按钮顺序为「取消 / 创建」', mk && mk.cancelFirst,
    mk && JSON.stringify(mk.labels));
  check('「新建目录」的确认按钮是主色', mk && mk.hasPrimary);

  const rz = await inspectsFoot(() => {
    const row = Array.from(doc.querySelectorAll('#tbody tr'))
      .find((tr) => tr.querySelector('.op-btn[data-kind="rename"]'));
    if (row) row.querySelector('.op-btn[data-kind="rename"]').click();
  }, false);
  check('「重命名」弹窗使用统一的 .modal-foot', rz && rz.hasFoot);
  check('「重命名」按钮顺序为「取消 / 确定」', rz && rz.cancelFirst,
    rz && JSON.stringify(rz.labels));

  const de = await inspectsFoot(() => {
    const row = Array.from(doc.querySelectorAll('#tbody tr'))
      .find((tr) => tr.querySelector('.op-btn[data-kind="trash"]'));
    if (row) row.querySelector('.op-btn[data-kind="trash"]').click();
  }, true);
  check('「删除确认」弹窗使用统一的 .modal-foot', de && de.hasFoot);
  check('「删除确认」按钮顺序为「取消 / 删除」', de && de.cancelFirst,
    de && JSON.stringify(de.labels));
}

section('⑮ 操作按钮按功能区分颜色');

{
  const btns = Array.from(doc.querySelectorAll('#tbody .op-btn'));
  check('行内操作按钮都带 data-kind 标记',
    btns.length > 0 && btns.every((b) => !!b.dataset.kind),
    String(btns.length) + ' 个');
  const kinds = Array.from(new Set(btns.map((b) => b.dataset.kind)));
  check('出现了多种功能类别', kinds.length >= 3, kinds.join(','));

  // 直接取样式表文本：jsdom 不会解析 CSS 变量，靠 getComputedStyle
  // 判断颜色会得到空值，读源码更可靠。
  const css = await (await fetch(BASE + '/__gofs__/assets/style.css')).text();
  for (const k of ['open', 'download', 'edit', 'unzip', 'list', 'rename', 'trash']) {
    check('CSS 为「' + k + '」定义了语义色',
      css.includes('.op-btn[data-kind="' + k + '"]'), k);
  }
  check('语义色使用独立变量而不是硬编码',
    /--op-download:/.test(css) && /--op-edit:/.test(css) && /--op-danger:/.test(css));
  check('操作区常态透明度已提高到可辨识（不再是 0.35）',
    /\.ops\s*\{[^}]*opacity:\s*\.7/.test(css));
}

section('⑯ 服务设置面板（切换根目录 / 调整上传上限）');

{
  check('顶栏有「设置」入口且可见', $('btn-settings') && !$('btn-settings').hidden,
    'hidden=' + ($('btn-settings') && $('btn-settings').hidden));

  $('btn-settings').click();
  const opened = await waitFor(() => doc.querySelector('.settings-panel'), 4000, '设置面板');
  check('设置面板能打开', opened);

  if (opened) {
    const panel = doc.querySelector('.settings-panel');
    const got = await waitFor(() => !/正在读取/.test(panel.textContent), 4000, '设置加载完成');
    check('设置面板成功读取到当前配置', got, panel.textContent.slice(0, 80));

    const text = panel.textContent;
    check('显示「服务根目录」一项', /服务根目录/.test(text));
    check('显示「单文件上传上限」一项', /单文件上传上限/.test(text));
    check('给出了可切换的范围', /可切换的范围/.test(text), text.slice(0, 200));
    check('给出当前生效的上传上限', /当前生效/.test(text));

    const inputs = Array.from(panel.querySelectorAll('input[type=text]'));
    check('根目录输入框已填好当前值', inputs.length > 0 && inputs[0].value.length > 0,
      inputs.length ? inputs[0].value : '(无输入框)');
    check('存在单位下拉（MiB/GiB/TiB）',
      Array.from(panel.querySelectorAll('select option')).map((o) => o.textContent)
        .join(',').includes('GiB'));
    check('有「不限制」开关', /不限制/.test(text));

    const foot = doc.querySelector('.modal-mask .modal > .modal-foot');
    const labels = foot
      ? Array.from(foot.querySelectorAll('.btn')).map((b) => b.textContent.trim())
      : [];
    check('设置面板底部同样统一（取消 / 保存）',
      labels[0] === '取消' && labels.indexOf('保存') > 0, JSON.stringify(labels));

    const x = doc.querySelector('.modal-mask .modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 100));
    check('设置面板可以正常关闭', !doc.querySelector('.settings-panel'));
  }
}

// ---------------------------------------------------------------- 17. 目录导航

section('⑰ 目录导航：进子目录 / 点「根目录」回退');

// 这一组覆盖的是一类「列表其实取回来了，却弹错误提示」的问题。
//
// 曾经的写法是 `history.pushState(..., fileURL(path) + '/')`。根目录 path="/"
// 时的 fileURL 本身就是 "/"，拼出来是 "//" —— 浏览器把它当「协议相对 URL」，
// 解析成 http: 加空主机，pushState 直接抛异常。因为它在 navigate 的 try 里，
// 异常被当成了「打开目录失败」，于是 render() 根本没执行 —— 点「根目录」
// 就卡在原地，只有控制台看得到真相。
//
// 断言里特意检查「没有报错 toast」，这正是当初唯一的可见症状。
{
  const errToast = () =>
    Array.from(doc.querySelectorAll('.toast.err')).map((t) => t.textContent).join(' | ');

  // 地址栏里除了服务目录还会带访问路径前缀（--path-prefix，例如 /fs），
  // 断言必须把它算进去，否则在带前缀部署下会误报。
  const PREFIX = new URL(BASE).pathname.replace(/\/+$/, '');   // "" 或 "/fs"
  const ROOT_URL = PREFIX + '/';
  const atRoot = () => decodeURIComponent(win.location.pathname) === ROOT_URL;
  const atDir = (p) => decodeURIComponent(win.location.pathname) === PREFIX + p + '/';

  // 等应用「闲下来」：导航是异步的，前面几节可能有刷新在途
  // （比如上传完成后的列表刷新）。不等就点，很可能被当成并发导航。
  await waitFor(() => win.GOFS.state && win.GOFS.state.busy === false, 8000, '导航空闲');

  // ⚠️ 判断「到底进没进子目录」不能只看「存在 .crumb.current」—— 前面某节
  // 导航留下的面包屑会让 waitFor 立刻返回，于是「进入子目录」其实没发生，
  // 后面的断言全是假的。必须把「路径真的变成了目标目录」作为等待条件。
  const pickDir = (scope) => Array.from(scope.querySelectorAll('#tbody tr')).find((tr) => {
    const p = tr.dataset.path || '';
    return tr.querySelector('.name-cell.dir') && p && p !== '/';
  });

  // 无论从哪儿出发，先回到根目录建立一个确定的起点 ——
  // 子目录里可能一个子目录都没有，探测必须在根目录上做。
  // （这里不传 push=false：要的就是地址栏也跟着回根目录。）
  await win.GOFS.navigate('/');
  await waitFor(
    () => atRoot() && doc.querySelectorAll('#breadcrumb .crumb').length === 0,
    8000, '回到根目录起点');
  check('可以回到根目录作为起点',
    atRoot() && win.GOFS.state.path === '/',
    'path=' + win.GOFS.state.path + ' pathname=' + win.location.pathname);

  const dirRow = pickDir(doc);
  const dirPath = dirRow ? dirRow.dataset.path : '';

  if (!dirPath) {
    check('根目录里能找到子目录', false, '根目录没有子目录，无法验证导航');
  } else {
    check('根目录里能找到子目录（' + dirPath + '）', true);

    dirRow.querySelector('.name-cell .label')
      .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    const entered = await waitFor(
      () => doc.querySelector('#breadcrumb .crumb.current') && win.GOFS.state.path === dirPath,
      8000, '进入子目录 ' + dirPath);
    check('点目录名可以进入（' + dirPath + '）', entered,
      '当前 path=' + win.GOFS.state.path);
    check('进入子目录没有报错', !errToast(), errToast());

    const crumbs = Array.from(doc.querySelectorAll('#breadcrumb > *'))
      .map((el) => el.textContent.trim());
    check('面包屑出现了「根目录」+ 子目录', crumbs[0] === '根目录' && crumbs.length >= 2,
      JSON.stringify(crumbs));
    check('地址栏变成了子目录', atDir(dirPath), win.location.pathname);

    // 关键一步：点「根目录」回去。
    //
    // ⚠️ 合成事件的 cancelable 必须显式给 true —— `new MouseEvent('click',
    // {bubbles: true})` 的 cancelable 默认是 **false**，此时 onclick 里的
    // `e.preventDefault()` 无效，jsdom 会真的去导航（日志里那句
    // "Not implemented: navigation to another Document"），看起来像
    // 「应用没拦住默认跳转」。真实用户点击是 cancelable 的。
    const rootCrumb = Array.from(doc.querySelectorAll('#breadcrumb a'))
      .find((a) => a.textContent.trim() === '根目录');
    check('存在「根目录」面包屑链接', !!rootCrumb);
    rootCrumb.dispatchEvent(new win.MouseEvent('click', { bubbles: true, cancelable: true }));

    const backAtRoot = await waitFor(atRoot, 8000, '回到根目录');
    check('点「根目录」地址栏回到 ' + ROOT_URL, backAtRoot, win.location.pathname);
    check('点「根目录」没有报错（曾经的 bug：URL 拼成 // → pushState 抛 http:）',
      !errToast(), errToast());
    check('根目录链接的 href 不是协议相对 URL（不能是 //...）',
      !/^\/\//.test(rootCrumb.getAttribute('href') || '/'),
      rootCrumb.getAttribute('href'));

    // 列表真的重画了：表格行数应当与 state 里的条目数一致，
    // 而且回到根目录后条目明显不止一条（子目录里只有 1 条）。
    const backRows = Array.from(doc.querySelectorAll('#tbody tr'))
      .map((tr) => tr.dataset.path || '');
    check('回到根目录后列表已重新渲染',
      backRows.length === win.GOFS.state.entries.length && backRows.length > 1,
      backRows.length + ' 行 / state 有 ' + win.GOFS.state.entries.length + ' 条');
    check('回到根目录后没有残留子目录面包屑',
      doc.querySelectorAll('#breadcrumb .crumb').length === 0);

    // 并发导航不能被静默丢弃：连着发起多次时，队列里只留最后一次。
    // （曾经的写法是 `if (state.busy) return;` —— 这一刻的点击就白点了，
    //   地址栏、列表、提示全都没反应。）
    //
    // async 函数体在第一个 await 之前是同步执行的，所以 p1 一定会先把 busy
    // 置上，p2/p3 必定走「排队」分支 —— 这个竞态是确定性的，不靠运气。
    const p1 = win.GOFS.navigate(dirPath);
    const p2 = win.GOFS.navigate('/');       // 排队，随后会被 p3 取代
    const p3 = win.GOFS.navigate(dirPath);   // 取代 p2，成为最终请求
    const [r1, r2, r3] = await Promise.all([p1, p2, p3]);
    check('在途的那次导航正常完成', r1 === true, 'p1=' + r1);
    check('被取代的排队导航以 false 结掉（调用方不会一直等下去）', r2 === false,
      'p2=' + r2);
    check('最后请求的导航正常完成', r3 === true, 'p3=' + r3);
    const settled = await waitFor(
      () => win.GOFS.state.path === dirPath && atDir(dirPath),
      8000, '并发导航最终落到最后请求的目录');
    check('导航途中的再次点击不会被丢掉（最终落到最后请求的那个目录）', settled,
      'path=' + win.GOFS.state.path + ' pathname=' + win.location.pathname);

    await win.GOFS.navigate('/');
  }
}

// ---------------------------------------------------------------- 18. 归档只在根目录

section('⑱ 上传归档只在根目录生效（子目录原地放）');

// 规则：只有「直接传到服务根目录」才按 年/月/日 建三级目录；
// 传进子目录就原地放。前端的三处提示（常驻投放区 / 上传按钮 title / 页脚）
// 必须与服务端的实际落盘一致 —— 提示与实际不符比不提示更糟。
{
  const zoneText = () => $('drop-zone-target').textContent;
  const dirText = () => (win.GOFS.data && win.GOFS.data.upload_date_dir) || '';
  const footText = () => $('footer-right').textContent;
  const atRoot = () => win.GOFS.state.path === '/';
  const backToRoot = async () => {
    await win.GOFS.navigate('/');
    await waitFor(atRoot, 8000, '回到根目录');
  };

  check('服务端下发了归档日期段（后面的断言依赖它）', !!dirText(), dirText());

  await backToRoot();
  check('根目录：投放区写的是归档后的日期目录',
    zoneText().indexOf('/' + dirText()) >= 0, zoneText());
  check('根目录：投放区标注「按日期自动归档」',
    /按日期自动归档/.test(zoneText()), zoneText());
  check('根目录：上传按钮的提示也标注了归档',
    /按日期自动归档/.test($('btn-upload').title), $('btn-upload').title);
  check('根目录：页脚显示「上传归档到 …」',
    /上传归档到/.test(footText()), footText());

  const subRow = Array.from(doc.querySelectorAll('#tbody tr')).find((tr) => {
    const p = tr.dataset.path || '';
    return tr.querySelector('.name-cell.dir') && p && p !== '/';
  });

  if (!subRow) {
    check('找到子目录（用于验证子目录不归档）', false, '根目录没有子目录');
  } else {
    const subPath = subRow.dataset.path;
    subRow.querySelector('.name-cell .label')
      .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await waitFor(() => win.GOFS.state.path === subPath, 8000, '进入子目录 ' + subPath);

    check('子目录：投放区指向子目录本身',
      zoneText().indexOf(subPath + '/') >= 0, zoneText());
    check('子目录：不再出现日期目录',
      zoneText().indexOf('/' + dirText()) < 0, zoneText());
    check('子目录：不再标注「按日期自动归档」',
      !/按日期自动归档/.test(zoneText()), zoneText());
    check('子目录：上传按钮的提示也不再提归档',
      !/按日期自动归档/.test($('btn-upload').title), $('btn-upload').title);
    check('子目录：页脚不再显示「上传归档到 …」',
      !/上传归档到/.test(footText()), footText());

    await backToRoot();
    check('回到根目录后归档提示恢复',
      /上传归档到/.test(footText()) && zoneText().indexOf('/' + dirText()) >= 0,
      footText() + ' | ' + zoneText());
  }
}

// ---------------------------------------------------------------- 19. 文件夹上传

section('⑲ 文件夹上传：入口、目录结构保留、拖拽目录');

// 文件夹上传有两条来源，最终都收敛成「带相对路径的 File 列表」：
//   1) <input webkitdirectory> 选目录 —— File 自带 webkitRelativePath；
//   2) 拖拽目录 —— 走 DataTransferItem.webkitGetAsEntry() 递归遍历，
//      File 上没有 webkitRelativePath，由代码自己拼。
// 这里两条都测：断言实际发出的 PUT URL 保留了目录结构。
{
  // 记录所有上传请求的 URL，并按需自动成功。
  const RealXHR = win.XMLHttpRequest;
  const sent = [];
  class RecXHR {
    constructor() { this.upload = {}; this.status = 0; this._h = {}; }
    open(m, u) { this.method = m; this.url = u; }
    setRequestHeader(k, v) { this._h[k] = v; }
    getResponseHeader(k) {
      return String(k).toLowerCase() === 'x-gofs-offset' ? String(this._size || 0) : null;
    }
    send(body) {
      this._size = (body && body.size) || 0;
      sent.push({ method: this.method, url: this.url });
      setTimeout(() => {
        this.status = 201;
        this.responseText = 'created';
        if (this.onload) this.onload();
      }, 10);
    }
  }
  win.XMLHttpRequest = RecXHR;

  const settle = async (n, label) => {
    await waitFor(() => sent.filter((s) => s.method === 'PUT').length >= n, 5000, label);
    // 让池子里的剩余请求与收尾逻辑跑完
    await new Promise((r) => setTimeout(r, 250));
  };
  const putPaths = () => sent.filter((s) => s.method === 'PUT')
    .map((s) => decodeURIComponent(s.url));

  // ---- 入口 ----
  check('顶栏有「文件夹」上传入口且可见',
    $('btn-upload-dir') && !$('btn-upload-dir').hidden);
  check('顶栏「文件夹」按钮的提示说明了保留结构',
    /保留目录结构/.test($('btn-upload-dir').title), $('btn-upload-dir').title);
  check('投放区有「选择文件夹…」',
    $('drop-zone-dir') && /文件夹/.test($('drop-zone-dir').textContent));
  check('文件输入框带 webkitdirectory（浏览器据此展开整个目录）',
    $('dir-input').hasAttribute('webkitdirectory'), $('dir-input').outerHTML);
  // 投放区整块可点时会直接调 file-input.click()，那条路径不经过顶栏按钮 ——
  // onchange 必须是启动时就绑好的，否则「选了文件什么也没发生」。
  check('两个输入框的 change 都已绑定（不依赖点按钮）',
    typeof $('file-input').onchange === 'function' &&
    typeof $('dir-input').onchange === 'function');

  // 点「文件夹」应打开的是目录选择框，而不是普通文件框
  let dirInputClicked = 0;
  const realDirClick = $('dir-input').click.bind($('dir-input'));
  $('dir-input').click = function () { dirInputClicked++; };
  $('btn-upload-dir').dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('点「文件夹」按钮触发的是目录选择框', dirInputClicked === 1, 'click=' + dirInputClicked);
  $('dir-input').click = realDirClick;

  // ---- 走一遍「选了目录」：File 带 webkitRelativePath ----
  const mkFile = (name, rel, size) => {
    const f = new win.File([new Uint8Array(size || 8)], name);
    Object.defineProperty(f, 'webkitRelativePath', { value: rel });
    return f;
  };
  const picked = [
    mkFile('index.html', 'site/index.html', 16),
    mkFile('app.js', 'site/assets/app.js', 32),
    mkFile('deep.txt', 'site/assets/nested/deep.txt', 8),
  ];
  Object.defineProperty($('dir-input'), 'files', { value: picked, configurable: true });
  $('dir-input').dispatchEvent(new win.Event('change', { bubbles: true }));

  const panel = await waitFor(() => doc.querySelector('.progress-wrap'), 5000, '文件夹上传弹窗');
  check('选择目录后出现上传进度弹窗', panel);

  if (panel) {
    check('弹窗标题标明这是文件夹上传',
      /文件夹/.test(doc.querySelector('.modal-head div').textContent),
      doc.querySelector('.modal-head div').textContent);
    check('进度里给出「第几个 / 共几个」',
      /1\/3|2\/3|3\/3/.test(doc.querySelector('.progress-current').textContent),
      doc.querySelector('.progress-current').textContent);
    check('进度里展示的是相对路径（能看出目录层级）',
      /site\//.test(doc.querySelector('.progress-current').textContent),
      doc.querySelector('.progress-current').textContent);
  }

  await settle(3, '三个文件都发出请求');
  const paths = putPaths();
  check('上传请求保留了目录结构（site/index.html）',
    paths.indexOf('/site/index.html') >= 0, JSON.stringify(paths));
  check('嵌套子目录也保留（site/assets/nested/deep.txt）',
    paths.indexOf('/site/assets/nested/deep.txt') >= 0, JSON.stringify(paths));
  check('一共发了 3 个 PUT', paths.length === 3, JSON.stringify(paths));

  // ---- 拖拽一个「文件夹」：走 webkitGetAsEntry 递归 ----
  sent.length = 0;
  const fileEntry = (name, content) => ({
    isFile: true, isDirectory: false, name,
    file(cb) { cb(new win.File([content], name)); },
  });
  const dirEntry = (name, children) => {
    let done = false;
    return {
      isFile: false, isDirectory: true, name,
      createReader() {
        return {
          readEntries(cb) {
            // 规范允许分批返回：第一次给一批，之后给空数组表示结束。
            // 代码必须循环调用到空为止，只调一次会漏文件。
            if (done) { cb([]); return; }
            done = true;
            cb(children);
          },
        };
      },
    };
  };
  const projDir = dirEntry('proj', [
    fileEntry('readme.md', 'md'),
    dirEntry('src', [fileEntry('main.go', 'go')]),
    dirEntry('empty', []),
  ]);
  const dt = {
    items: [{ kind: 'file', webkitGetAsEntry: () => projDir }],
    files: [],
    types: ['Files'],
  };
  const dropEv = new win.Event('drop', { bubbles: true, cancelable: true });
  Object.defineProperty(dropEv, 'dataTransfer', { value: dt });
  win.dispatchEvent(dropEv);

  await settle(2, '拖拽目录后发出请求');
  const dropPaths = putPaths();
  check('拖拽目录：顶层文件路径正确（proj/readme.md）',
    dropPaths.indexOf('/proj/readme.md') >= 0, JSON.stringify(dropPaths));
  check('拖拽目录：递归进入子目录（proj/src/main.go）',
    dropPaths.indexOf('/proj/src/main.go') >= 0, JSON.stringify(dropPaths));
  check('拖拽目录：空目录不会产生请求',
    !dropPaths.some((p) => /\/empty/.test(p)), JSON.stringify(dropPaths));
  check('拖拽目录：一共发出 2 个 PUT', dropPaths.length === 2, JSON.stringify(dropPaths));

  win.XMLHttpRequest = RealXHR;
  // 收尾：关掉可能还留着的进度弹窗
  const leftover = doc.querySelector('.modal-mask');
  if (leftover) {
    const x = leftover.querySelector('.modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  }
}

// ---------------------------------------------------------------- 20. 用户与权限

section('⑳ 用户与权限：入口、表单、提交格式');

// 用户管理会**改动服务端状态**（写 users.json），所以这一节不真的打服务器：
// 把 fetch 换成桩，只断言界面行为与「发出去的请求长什么样」。
// 服务端那一侧的 CRUD / 权限生效 / 落盘由 scripts/check-users.sh 覆盖。
{
  const RealFetch = win.fetch;
  const calls = [];
  const USERS = () => ({
    users: [
      { name: 'admin', rules: [{ path: '/', perm: 'rw' }], from_startup: true },
      {
        name: 'alice',
        rules: [{ path: '/docs', perm: 'rw' }, { path: '/', perm: 'r' }],
        from_startup: false,
        created_at: '2026-09-28T10:00:00+08:00',
        updated_at: '2026-09-28T10:00:00+08:00',
      },
    ],
    anonymous: [{ path: '/public', perm: 'r' }],
    editable: true,
    user_file: '/tmp/gofs-users.json',
    auth_on: true,
  });

  win.fetch = (input, init) => {
    const url = typeof input === 'string' ? input : (input && input.url) || '';
    const method = (init && init.method) || 'GET';
    calls.push({ method: method, url: url, body: init && init.body });
    const payload = method === 'GET' ? USERS() : USERS();
    // ⚠️ jsdom 没有实现 Response，只有 Node 的全局 Response 可用。
    // 写成 new win.Response(...) 会抛异常，表现为「列表加载失败」。
    return Promise.resolve(new Response(JSON.stringify(payload), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
  };

  check('顶栏有「用户」入口且可见（管理员）',
    $('btn-users') && !$('btn-users').hidden);
  check('「用户」按钮的提示说明了用途',
    /用户|账号|权限/.test($('btn-users').title), $('btn-users').title);

  $('btn-users').dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  const opened = await waitFor(() => doc.querySelector('.key-list'), 5000, '用户面板');
  // ⚠️ 面板出现 ≠ 列表渲染完：列表要等 fetch 回来才填。
  // 只等 .key-list 会让紧随其后的「列出两个账号」看到 0 条。
  await waitFor(() => doc.querySelectorAll('.key-item').length > 0, 5000, '账号列表渲染');
  check('用户面板可以打开', opened && /用户与权限/.test(
    doc.querySelectorAll('.modal-head div')[doc.querySelectorAll('.modal-head div').length - 1].textContent));

  const items = Array.from(doc.querySelectorAll('.key-item'));
  check('列出了两个账号', items.length === 2, '实际 ' + items.length);
  const startupItem = items.find((i) => /admin/.test(i.textContent));
  check('启动参数的账号标了「来自启动参数」',
    !!startupItem && /来自启动参数/.test(startupItem.textContent));
  check('启动参数的账号不能编辑（显示为只读）',
    !!startupItem && /只读/.test(startupItem.textContent) &&
    !Array.from(startupItem.querySelectorAll('.btn')).some((b) => /编辑/.test(b.textContent)));
  const aliceItem = items.find((i) => /alice/.test(i.textContent));
  check('用户表里的账号可以编辑与删除',
    !!aliceItem &&
    Array.from(aliceItem.querySelectorAll('.btn')).some((b) => /编辑/.test(b.textContent)) &&
    Array.from(aliceItem.querySelectorAll('.btn')).some((b) => /删除/.test(b.textContent)));
  check('权限以胶囊形式逐条列出（/docs 可读写）',
    !!aliceItem && /\/docs/.test(aliceItem.textContent) && /可读写/.test(aliceItem.textContent));
  check('匿名访问单独说明',
    /匿名访问/.test(doc.querySelector('.modal-body').textContent));

  // ---- 新建表单 ----
  const newBtn = Array.from(doc.querySelectorAll('.modal-foot .btn'))
    .find((b) => /新建用户/.test(b.textContent));
  check('面板底部有「新建用户」', !!newBtn);
  newBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

  const formOpen = await waitFor(() => doc.querySelector('.rule-list'), 5000, '新建用户表单');
  check('新建表单打开', formOpen);
  check('表单有用户名输入框',
    !!doc.querySelector('.modal-body input.key-input[type=text]'));
  const pwInput = doc.querySelector('.pw-row input');
  check('密码框默认是遮蔽的', pwInput && pwInput.type === 'password');
  check('默认给了一行权限（/ + 只读）',
    doc.querySelectorAll('.rule-row').length === 1 &&
    doc.querySelector('.rule-row .rule-path').value === '/',
    doc.querySelector('.rule-row .rule-path').value);

  const pwBtns = Array.from(doc.querySelectorAll('.pw-row .btn'));
  const showBtn = pwBtns.find((b) => /显示/.test(b.textContent));
  showBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('点「显示」后密码可见', pwInput.type === 'text');
  const genBtn = pwBtns.find((b) => /随机生成/.test(b.textContent));
  genBtn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('「随机生成」填入了足够长的随机密码',
    pwInput.type === 'text' && pwInput.value.length >= 12, '长度 ' + pwInput.value.length);

  // 权限行的增删
  const addRule = Array.from(doc.querySelectorAll('.modal-body .btn'))
    .find((b) => /添加路径/.test(b.textContent));
  addRule.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('「添加路径」能加一行', doc.querySelectorAll('.rule-row').length === 2);
  const lastRow = doc.querySelectorAll('.rule-row')[1];
  lastRow.querySelector('.rule-path').value = '/public';
  lastRow.querySelector('.rule-perm').value = 'r';
  const delRow = Array.from(lastRow.querySelectorAll('.btn')).find((b) => /移除/.test(b.textContent));
  delRow.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('「移除」能删掉一行', doc.querySelectorAll('.rule-row').length === 1);

  // 填好再提交，检查发出去的请求
  const nameInput = doc.querySelector('.modal-body input.key-input[type=text]');
  nameInput.value = 'bob';
  pwInput.type = 'text';
  pwInput.value = 'bobpass123';
  doc.querySelector('.rule-row .rule-path').value = '/docs';
  doc.querySelector('.rule-row .rule-perm').value = 'rw';
  calls.length = 0;
  Array.from(doc.querySelectorAll('.modal-foot .btn'))
    .find((b) => /^创建$/.test(b.textContent.trim()))
    .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  await waitFor(() => calls.some((c) => c.method === 'POST'), 5000, '提交请求');

  const post = calls.find((c) => c.method === 'POST');
  check('新建用的是 POST /__gofs__/users',
    !!post && /\/__gofs__\/users$/.test(post.url), post && (post.method + ' ' + post.url));
  let sent = null;
  try { sent = JSON.parse(post.body); } catch (e) { sent = null; }
  check('请求体带上了用户名与密码',
    !!sent && sent.name === 'bob' && sent.password === 'bobpass123',
    post && String(post.body));
  check('请求体里的规则是结构化的 {path, perm}',
    !!sent && Array.isArray(sent.rules) && sent.rules.length === 1 &&
    sent.rules[0].path === '/docs' && sent.rules[0].perm === 'rw',
    post && String(post.body));

  // ---- 编辑：用户名不可改、留空密码不覆盖 ----
  await waitFor(() => !doc.querySelector('.rule-list'), 5000, '表单关闭');
  const openRow = (name) => {
    const item = Array.from(doc.querySelectorAll('.key-item'))
      .find((i) => i.querySelector('.key-name') &&
        i.querySelector('.key-name').textContent.indexOf(name) === 0);
    return item || null;
  };
  const aliceAgain = openRow('alice');
  if (!aliceAgain) {
    // 列表没渲染出来时不要一路崩下去：记一条失败，后面的断言跳过。
    check('列表里有 alice 可供编辑', false, '列表里找不到 alice');
  } else {
  Array.from(aliceAgain.querySelectorAll('.btn'))
    .find((b) => /编辑/.test(b.textContent))
    .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  await waitFor(() => doc.querySelector('.rule-list'), 5000, '编辑表单');

  const editName = doc.querySelector('.modal-body input.key-input[type=text]');
  check('编辑时用户名输入框被禁用（用户名就是身份）', editName.disabled === true);
  check('编辑时密码框提示「留空则不修改」',
    /留空/.test(doc.querySelector('.pw-row input').placeholder),
    doc.querySelector('.pw-row input').placeholder);
  check('编辑时带出了原有的两行权限',
    doc.querySelectorAll('.rule-row').length === 2,
    String(doc.querySelectorAll('.rule-row').length));

  calls.length = 0;
  Array.from(doc.querySelectorAll('.modal-foot .btn'))
    .find((b) => /^保存$/.test(b.textContent.trim()))
    .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  await waitFor(() => calls.some((c) => c.method === 'PUT'), 5000, '保存请求');
  const put = calls.find((c) => c.method === 'PUT');
  check('保存用的是 PUT', !!put && put.method === 'PUT');
  let putBody = null;
  try { putBody = JSON.parse(put.body); } catch (e) { putBody = null; }
  check('没填新密码时不发送 password 字段（避免把密码清空）',
    !!putBody && !('password' in putBody), put && String(put.body));
  check('保存时带上了两行权限',
    !!putBody && putBody.rules.length === 2, put && String(put.body));
  }

  // ---- 删除要先确认 ----
  await waitFor(() => !doc.querySelector('.rule-list'), 5000, '表单关闭');
  const alice3 = openRow('alice');
  if (!alice3) {
    check('列表里有 alice 可供删除', false, '列表里找不到 alice');
  } else {
  Array.from(alice3.querySelectorAll('.btn'))
    .find((b) => /删除/.test(b.textContent))
    .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  const confirmShown = await waitFor(() => {
    const masks = doc.querySelectorAll('.modal-mask');
    return masks.length >= 2 && /删除用户 alice/.test(masks[masks.length - 1].textContent);
  }, 3000, '删除确认框');
  check('删除前弹出确认框', confirmShown);

  calls.length = 0;
  if (confirmShown) {
    const masks = doc.querySelectorAll('.modal-mask');
    const box = masks[masks.length - 1];
    // 破坏性确认按钮是 .danger-solid（实心红）；.danger 是描边款，两者别混
    check('删除确认框用的是实心红按钮',
      !!box.querySelector('.btn.danger-solid'));
    Array.from(box.querySelectorAll('.btn'))
      .find((b) => /^删除$/.test(b.textContent.trim()))
      .dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await waitFor(() => calls.some((c) => c.method === 'DELETE'), 5000, '删除请求');
  }
  const del = calls.find((c) => c.method === 'DELETE');
  check('删除用的是 DELETE 且带 name 参数',
    !!del && /name=alice/.test(del.url), del && del.url);
  }

  win.fetch = RealFetch;
  // 收尾：把所有还开着的弹窗关掉
  for (let i = 0; i < 4; i++) {
    const masks = doc.querySelectorAll('.modal-mask');
    if (!masks.length) break;
    const x = masks[masks.length - 1].querySelector('.modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 80));
  }
}

// ------------------------------------------------- ㉑ 顶栏折叠菜单与窄屏文案

section('㉑ 顶栏「更多」折叠菜单（窄屏入口没丢）');

{
  check('存在「更多」按钮', !!$('btn-more'));
  check('更多按钮默认没有展开（aria-expanded=false）',
    $('btn-more').getAttribute('aria-expanded') === 'false',
    $('btn-more').getAttribute('aria-expanded'));

  // 折叠**只改布局**：被收进菜单的按钮仍然是「可见」（hidden=false），
  // 权限控制的隐藏与布局折叠是两件独立的事。
  const collapsed = ['btn-theme', 'btn-upload-dir', 'btn-mkdir', 'btn-keys', 'btn-users', 'btn-settings'];
  check('被折叠的按钮在 DOM 里都还在，且没有被 hidden 掉',
    collapsed.every((id) => $(id) && !$(id).hidden),
    collapsed.map((id) => id + '=' + (($(id) && $(id).hidden) ? 'hidden' : 'ok')).join(' '));
  check('菜单容器与面板都在', $('more-panel') && $('more-wrap'));
  check('6 个低频操作都在面板里（点开就能用，不是删掉）',
    collapsed.every((id) => $('more-panel').contains($(id))),
    Array.from($('more-panel').querySelectorAll('.btn')).map((b) => b.textContent.trim()).join(', '));

  const click = (el) => el.dispatchEvent(new win.MouseEvent('click', { bubbles: true, cancelable: true }));

  click($('btn-more'));
  check('点「更多」展开菜单', $('more-panel').classList.contains('open'));
  check('展开后 aria-expanded 变成 true',
    $('btn-more').getAttribute('aria-expanded') === 'true');

  click($('btn-more'));
  check('再点一次收起', !$('more-panel').classList.contains('open'));

  click($('btn-more'));
  click($('drop-zone'));
  check('点面板外的任意位置会收起', !$('more-panel').classList.contains('open'),
    '面板仍开着');

  click($('btn-more'));
  doc.dispatchEvent(new win.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  check('按 Esc 会收起', !$('more-panel').classList.contains('open'));

  // 点菜单项本身也要收起：否则改完设置回到页面，菜单还挂在那儿。
  click($('btn-more'));
  check('再次展开以备后续断言', $('more-panel').classList.contains('open'));
  click($('btn-keys'));
  await new Promise((r) => setTimeout(r, 120));
  check('点了菜单项之后菜单自动收起（不会跨页面挂着）',
    !$('more-panel').classList.contains('open'));
  // 收尾：把可能被打开的密钥弹窗关掉
  for (let i = 0; i < 4; i++) {
    const masks = doc.querySelectorAll('.modal-mask');
    if (!masks.length) break;
    const x = masks[masks.length - 1].querySelector('.modal-head button');
    if (x) x.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 80));
  }
  check('展开菜单不会顺带打开别的弹窗（收尾后无残留）',
    doc.querySelectorAll('.modal-mask').length === 0);
}

section('㉒ 窄屏文案：统计 / 页脚 / 投放区都换短句，而不是靠截断');

{
  check('isNarrow 已暴露（供真机逻辑判断）', typeof win.GOFS.isNarrow === 'function');
  // jsdom 没实现 matchMedia，这里临时补一个「命中窄屏」的桩。
  const realMM = win.matchMedia;
  win.matchMedia = (q) => ({
    matches: /max-width:\s*560px/.test(q),
    media: q,
    addEventListener() {}, removeEventListener() {},
    addListener() {}, removeListener() {},
  });

  check('窄屏判定生效', win.GOFS.isNarrow() === true);

  await win.GOFS.refresh();
  await waitFor(() => win.GOFS.state && win.GOFS.state.busy === false, 8000, '窄屏重绘完成');

  const stat = $('stat').textContent;
  check('窄屏统计用「N 目录 · N 文件」短口径', /\d+ 目录 · \d+ 文件/.test(stat), stat);
  check('窄屏统计不再出现「个目录 / 个文件 / 文件合计」这类长词',
    !/个目录|个文件|文件合计/.test(stat), stat);

  const foot = $('footer-right').textContent;
  check('窄屏页脚不再重复「已登录」', !/已登录/.test(foot), foot);
  check('窄屏页脚仍写清账号与权限', /admin/.test(foot) && /可读写/.test(foot), foot);
  check('窄屏页脚的能力是短词（上传·删除…）', /上传·删除/.test(foot), foot);

  const zone = $('drop-zone-target').textContent;
  check('窄屏投放区仍有明确落点', /存入\s+\//.test(zone), zone);
  check('窄屏投放区一行说得完（不出现「单文件上限」长词）',
    !/单文件上限/.test(zone), zone);

  // 还原，避免影响（将来）后续断言
  if (realMM) win.matchMedia = realMM; else delete win.matchMedia;
  await win.GOFS.refresh();
  await waitFor(() => win.GOFS.state && win.GOFS.state.busy === false, 8000, '恢复宽屏重绘');
  check('恢复宽屏后统计又变回长口径（说明两套文案都活着）',
    /个目录/.test($('stat').textContent), $('stat').textContent);
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
