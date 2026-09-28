// 用 CDP 直连 headless Chrome，量出目录表格各列的真实边界。
//
// 为什么需要它：表格列宽是 CSS 决定的，`table-layout: fixed` 下列宽不够时
// 内容会画到相邻单元格上（视觉上是「覆盖」）。jsdom 不做布局计算，
// Go 测试更碰不到，只有真跑渲染引擎才看得见。
//
// 用法：
//   1) 先起一个带调试端口的 headless Chrome：
//      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
//        --headless=new --no-sandbox --disable-gpu --no-proxy-server \
//        --user-data-dir=/tmp/chrome-cdp --remote-debugging-port=9333 about:blank
//   2) node scripts/measure-layout.mjs <url> [width] [height]
//      目标开了鉴权时加 MEASURE_AUTH=user:pass
//      想人眼复核排版时加 MEASURE_SHOT=/tmp/shot.png
//      想看弹窗内部时加 MEASURE_OPEN=editor|sheet|settings
//
// 退出码：0 = 布局正常；1 = 列错位 / 按钮溢出；2 = 测量失败。

import { createRequire } from 'node:module';
import { writeFile } from 'node:fs/promises';

const CDP_PORT = process.env.CDP_PORT || '9333';
const TARGET = process.argv[2] || 'http://127.0.0.1:5001/';
const WIDTH = parseInt(process.argv[3] || '1440', 10);
const HEIGHT = parseInt(process.argv[4] || '900', 10);
const MIN_ROWS = parseInt(process.env.MEASURE_MIN_ROWS || '1', 10);
// TOUCH=1 模拟触摸设备（无 hover、粗指针），用于验证手机端交互。
const TOUCH = process.env.TOUCH === '1';
// 触摸目标的最小边长。Apple HIG 建议 44，Material 建议 48，
// 这里取 40 作为「可点」的底线，太小的按钮在手机上按不准。
const MIN_TAP = parseInt(process.env.MIN_TAP || '40', 10);
// MEASURE_OPEN=editor|sheet|settings 时先打开对应的模态界面再测量，
// 这样编辑器和操作菜单里的按钮也能一起纳入触摸可用性检查。
const OPEN = process.env.MEASURE_OPEN || '';
// MEASURE_SHOT=/tmp/x.png 时，在测量前把当前视口截一张图。
// 数字判据能告诉你「有没有坏」，但看不出「长什么样」——改动 UI 后想人眼
// 复核一眼，比在终端里拼数字快得多（尤其排版类改动）。
const SHOT = process.env.MEASURE_SHOT || '';

// Node 自带 fetch 不受 HTTP_PROXY 影响，直连本地调试端口即可。
async function pickTarget() {
  const res = await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`);
  const list = await res.json();
  const page = list.find((t) => t.type === 'page' && t.webSocketDebuggerUrl);
  if (!page) throw new Error('没有可用的 page target');
  return page.webSocketDebuggerUrl;
}

class CDP {
  constructor(url) {
    this.ws = new WebSocket(url);
    this.id = 0;
    this.pending = new Map();
    this.ready = new Promise((resolve, reject) => {
      this.ws.addEventListener('open', () => resolve());
      this.ws.addEventListener('error', (e) => reject(new Error('WebSocket 连接失败: ' + (e.message || '未知'))));
    });
    this.ws.addEventListener('message', (ev) => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) reject(new Error(msg.error.message));
        else resolve(msg.result);
      }
    });
  }

  send(method, params = {}) {
    const id = ++this.id;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error('CDP 超时: ' + method));
        }
      }, 20000);
    });
  }

  async evalJSON(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true,
    });
    if (r.exceptionDetails) {
      throw new Error('页面内异常: ' + (r.exceptionDetails.text || '') + ' ' +
        (r.exceptionDetails.exception ? r.exceptionDetails.exception.description : ''));
    }
    return r.result.value;
  }

  close() { try { this.ws.close(); } catch { /* 忽略 */ } }
}

const MEASURE = `(() => {
  const d = document;
  const rows = Array.from(d.querySelectorAll('#tbody tr'));
  const ths = Array.from(d.querySelectorAll('.listing thead th'));
  const firstTDs = rows.length ? Array.from(rows[0].children) : [];
  const visible = el => el.getBoundingClientRect().width > 0;
  const out = {
    viewport: window.innerWidth,
    bodyW: document.body.clientWidth,
    docW: document.documentElement.clientWidth,
    tableW: Math.round((document.getElementById('table') || {getBoundingClientRect: () => ({width: 0})}).getBoundingClientRect().width),
    // 表头可见列数必须等于数据行的可见单元格数。
    // 只给 th 加 display:none 而漏掉 td，会让 table-layout: fixed
    // 的列宽分配整体错位（实测能把操作列挤到只剩 34px）。
    visibleTH: ths.filter(visible).length,
    visibleTD: firstTDs.filter(visible).length,
    // 页面是否出现横向溢出（手机上很常见：某个元素撑破了视口）。
    overflowX: document.documentElement.scrollWidth - window.innerWidth,
    // 撑破视口的元凶。只保留最靠右的几个，并跳过「子元素比父元素还靠左」的重复项。
    offenders: (() => {
      const list = [];
      for (const el of d.querySelectorAll('body *')) {
        const r = el.getBoundingClientRect();
        if (r.width === 0) continue;
        if (r.right > window.innerWidth + 1) {
          list.push({
            tag: el.tagName.toLowerCase(),
            cls: (el.className || '').toString().split(' ').slice(0, 2).join('.'),
            right: Math.round(r.right), left: Math.round(r.left), w: Math.round(r.width),
            text: (el.textContent || '').trim().slice(0, 12),
          });
        }
      }
      list.sort((a, b) => b.right - a.right);
      return list.slice(0, 5);
    })(),
    // 可点元素的尺寸与可见性。手机上两件事最要命：
    // ① 目标太小按不准；② 依赖 :hover 才显形的东西永远不显形。
    taps: [],
    rows: rows.length,
    cols: ths.map(th => {
      const r = th.getBoundingClientRect();
      return { cls: (th.className.split(' ')[0] || '?'),
               w: Math.round(r.width), left: Math.round(r.left), right: Math.round(r.right) };
    }),
    overlaps: [],
    squeezed: [],
    widest: null,
    maxButtons: 0
  };
  // 收集可点元素的尺寸与不透明度。只看「可见且可点」的那些。
  // 选择器要覆盖全部交互入口：按钮、链接、勾选框、以及文件名的可点文字。
  // 勾选框本身只有十几像素，真正能点中的是它外层的 label，
  // 所以量「可点区域」时要向上找 label，否则会冤枉一堆合格的目标。
  const clickable = d.querySelectorAll(
    '.btn, .op-btn, a[href], input[type=checkbox], .name-cell .label');
  const tapTarget = (el) => (el.type === 'checkbox' || el.type === 'radio')
    ? (el.closest('label') || el)
    : el;
  const seen = new Set();
  for (const el of clickable) {
    const target = tapTarget(el);
    const r = target.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) continue;      // 隐藏的不算
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') continue;
    // 元素自身或被祖先设了低不透明度（典型是「hover 才显示」的操作区）
    let opacity = parseFloat(cs.opacity);
    let node = el.parentElement;
    while (node && node !== d.body) {
      opacity *= parseFloat(getComputedStyle(node).opacity);
      node = node.parentElement;
    }
    const label = (el.getAttribute('title') || el.textContent || el.className || '?').trim().slice(0, 14);
    const sig = label + '@' + Math.round(r.width) + 'x' + Math.round(r.height);
    if (seen.has(sig)) continue;
    seen.add(sig);
    // 分两级要求：按钮类目标必须两个方向都够大；
    // 文字链接的宽度天然由文字决定（「.env」就是很窄），
    // 对它们只要求高度达标 —— 这也符合各家移动端设计规范的做法。
    const strict = el.type === 'checkbox' ||
      el.matches('.btn, .op-btn, .action-item, .check-hit, input[type=checkbox]');
    out.taps.push({
      label: label,
      cls: (target.className || el.className || '').toString().split(' ')[0] || el.tagName.toLowerCase(),
      w: Math.round(r.width), h: Math.round(r.height),
      opacity: Math.round(opacity * 100) / 100,
      strict: strict,
    });
  }

  for (const tr of rows) {
    const ops = tr.querySelector('.ops');
    if (!ops) continue;
    const o = ops.getBoundingClientRect();
    const tm = tr.querySelector('.time-cell');
    const name = tr.dataset.path || '?';

    // 只统计「看得见」的按钮：窄屏下整排按钮被 display:none 收起，
    // 它们的 rect 全是 0，算进去会把间隙也算多，得出「放不下」的假象。
    const btns = Array.from(ops.querySelectorAll('.op-btn')).filter(visible);
    if (btns.length > out.maxButtons) out.maxButtons = btns.length;

    // 按钮的真实占位宽度：用 rect 宽度求和。
    // 注意不要用 scrollWidth —— 对 inline-flex 元素它会低估（不含内边距），
    // 拿它去和容器比会得出「明明放得下却折行了」的错觉。
    let natural = 0;
    for (const b of btns) natural += b.getBoundingClientRect().width;
    const gaps = btns.length > 1 ? (btns.length - 1) * 4 : 0;
    natural = Math.round(natural + gaps);
    // 折行判据用「按钮顶边有几个不同的值」，
    // 不要拿高度除以写死的行高 —— 触摸设备上单个按钮就有 40px 高，
    // 除出来会得到「一行却算成两行」的假象。
    const lines = btns.length
      ? new Set(btns.map((b) => Math.round(b.getBoundingClientRect().top))).size
      : 0;
    if (!out.widest || natural > out.widest.natural) {
      out.widest = {
        row: name,
        count: btns.length,
        natural: natural,
        avail: ops.clientWidth,
        // 直接读盒子几何：clientWidth 在折行场景下可能被舍入得很怪，
        // 用 rect 宽度 + 高度才能判断有没有折行。
        boxW: Math.round(o.width),
        boxH: Math.round(o.height),
        lines: lines,
        labels: btns.map(b => (b.textContent || '').trim()),
      };
    }

    // 关键判据：flex 项目默认 min-width:auto，按钮不会被压缩，
    // 于是超出的部分从容器左边缘溢出（justify-content 是 flex-end）。
    // ① 溢出自己的容器 = 画到了相邻单元格上（最典型的是「修改时间」列）
    // ② 与时间列矩形相交 = 视觉上直接被盖住
    if (btns.length) {
      const first = btns[0].getBoundingClientRect();
      const oLeft = ops.getBoundingClientRect().left;
      const bleedContainer = Math.round(oLeft - first.left);
      let bleedTime = 0;
      if (tm) {
        const t = tm.getBoundingClientRect();
        if (t.width > 0) bleedTime = Math.round(t.right - first.left);
      }
      if (bleedContainer > 1 || bleedTime > 1) {
        out.overlaps.push({
          row: name,
          opsLeft: Math.round(oLeft),
          firstBtnLeft: Math.round(first.left),
          bleedContainer: bleedContainer > 0 ? bleedContainer : 0,
          bleedTime: bleedTime > 0 ? bleedTime : 0,
        });
      }
    }
  }
  return JSON.stringify(out);
})()`;

// openModalUI 按名字打开一个模态界面（编辑器 / 行操作菜单），
// 好让它们内部的按钮也进入触摸可用性检查。
async function openModalUI(cdp, which) {
  const expr = which === 'settings'
    ? `(() => {
         const b = document.getElementById('btn-settings');
         if (!b || b.hidden) return false;
         b.click();
         return true;
       })()`
    : which === 'editor'
      ? `(async () => {
         const tr = document.querySelector('#tbody tr[data-path$=".md"]')
                 || document.querySelector('#tbody tr');
         if (!tr) return false;
         const path = tr.dataset.path;
         window.GOFS.openEditor({ path: path, name: path.split('/').pop(),
                                  size: 999, is_dir: false, editable: true });
         return true;
       })()`
    : `(() => {
         const tr = document.querySelector('#tbody tr');
         if (!tr) return false;
         const b = tr.querySelector('.ops-more') || tr.querySelector('.op-btn');
         if (!b) return false;
         b.click();
         return true;
       })()`;
  try {
    return !!(await cdp.evalJSON(expr));
  } catch {
    return false;
  }
}

async function main() {
  const wsUrl = await pickTarget();
  const cdp = new CDP(wsUrl);
  await cdp.ready;

  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');

  // 目标开了鉴权时，用 MEASURE_AUTH=user:pass 带上 Basic 凭据，
  // 否则页面只会渲染登录框，量不到目录表格。
  const auth = process.env.MEASURE_AUTH;
  if (auth) {
    await cdp.send('Network.enable');
    await cdp.send('Network.setExtraHTTPHeaders', {
      headers: { Authorization: 'Basic ' + Buffer.from(auth).toString('base64') },
    });
  }

  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: WIDTH, height: HEIGHT, deviceScaleFactor: 1, mobile: false,
  });

  if (TOUCH) {
    // 同时打开触摸模拟与媒体特性覆盖：前者让事件走触摸路径，
    // 后者让 `@media (hover: none)` / `(pointer: coarse)` 生效。
    await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
    await cdp.send('Emulation.setEmulatedMedia', {
      features: [
        { name: 'hover', value: 'none' },
        { name: 'pointer', value: 'coarse' },
        { name: 'any-hover', value: 'none' },
        { name: 'any-pointer', value: 'coarse' },
      ],
    });
  }

  await cdp.send('Page.navigate', { url: TARGET });

  // 目录是异步拉取的，轮询等到表格渲染出来。
  const deadline = Date.now() + 15000;
  let rendered = false;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 250));
    try {
      const n = await cdp.evalJSON('document.querySelectorAll("#tbody tr").length');
      if (n >= MIN_ROWS) { rendered = true; break; }
    } catch { /* 导航期间忽略 */ }
  }
  if (!rendered) {
    console.log('  目录未渲染（可能还在登录框），无法测量');
    cdp.close();
    process.exit(1);
  }
  // 让样式与字体稳定后再量。
  await new Promise((r) => setTimeout(r, 400));

  if (OPEN) {
    const ok = await openModalUI(cdp, OPEN);
    if (!ok) console.log('  （未能打开 ' + OPEN + '，仍按当前页面测量）');
    await new Promise((r) => setTimeout(r, 1000));
  }

  if (SHOT) {
    const shot = await cdp.send('Page.captureScreenshot', { format: 'png', fromSurface: true });
    await writeFile(SHOT, Buffer.from(shot.data, 'base64'));
    console.log('已截图 ' + SHOT);
  }

  const m = JSON.parse(await cdp.evalJSON(MEASURE));
  cdp.close();

  console.log('视口 ' + m.viewport + 'px（文档 ' + m.docW + 'px，body ' + m.bodyW +
    'px，表格 ' + m.tableW + 'px）');

  let bad = 0;

  // 列数一致性先查 —— 它错了，后面的列宽判断都没意义。
  if (m.visibleTH !== m.visibleTD) {
    bad++;
    console.log('\u001b[31m表头列数与数据列数不一致：表头 ' + m.visibleTH +
      ' 列，数据 ' + m.visibleTD + ' 列（只隐藏了 th 没隐藏 td，列宽会整体错位）\u001b[0m');
  } else {
    console.log('\u001b[32m表头与数据列数一致（' + m.visibleTH + ' 列）\u001b[0m');
  }

  console.log('列宽：');
  for (const c of m.cols) {
    console.log('  ' + c.cls.padEnd(12) + ' width=' + String(c.w).padStart(4) +
      '  [' + String(c.left).padStart(4) + ' … ' + String(c.right).padStart(4) + ']');
  }
  console.log('行数 ' + m.rows + '；单行最多 ' + m.maxButtons + ' 个操作按钮');
  if (m.widest) {
    console.log('最宽一行：' + m.widest.row + '（' + m.widest.count + ' 个：' +
      m.widest.labels.join(' / ') + '）');
    console.log('  按钮自然宽度合计 ' + m.widest.natural + 'px，容器实际宽 ' +
      m.widest.boxW + 'px，高 ' + m.widest.boxH + 'px');
    if (m.widest.lines > 1) {
      console.log('  \u001b[33m注意：按钮折成了 ' + m.widest.lines + ' 行（不覆盖，但行高变大）\u001b[0m');
    }
  }

  if (m.overlaps.length) {
    bad += m.overlaps.length;
    console.log('\n\u001b[31m操作按钮溢出容器（' + m.overlaps.length + ' 行）：\u001b[0m');
    for (const o of m.overlaps.slice(0, 6)) {
      const parts = [];
      if (o.bleedContainer) parts.push('溢出容器 ' + o.bleedContainer + 'px');
      if (o.bleedTime) parts.push('压住「修改时间」列 ' + o.bleedTime + 'px');
      console.log('  ' + o.row + '  容器左边界=' + o.opsLeft +
        '  第一个按钮左边界=' + o.firstBtnLeft + '  → ' + parts.join('，'));
    }
  } else {
    console.log('\n\u001b[32m操作列未溢出，与「修改时间」列无重叠\u001b[0m');
  }

  if (m.squeezed.length) {
    bad += m.squeezed.length;
    console.log('\u001b[31m按钮被压缩到内容之外（' + m.squeezed.length + ' 行）：\u001b[0m');
    for (const s of m.squeezed.slice(0, 6)) {
      console.log('  ' + s.row + '  ' + s.count + ' 个按钮需 ' + s.natural +
        'px / 可用 ' + s.avail + 'px');
    }
  } else {
    console.log('\u001b[32m所有操作按钮都未被压缩\u001b[0m');
  }

  // ---- 触摸可用性 ----

  if (m.overflowX > 1) {
    bad++;
    console.log('\u001b[31m页面横向溢出 ' + m.overflowX + 'px（有元素撑破了视口）\u001b[0m');
    for (const o of m.offenders) {
      console.log('  ' + (o.tag + '.' + o.cls).padEnd(26) + ' right=' + String(o.right).padStart(4) +
        '  宽=' + String(o.w).padStart(4) + '  ' + o.text);
    }
  } else {
    console.log('\u001b[32m无横向溢出\u001b[0m');
  }

  const small = m.taps.filter((t) => (t.strict
    ? Math.min(t.w, t.h) < MIN_TAP
    : t.h < MIN_TAP));
  const faded = m.taps.filter((t) => t.opacity < 0.9);

  // 触摸目标尺寸与「hover 才显示」只在触摸设备上是问题：
  // 鼠标设备上 32px 的图标按钮和 hover 显隐都是合理设计，
  // 用同一套阈值去卡会一直误报。
  if (TOUCH) {
    console.log('可点目标 ' + m.taps.length + ' 个（最小边 < ' + MIN_TAP + 'px 的有 ' +
      small.length + ' 个；不透明度 < 0.9 的有 ' + faded.length + ' 个）');

    if (small.length) {
      bad++;
      console.log('\u001b[31m触摸目标过小（按不准）（按钮类要求两维 ≥ ' + MIN_TAP +
        'px，文字链接只要求高度）：\u001b[0m');
      for (const t of small.slice(0, 10)) {
        console.log('  ' + t.cls.padEnd(14) + ' ' + (t.w + '×' + t.h).padEnd(9) +
          (t.strict ? '[按钮] ' : '[链接] ') + t.label);
      }
    }

    if (faded.length) {
      bad++;
      console.log('\u001b[31m可点元素处于半透明（触摸设备上没有 hover，会一直看不清）：\u001b[0m');
      for (const t of faded.slice(0, 10)) {
        console.log('  ' + t.cls.padEnd(14) + ' opacity=' + t.opacity + '  ' + t.label);
      }
    }
  } else {
    console.log('（鼠标设备：跳过触摸目标尺寸检查，加 TOUCH=1 可启用）');
  }

  process.exit(bad === 0 ? 0 : 1);
}

main().catch((e) => {
  console.error('测量失败: ' + e.message);
  process.exit(2);
});
