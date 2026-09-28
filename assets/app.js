/* gofs 前端交互：目录浏览、上传、删除、重命名、打包下载、压缩包预览与在线解压。
 * 无任何外部依赖，所有元素均通过 DOM API 构建以避免 XSS。 */
(function () {
  'use strict';

  // G 是跨文件共享的命名空间：editor.js / keys.js 会往上面挂自己的入口，
  // 同时复用这里的请求封装、Toast 与弹窗组件。
  var G = window.GOFS = window.GOFS || {};

  // ---------------------------------------------------------------- 基础

  var DATA = {};
  try {
    DATA = JSON.parse(document.getElementById('gofs-data').textContent || '{}');
  } catch (e) {
    DATA = {};
  }

  var BASE = DATA.path_prefix || '';
  var ASSETS = BASE + '/__gofs__/assets/';
  var API = BASE + '/__gofs__/extract';

  // ------------------------------------------------------------ 权限与配置
  //
  // 这些值会随登录状态变化：未登录时服务端只下发一张空壳（权限位全 false、
  // 配置项不下发），登录后才补齐。因此必须每次从 DATA 实时读取，
  // 绝不能像 BASE 那样在模块加载时做一次性快照 ——
  // 否则登录后「上传 / 编辑 / 删除 / 解压 / 重命名 / 密钥」这些入口会全部消失，
  // 只剩「下载」，看起来就像这些功能根本没实现。
  function can(name) { return !!(DATA.perms && DATA.perms[name]); }
  function uploadDated() { return !!DATA.upload_dated; }
  function uploadDateDir() { return DATA.upload_date_dir || ''; }
  function editMax() { return DATA.edit_max_size || 0; }
  function allowKeys() { return !!DATA.allow_keys; }
  function allowSettings() { return !!DATA.allow_settings; }
  // allowUsers 表示当前账号能在页面上增删改用户。服务端在下发页面/登录响应时
  // 就已经按「功能开关 + 是否启用鉴权 + 是否管理员」算好了。
  function allowUsers() { return !!DATA.allow_users; }
  // uploadMax 返回单文件上传上限（字节），0 表示不限制。
  function uploadMax() { return typeof DATA.upload_max_size === 'number' ? DATA.upload_max_size : 0; }

  // applySession 把登录后拿到的权限与配置合并进 DATA。
  //
  // 必须**原地合并** DATA.perms 而不是整个替换：其他模块（以及本文件的
  // can()）都通过 DATA.perms 这个引用读取权限，替换对象会让它们读到旧值。
  function applySession(info) {
    if (info.perms && typeof info.perms === 'object') {
      DATA.perms = DATA.perms || {};
      for (var k in info.perms) {
        if (Object.prototype.hasOwnProperty.call(info.perms, k)) {
          DATA.perms[k] = !!info.perms[k];
        }
      }
    }
    if (typeof info.upload_dated === 'boolean') DATA.upload_dated = info.upload_dated;
    if (typeof info.upload_date_dir === 'string') DATA.upload_date_dir = info.upload_date_dir;
    if (typeof info.allow_keys === 'boolean') DATA.allow_keys = info.allow_keys;
    if (typeof info.allow_settings === 'boolean') DATA.allow_settings = info.allow_settings;
    // allow_users 必须在这里补齐：未登录外壳里刻意不下发它，
    // 漏了这一行就表现为「登录后仍然看不到『用户』入口」。
    if (typeof info.allow_users === 'boolean') DATA.allow_users = info.allow_users;
    if (typeof info.upload_max_size === 'number') DATA.upload_max_size = info.upload_max_size;
    if (typeof info.edit_max_size === 'number') DATA.edit_max_size = info.edit_max_size;
    if (info.user !== undefined) DATA.user = info.user;
    if (typeof info.auth_on === 'boolean') DATA.auth_on = info.auth_on;
  }

  // canEdit 判断条目能否在线编辑：类型、权限、体积三重条件。
  // 服务端只按文件名判定 editable，体积上限在这里叠加。
  function canEdit(e) {
    if (!e || e.is_dir || !e.editable) return false;
    if (!can('edit')) return false;
    var max = editMax();
    if (max > 0 && e.size > max) return false;
    return true;
  }

  // archiveAt 判断「在这个目录上传」会不会按日期归档。
  //
  // 只有站在**服务根目录**上传才归档（/pic.jpg → /2026/09/28/pic.jpg）；
  // 进了子目录就原地放。理由：子目录本身通常已经是有意义的分层
  // （项目名、月份、业务线…），再套一层年/月/日 只会越陷越深。
  //
  // 判断必须与服务端 applyUploadLayout 完全一致，否则界面提示与实际落盘不符 ——
  // 那比不提示更糟。服务端的判据是「目标文件的父目录就是根」。
  function archiveAt(basePath) {
    return uploadDated() && !!uploadDateDir() && (basePath || '/') === '/';
  }

  // uploadTargetDir 返回上传文件实际会落到哪个目录（仅用于界面提示）。
  // 真正的路径改写由服务端完成，前端只负责展示，避免两端算法不一致。
  function uploadTargetDir(basePath) {
    if (!archiveAt(basePath)) return basePath;
    return joinPath(basePath, uploadDateDir());
  }

  // isNarrow 判断当前是不是手机宽度（与 CSS 的 560px 断点保持一致）。
  //
  // 有几处文案必须在窄屏换一种说法，而不是靠 CSS 截断：
  //   「3 个目录 · 8 个文件 · 文件合计 54 KiB」在 375px 下会折成 3 行；
  //   「可上传 · 可删除 · 可在线编辑 · 可在线解压」同理。
  // 换口号比换字号有效得多。
  //
  // ⚠️ jsdom 没有实现 window.matchMedia，直接调用会抛异常并把整页逻辑带崩，
  // 所以这里必须判空。缺失时按「宽屏」处理 —— 那也是文案更完整的那个分支。
  function isNarrow() {
    return !!(window.matchMedia && window.matchMedia('(max-width: 560px)').matches);
  }

  var state = {
    path: DATA.path || '/',
    query: DATA.query || '',
    name: DATA.name || '/',
    entries: (DATA.listing && DATA.listing.entries) || [],
    // listing 保存服务端返回的完整目录信息，统计口径以它为准：
    // 里面带 total_all / truncated / dir_count / file_size，
    // 这些是「把 entries 再数一遍」算不出来的（尤其截断与隐藏文件的情况）。
    listing: DATA.listing || null,
    selected: new Set(),
    sortKey: 'name',
    sortDir: 1,
    busy: false
  };

  // pendingNav 保存「在导航途中又被请求的那一次导航」（只留最后一次）。
  // 见 navigate 里的说明：并发点击不能被静默丢掉。
  var pendingNav = null;

  var $ = function (id) { return document.getElementById(id); };

  // ================================================================ 凭据管理
  //
  // 页面使用 HTTP Basic 认证，但刻意不依赖浏览器原生登录框：
  // 原生框无法登出、无法承载跳转、移动端体验差，且一旦被浏览器缓存，
  // 退出后必须重启浏览器才失效。这里由应用自己弹出登录框，
  // 把凭据存在 sessionStorage（默认）或 localStorage（勾选「记住」），
  // 每次请求通过 Authorization 头带上。

  var STORE_SESSION = 'gofs-auth';
  var STORE_PERSIST = 'gofs-auth-remember';
  var creds = null;

  // b64encode 支持非 ASCII 凭据（用户名或密码含中文时 btoa 会直接抛错）。
  function b64encode(s) {
    return btoa(unescape(encodeURIComponent(s)));
  }

  function loadCreds() {
    var raw = null;
    try {
      raw = sessionStorage.getItem(STORE_SESSION) || localStorage.getItem(STORE_PERSIST);
    } catch (e) { raw = null; }
    if (!raw) return null;
    try {
      var o = JSON.parse(raw);
      if (o && typeof o.u === 'string' && typeof o.p === 'string') {
        return { user: o.u, pass: o.p };
      }
    } catch (e) { /* 数据损坏，当作未登录 */ }
    return null;
  }

  function saveCreds(user, pass, remember) {
    creds = { user: user, pass: pass };
    var raw = JSON.stringify({ u: user, p: pass });
    try {
      sessionStorage.setItem(STORE_SESSION, raw);
      if (remember) localStorage.setItem(STORE_PERSIST, raw);
      else localStorage.removeItem(STORE_PERSIST);
    } catch (e) { /* 隐私模式下可能写入失败，内存里仍然有效 */ }
  }

  function clearCreds() {
    creds = null;
    try {
      sessionStorage.removeItem(STORE_SESSION);
      localStorage.removeItem(STORE_PERSIST);
    } catch (e) { /* 忽略 */ }
  }

  // authHeaders 生成带凭据的请求头。
  // X-Gofs-Ajax 告诉服务端「这是应用内部请求，401 时不要发挑战头」，
  // 否则浏览器会弹出脱离应用上下文的原生登录框。
  function authHeaders(extra) {
    var h = {};
    if (extra) { for (var k in extra) { if (Object.prototype.hasOwnProperty.call(extra, k)) h[k] = extra[k]; } }
    h['X-Gofs-Ajax'] = '1';
    if (creds) h['Authorization'] = 'Basic ' + b64encode(creds.user + ':' + creds.pass);
    return h;
  }

  // apiFetch 是所有请求的统一入口：自动带凭据，401 时转交登录流程。
  async function apiFetch(url, opts) {
    opts = opts || {};
    opts.headers = authHeaders(opts.headers);
    var res;
    try {
      res = await fetch(url, opts);
    } catch (e) {
      throw new Error('网络错误：' + e.message);
    }
    if (res.status === 401) {
      clearCreds();
      showLogin('登录已失效，请重新登录');
      throw new Error('未登录');
    }
    return res;
  }

  // ================================================================ 登录界面

  var loginResolve = null;

  function showLogin(message) {
    if ($('login-mask')) {
      if (message) $('login-error').textContent = message;
      return;
    }

    var mask = document.createElement('div');
    mask.className = 'login-mask';
    mask.id = 'login-mask';

    var card = document.createElement('form');
    card.className = 'login-card';
    card.autocomplete = 'on';
    card.onsubmit = function (e) { e.preventDefault(); submit(); };

    var logo = document.createElement('div');
    logo.className = 'login-logo';
    logo.innerHTML = '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>';
    card.appendChild(logo);

    var h = document.createElement('h2');
    h.textContent = '登录 gofs';
    card.appendChild(h);

    var sub = document.createElement('p');
    sub.className = 'login-sub';
    sub.textContent = '使用 HTTP Basic 认证，凭据仅保存在本机浏览器中';
    card.appendChild(sub);

    function field(labelText, type, name, autocomplete) {
      var wrap = document.createElement('label');
      wrap.className = 'login-field';
      var lb = document.createElement('span');
      lb.textContent = labelText;
      wrap.appendChild(lb);
      var input = document.createElement('input');
      input.type = type;
      input.name = name;
      input.autocomplete = autocomplete;
      input.required = true;
      wrap.appendChild(input);
      card.appendChild(wrap);
      return input;
    }

    var userInput = field('用户名', 'text', 'username', 'username');
    var passInput = field('密码', 'password', 'password', 'current-password');

    var rememberWrap = document.createElement('label');
    rememberWrap.className = 'login-remember';
    var remember = document.createElement('input');
    remember.type = 'checkbox';
    var rememberText = document.createElement('span');
    rememberText.textContent = '记住我（保存在本机，适合个人设备）';
    rememberWrap.appendChild(remember);
    rememberWrap.appendChild(rememberText);
    card.appendChild(rememberWrap);

    var err = document.createElement('div');
    err.className = 'login-error';
    err.id = 'login-error';
    if (message) err.textContent = message;
    card.appendChild(err);

    var submitBtn = document.createElement('button');
    submitBtn.type = 'submit';
    submitBtn.className = 'btn primary login-submit';
    submitBtn.id = 'login-submit';
    submitBtn.textContent = '登录';
    card.appendChild(submitBtn);

    var hint = document.createElement('div');
    hint.className = 'login-hint';
    hint.textContent = '账号由启动参数 --auth 配置，例如 --auth admin:123@/:rw';
    card.appendChild(hint);

    mask.appendChild(card);
    document.body.appendChild(mask);
    userInput.focus();

    function setBusy(busy) {
      submitBtn.disabled = busy;
      submitBtn.textContent = busy ? '验证中…' : '登录';
    }

    async function submit() {
      var u = userInput.value.trim();
      var pw = passInput.value;
      err.textContent = '';
      if (!u) { err.textContent = '请输入用户名'; userInput.focus(); return; }

      setBusy(true);
      var res;
      try {
        res = await fetch(BASE + '/__gofs__/auth?path=' + encodeURIComponent(state.path || '/'), {
          method: 'GET',
          headers: {
            'Authorization': 'Basic ' + b64encode(u + ':' + pw),
            'X-Gofs-Ajax': '1'
          }
        });
      } catch (e) {
        setBusy(false);
        err.textContent = '无法连接服务：' + e.message;
        return;
      }

      var data = {};
      try { data = await res.json(); } catch (e) { data = {}; }

      if (!res.ok) {
        setBusy(false);
        err.textContent = data.message || ('登录失败（HTTP ' + res.status + '）');
        passInput.select();
        return;
      }

      saveCreds(u, pw, remember.checked);
      data.user = data.user || u;
      // 合并服务端下发的权限与配置；这一步必须发生在渲染之前，
      // 否则按钮显隐会停留在未登录时的状态。
      applySession(data);
      DATA.auth_required = false;
      mask.remove();
      updateAuthChip();
      toast('ok', '欢迎回来，' + (DATA.user || u));
      await onAuthenticated();
    }

    loginResolve = submit;
  }

  function hideLogin() {
    var m = $('login-mask');
    if (m) m.remove();
  }

  // onAuthenticated 在登录成功后补齐页面数据。
  // 未登录时服务端只返回一张空壳，任何文件信息都要自己拉。
  async function onAuthenticated() {
    renderPerms();
    return navigate(state.path || '/', false);
  }

  // resumeSession 用本地保存的凭据换取真实的身份与权限。
  //
  // 为什么必须主动做这一步：刷新页面时浏览器**不会**自动给页面请求带上
  // Authorization 头（JS 往 storage 里写凭据，浏览器并不知情），服务端只能
  // 返回一张空壳——里面既没有用户名，权限位也全是 false。
  // 不补这一次请求的话，界面就会退化成「匿名」，登出按钮也会消失。
  async function resumeSession() {
    var path = state.path || '/';
    var res;
    try {
      res = await fetch(BASE + '/__gofs__/auth?path=' + encodeURIComponent(path), {
        headers: authHeaders()
      });
    } catch (e) {
      return false;
    }
    if (res.status === 401) {
      clearCreds();
      DATA.user = '';
      return false;
    }
    if (!res.ok) return false;

    var info = {};
    try {
      info = await res.json();
    } catch (e) {
      return false;
    }
    // 服务端未回传用户名时，退回本地凭据里的那个。
    if (!info.user && creds) info.user = creds.user;
    applySession(info);
    DATA.auth_required = false;
    return true;
  }

  function updateAuthChip() {
    var chip = $('user-chip');
    if (!DATA.auth_on) { chip.hidden = true; return; }
    chip.hidden = false;
    chip.textContent = '';
    var span = document.createElement('span');
    span.textContent = DATA.user || '匿名';
    chip.appendChild(span);
    if (DATA.user) {
      var out = document.createElement('button');
      out.type = 'button';
      out.className = 'user-logout';
      out.title = '退出登录';
      out.textContent = '退出';
      out.onclick = function () {
        clearCreds();
        DATA.user = '';
        DATA.auth_required = true;
        toast('info', '已退出登录');
        location.reload();
      };
      chip.appendChild(out);
    }
  }

  // progressToast 返回一个不会自动消失的提示，用于展示下载进度。
  function progressToast(title) {
    var el = document.createElement('div');
    el.className = 'toast info';
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.innerHTML = '<path d="M12 5v14"/><path d="m6 13 6 6 6-6"/>';
    el.appendChild(svg);
    var m = document.createElement('div');
    m.className = 'msg';
    m.textContent = title;
    el.appendChild(m);
    $('toasts').appendChild(el);
    var killed = false;
    var sp = makeSpeedometer();
    return {
      update: function (loaded, total) {
        if (killed) return;
        var pct = total > 0 ? Math.round((loaded / total) * 100) : 0;
        var s = sp.sample(loaded, total);
        m.textContent = title + '　' + fmtSize(loaded) + ' / ' + fmtSize(total) +
          '（' + pct + '%）' + speedSuffix(s);
      },
      done: function (msg) {
        if (killed) return;
        killed = true;
        el.className = 'toast ok';
        svg.innerHTML = '<path d="M20 6 9 17l-5-5"/>';
        m.textContent = msg;
        setTimeout(function () {
          el.style.transition = 'opacity .2s';
          el.style.opacity = '0';
          setTimeout(function () { el.remove(); }, 220);
        }, 2600);
      },
      fail: function (msg) {
        if (killed) return;
        killed = true;
        el.className = 'toast err';
        svg.innerHTML = '<circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16h.01"/>';
        m.textContent = msg;
        setTimeout(function () {
          el.style.transition = 'opacity .2s';
          el.style.opacity = '0';
          setTimeout(function () { el.remove(); }, 220);
        }, 6000);
      }
    };
  }

  // downloadEntry 下载单个文件并显示进度。
  function downloadEntry(e) {
    var t = progressToast('下载 ' + e.name);
    downloadFile(fileURL(e.path), e.name, function (loaded, total) {
      t.update(loaded, total);
    }).then(function () {
      t.done('已下载 ' + e.name);
    }).catch(function (err) {
      t.fail('下载失败：' + err.message);
    });
  }

  // 对每一段分别编码，保留 / 分隔符。
  function encPath(p) {
    return String(p).split('/').map(encodeURIComponent).join('/');
  }
  function raw(p) { return BASE + p; }
  function fileURL(p) { return raw(encPath(p)); }

  // dirURL 返回目录的规范 URL：**恰好一个**结尾斜杠。
  //
  // 不能图省事写 `fileURL(p) + '/'`：根目录 p="/" 时 fileURL 已经是 "/"，
  // 拼出来是 "//" —— 浏览器把它当「协议相对 URL」（scheme-relative），
  // 解析结果是 http: 加一个空主机，于是
  //   history.pushState(..., '//')
  // 直接抛 `A history state object with URL 'http:' cannot be created`。
  // 症状很误导：目录其实已经取回来了，却弹出「打开目录失败」。
  function dirURL(p) {
    var u = fileURL(p);
    return u.charAt(u.length - 1) === '/' ? u : u + '/';
  }

  function joinPath(dir, name) {
    return (dir === '/' ? '' : dir.replace(/\/+$/, '')) + '/' + name;
  }

  // ---------------------------------------------------------------- 格式化

  function fmtSize(n) {
    if (n === null || n === undefined || n < 0) return '—';
    if (n < 1024) return n + ' B';
    var units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    var v = n, i = -1;
    do { v /= 1024; i++; } while (v >= 1024 && i < units.length - 1);
    return (v < 10 ? v.toFixed(1) : Math.round(v)) + ' ' + units[i];
  }

  function fmtTime(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '—';
    function p(x) { return String(x).padStart(2, '0'); }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
      ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function fmtDuration(ms) {
    if (ms < 1000) return ms + ' ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
    return Math.floor(ms / 60000) + ' 分 ' + Math.round((ms % 60000) / 1000) + ' 秒';
  }

  // fmtEta 把剩余秒数写成人类的说法。
  function fmtEta(sec) {
    if (!(sec > 0) || !isFinite(sec)) return '';
    if (sec < 1) return '不到 1 秒';
    if (sec < 60) return Math.ceil(sec) + ' 秒';
    if (sec < 3600) return Math.floor(sec / 60) + ' 分 ' + Math.round(sec % 60) + ' 秒';
    return Math.floor(sec / 3600) + ' 小时 ' + Math.round((sec % 3600) / 60) + ' 分';
  }

  // makeSpeedometer 估算瞬时速率与剩余时间。
  //
  // 用指数平滑，而不是「已传字节 / 已耗时」：后者在速度变化时严重滞后 ——
  // 一开始慢、后来快的情况，它会把平均值一直拉在低位，看着像卡住了。
  // 平滑系数 0.35 是实测下来的折中：再大抖动明显，再小反应太慢。
  function makeSpeedometer() {
    var speed = 0;
    var lastT = Date.now();
    var lastB = 0;
    return {
      sample: function (bytes, total) {
        var now = Date.now();
        var dt = (now - lastT) / 1000;
        if (dt >= 0.35) {
          var inst = (bytes - lastB) / dt;
          if (inst >= 0) speed = speed > 0 ? speed * 0.65 + inst * 0.35 : inst;
          lastT = now;
          lastB = bytes;
        }
        return {
          bps: speed,
          // 速率没稳下来之前不报剩余时间，否则会闪出一串离谱的数字。
          eta: (speed > 4096 && total > bytes) ? (total - bytes) / speed : 0
        };
      },
      reset: function () { speed = 0; lastT = Date.now(); lastB = 0; }
    };
  }

  // speedSuffix 生成「· 1.2 MiB/s · 剩余 8 秒」这一段。
  function speedSuffix(s) {
    if (!s || !(s.bps > 0)) return '';
    var out = '　·　' + fmtSize(s.bps) + '/s';
    if (s.eta > 0) out += '　·　剩余 ' + fmtEta(s.eta);
    return out;
  }

  // ---------------------------------------------------------------- 图标

  var ICON_PATHS = {
    dir: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
    file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/>',
    zip: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/><path d="M12 10v1M12 13v1M12 16v1"/>',
    image: '<rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="9" cy="10" r="2"/><path d="m4 18 5-5 4 4 3-3 4 4"/>',
    code: '<path d="m9 8-5 4 5 4"/><path d="m15 8 5 4-5 4"/>',
    video: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m10 9 5 3-5 3z"/>',
    audio: '<path d="M9 18V6l10-2v12"/><circle cx="6" cy="18" r="3"/><circle cx="16" cy="16" r="3"/>',
    pdf: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/><path d="M9 13h6M9 17h4"/>'
  };

  function svgIcon(kind) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.classList.add('ico');
    svg.innerHTML = ICON_PATHS[kind] || ICON_PATHS.file;
    return svg;
  }

  function kindOf(entry) {
    if (entry.is_dir) return 'dir';
    if (entry.archive) return 'zip';
    var ext = entry.ext || '';
    if (/^(png|jpe?g|gif|webp|svg|bmp|ico|avif|heic)$/.test(ext)) return 'image';
    if (/^(mp4|mov|mkv|avi|webm|flv|m4v)$/.test(ext)) return 'video';
    if (/^(mp3|wav|flac|aac|ogg|m4a)$/.test(ext)) return 'audio';
    if (/^(pdf)$/.test(ext)) return 'pdf';
    if (/^(go|js|ts|jsx|tsx|java|py|rb|rs|c|h|cpp|cs|php|sh|sql|json|ya?ml|xml|html|css|toml|ini|conf|md)$/.test(ext)) return 'code';
    return 'file';
  }

  var OP_ICONS = {
    download: '<path d="M12 5v14"/><path d="m6 13 6 6 6-6"/><path d="M4 20h16"/>',
    unzip: '<path d="M4 6a2 2 0 0 1 2-2h5l2 2h5a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z"/><path d="M12 10v6"/><path d="m9 13 3 3 3-3"/>',
    list: '<path d="M8 6h13M8 12h13M8 18h13"/><circle cx="3.5" cy="6" r="1"/><circle cx="3.5" cy="12" r="1"/><circle cx="3.5" cy="18" r="1"/>',
    rename: '<path d="M4 20h4l10-10-4-4L4 16z"/><path d="m14 6 4 4"/>',
    trash: '<path d="M4 7h16"/><path d="M9 7V5h6v2"/><path d="M6 7l1 12h10l1-12"/>',
    open: '<path d="M14 4h6v6"/><path d="M20 4 10 14"/><path d="M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4"/>',
    edit: '<path d="M4 20h4l10.5-10.5a2.1 2.1 0 0 0-3-3L5 17z"/><path d="M13.5 6.5 17.5 10.5"/>',
    more: '<circle cx="5" cy="12" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="19" cy="12" r="1.7"/>'
  };

  function makeOpBtn(kind, label, cls) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'op-btn' + (cls ? ' ' + cls : '');
    // data-kind 是给 CSS 用的：每类操作有各自的语义色
    // （下载青、编辑紫、解压橙、重命名绿、删除红…），
    // 一眼就能分辨，不用逐个去读文字。
    b.dataset.kind = kind;
    b.title = label;
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.innerHTML = OP_ICONS[kind] || OP_ICONS.list;
    b.appendChild(svg);
    var sp = document.createElement('span');
    sp.textContent = label;
    b.appendChild(sp);
    return b;
  }

  // ---------------------------------------------------------------- Toast

  // toast 支持可选的行动按钮，例如上传完成后提供「查看」跳转。
  function toast(kind, msg, ms, action) {
    var icons = {
      ok: '<path d="M20 6 9 17l-5-5"/>',
      err: '<circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16h.01"/>',
      warn: '<path d="M10.3 4 2.6 17a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 4a2 2 0 0 0-3.4 0z"/><path d="M12 9v4M12 17h.01"/>',
      info: '<circle cx="12" cy="12" r="9"/><path d="M12 16v-5M12 8h.01"/>'
    };
    var el = document.createElement('div');
    el.className = 'toast ' + kind;
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.innerHTML = icons[kind] || icons.info;
    el.appendChild(svg);
    var m = document.createElement('div');
    m.className = 'msg';
    m.textContent = msg;
    el.appendChild(m);
    var dismiss = function () {
      el.style.transition = 'opacity .2s, transform .2s';
      el.style.opacity = '0';
      el.style.transform = 'translateX(12px)';
      setTimeout(function () { el.remove(); }, 220);
    };
    if (action) {
      var btnEl = document.createElement('button');
      btnEl.type = 'button';
      btnEl.className = 'toast-action';
      btnEl.textContent = action.label;
      btnEl.onclick = function () { dismiss(); action.onClick(); };
      el.appendChild(btnEl);
    }
    $('toasts').appendChild(el);
    setTimeout(dismiss, ms || (kind === 'err' ? 7000 : (action ? 9000 : 3200)));
  }

  // ---------------------------------------------------------------- 弹窗

  function openModal(opts) {
    var mask = document.createElement('div');
    mask.className = 'modal-mask';

    var box = document.createElement('div');
    box.className = 'modal' + (opts.wide ? ' wide' : '') + (opts.full ? ' full' : '');

    var head = document.createElement('div');
    head.className = 'modal-head';
    var t = document.createElement('div');
    t.textContent = opts.title || '';
    head.appendChild(t);
    var x = document.createElement('button');
    x.type = 'button';
    x.className = 'btn icon ghost';
    x.innerHTML = '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18"/></svg>';
    x.onclick = close;
    head.appendChild(x);
    box.appendChild(head);

    var body = document.createElement('div');
    body.className = 'modal-body';
    if (typeof opts.body === 'string') body.innerHTML = opts.body;
    else if (opts.body) body.appendChild(opts.body);
    box.appendChild(body);

    if (opts.footer) {
      var foot = document.createElement('div');
      foot.className = 'modal-foot';
      if (typeof opts.footer === 'string') foot.innerHTML = opts.footer;
      else if (opts.footer) foot.appendChild(opts.footer);
      box.appendChild(foot);
    }

    mask.appendChild(box);
    mask.addEventListener('mousedown', function (e) {
      if (e.target === mask && !opts.locked) close();
    });
    // locked 的弹窗（上传进度、解压进度）不允许用 ESC 关掉：
    // 关掉只是把界面藏起来，后台任务还在跑，用户会以为已经取消。
    var escHandler = function (e) {
      if (e.key === 'Escape' && !opts.locked) close();
    };
    document.addEventListener('keydown', escHandler);

    // closing 防止递归：beforeClose 里再次调用 close 时直接放行。
    var closing = false;

    function close() {
      if (closing) return;
      // beforeClose 返回 false 表示「暂不关闭」，用于承载未保存确认之类的拦截。
      // 它需要同时拦住 ESC、点了遮罩和右上角按钮三条关闭路径。
      if (opts.beforeClose && opts.beforeClose() === false) return;
      closing = true;
      document.removeEventListener('keydown', escHandler);
      mask.remove();
      if (opts.onClose) opts.onClose();
    }

    $('modal-root').appendChild(mask);
    var focusEl = box.querySelector('input[type=text]');
    if (focusEl) { focusEl.focus(); focusEl.select(); }
    return { close: close, el: box, body: body, setLocked: function (v) { opts.locked = v; } };
  }

  function btn(label, cls, onClick) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn' + (cls ? ' ' + cls : '');
    b.textContent = label;
    b.onclick = onClick;
    return b;
  }

  // modalFoot 生成统一的弹窗底部操作区。
  //
  // 用法：`footer: modalFoot([{label:'取消', onClick:...}, {label:'创建', cls:'primary', onClick:...}])`
  //
  // 为什么要有这个函数：以前每个弹窗都是自己拼一个 flex 容器再挂到 .modal 上，
  // 于是出现了三种问题 —— 底部没有分隔线与背景（看起来像内容的一部分）、
  // 按钮顺序不统一（有的取消在右）、手机上按钮不等宽。统一走这里之后，
  // 「取消在左、确认在右、确认按钮带语义色」就是默认行为。
  //
  // items 为 null 的项会被跳过，方便按条件插入（例如只读时不显示保存）。
  function modalFoot(items) {
    var foot = document.createElement('div');
    foot.className = 'modal-foot';
    (items || []).forEach(function (it) {
      if (!it) return;
      var b = btn(it.label, it.cls, it.onClick);
      if (it.id) b.id = it.id;
      if (it.title) b.title = it.title;
      if (it.disabled) b.disabled = true;
      foot.appendChild(b);
    });
    return foot;
  }

  // confirmDialog 是 Promise 化的确认弹窗，替代原生 confirm
  // （原生的样式割裂、无法承载富文本，且会阻塞渲染）。
  //
  // 注意这里用 settled 做一次性定值：关闭弹窗会触发 onClose，
  // 而「确定」按钮自身也要先把结果定下来再关闭 —— 否则 onClose 里的
  // resolve(false) 会抢在前面，让用户点「确定」也拿到 false。
  function confirmDialog(opts) {
    return new Promise(function (resolve) {
      var settled = false;
      function finish(v) {
        if (settled) return;
        settled = true;
        resolve(v);
      }

      var body = document.createElement('div');
      if (opts.message) {
        var p = document.createElement('div');
        p.style.lineHeight = '1.8';
        p.textContent = opts.message;
        body.appendChild(p);
      }
      if (opts.detail) {
        var d = document.createElement('div');
        d.className = 'hint-box';
        d.style.marginTop = '12px';
        d.textContent = opts.detail;
        body.appendChild(d);
      }
      var m = openModal({
        title: opts.title || '确认',
        body: body,
        onClose: function () { finish(false); },
        footer: modalFoot([
          { label: opts.cancelLabel || '取消', onClick: function () { m.close(); } },
          {
            label: opts.okLabel || '确定',
            // 破坏性操作用实心红：这是「最后一步确认」，后果需要一眼可见。
            cls: opts.danger ? 'danger-solid' : 'primary',
            onClick: function () {
              finish(true);   // 先定值
              m.close();      // 再关闭（此时 onClose 的 finish(false) 会被忽略）
            }
          }
        ])
      });
    });
  }

  // ---------------------------------------------------------------- 渲染

  function renderBreadcrumb() {
    var bc = $('breadcrumb');
    bc.textContent = '';

    var rootCrumb = document.createElement('a');
    rootCrumb.textContent = '根目录';
    rootCrumb.href = raw('/');
    rootCrumb.onclick = function (e) { e.preventDefault(); navigate('/'); };
    bc.appendChild(rootCrumb);

    var parts = state.path.split('/').filter(Boolean);
    parts.forEach(function (part, i) {
      var sep = document.createElement('span');
      sep.className = 'sep';
      sep.textContent = '/';
      bc.appendChild(sep);

      var isLast = i === parts.length - 1;
      var target = '/' + parts.slice(0, i + 1).join('/');
      var el = document.createElement(isLast ? 'span' : 'a');
      el.textContent = part;
      el.className = 'crumb' + (isLast ? ' current' : '');
      if (!isLast) {
        el.href = raw(encPath(target) + '/');
        el.onclick = function (e) { e.preventDefault(); navigate(target); };
      }
      bc.appendChild(el);
    });
    bc.scrollLeft = bc.scrollWidth;
  }

  function sortedEntries() {
    var k = state.sortKey, dir = state.sortDir;
    return state.entries.slice().sort(function (a, b) {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      var r = 0;
      if (k === 'size') r = (a.size || 0) - (b.size || 0);
      else if (k === 'mtime') r = new Date(a.mtime) - new Date(b.mtime);
      else r = a.name.toLowerCase().localeCompare(b.name.toLowerCase(), 'zh-Hans-CN');
      if (r === 0) r = a.name.toLowerCase().localeCompare(b.name.toLowerCase(), 'zh-Hans-CN');
      return r * dir;
    });
  }

  // pickedEntries 返回当前选中的条目（保持列表顺序）。
  function pickedEntries() {
    return state.entries.filter(function (e) { return state.selected.has(e.path); });
  }

  // countOf 统计一组目录/文件的数量与文件字节数。
  // 目录大小按 0 计 —— 不做递归统计（一个含百万文件的目录会把页面拖死），
  // 所以文案里写的是「文件合计」而不是含糊的「共」。
  function countOf(list) {
    var r = { dirs: 0, files: 0, bytes: 0 };
    (list || []).forEach(function (e) {
      if (e.is_dir) r.dirs++;
      else { r.files++; r.bytes += e.size || 0; }
    });
    return r;
  }

  // statText 生成工具栏统计文案。
  //
  // 口径说明（之前这一行显示不准就是因为口径混了）：
  //   - 目录数/文件数/大小一律取自服务端返回的 listing，而不是前端把
  //     entries 再数一遍。两边口径必须一致，否则碰上限截断或隐藏文件时，
  //     页面会报出一个与真实情况不符的数字；
  //   - 有选中时先报「已选」的规模，再报整个目录的规模，两者用 / 分开。
  //     以前选中之后数字完全不变，很容易把「整个目录有多大」误读成
  //     「我选中的有多大」；
  //   - 被 --list-max-entries 截断时明确标注，避免看起来像是「目录里就这么多」。
  function statText() {
    var l = state.listing || {};
    var listed = state.entries.length;
    var dirs = typeof l.dir_count === 'number' ? l.dir_count : countOf(state.entries).dirs;
    var dirsAll = countOf(state.entries);
    var files = listed - dirs;
    var size = typeof l.file_size === 'number' ? l.file_size : dirsAll.bytes;
    var picked = pickedEntries();
    // 窄屏走紧凑口径（单位词去掉「个」、大小不写「文件合计」）。
    // 375px 下完整口径会折成 3 行，把工具条撑到 100px 以上。
    var narrow = isNarrow();

    var out = '';
    if (picked.length) {
      var pk = countOf(picked);
      if (narrow) {
        out += '已选 ' + picked.length + ' 项';
        if (pk.bytes > 0) out += ' · ' + fmtSize(pk.bytes);
      } else {
        out += '已选 ' + picked.length + ' 项（' + pk.dirs + ' 个目录 · ' + pk.files + ' 个文件';
        if (pk.bytes > 0) out += ' · ' + fmtSize(pk.bytes);
        out += '）';
      }
      out += '　/　';
    }

    if (state.query) {
      out += narrow
        ? '命中 ' + listed + ' 项'
        : '搜索命中 ' + listed + ' 项（' + dirs + ' 个目录 · ' + files + ' 个文件）';
    } else {
      out += narrow
        ? dirs + ' 目录 · ' + files + ' 文件'
        : dirs + ' 个目录 · ' + files + ' 个文件';
    }
    if (size > 0) out += narrow ? ' · ' + fmtSize(size) : ' · 文件合计 ' + fmtSize(size);

    if (l.truncated) {
      out += '　·　已截断：该目录共 ' + (l.total_all || 0) + ' 项，仅列出前 ' + listed + ' 项';
    }
    return out;
  }

  function renderTable() {
    var tbody = $('tbody');
    tbody.textContent = '';
    var list = sortedEntries();

    $('empty').hidden = list.length > 0;
    if (list.length === 0) {
      $('empty').textContent = state.query
        ? '没有匹配「' + state.query + '」的文件'
        : '这个目录是空的';
    }

    list.forEach(function (e) {
      var tr = document.createElement('tr');
      tr.dataset.path = e.path;
      if (state.selected.has(e.path)) tr.classList.add('selected');

      // 选择框（包一层 label，让可点面积覆盖整个格子）
      var tdCheck = document.createElement('td');
      tdCheck.className = 'col-check';
      var hit = document.createElement('label');
      hit.className = 'check-hit';
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = state.selected.has(e.path);
      cb.style.accentColor = 'var(--accent)';
      cb.onchange = function () {
        if (cb.checked) state.selected.add(e.path);
        else state.selected.delete(e.path);
        tr.classList.toggle('selected', cb.checked);
        syncSelection();
      };
      hit.appendChild(cb);
      tdCheck.appendChild(hit);
      tr.appendChild(tdCheck);

      // 名称
      var tdName = document.createElement('td');
      tdName.className = 'col-name';
      var cell = document.createElement('div');
      cell.className = 'name-cell' + (e.is_dir ? ' dir' : '') + (e.archive ? ' archive' : '');
      cell.appendChild(svgIcon(kindOf(e)));
      var label = document.createElement('span');
      label.className = 'label';
      label.textContent = e.name;
      label.title = e.path;
      if (e.is_dir) {
        label.onclick = function () { navigate(e.path); };
      } else if (canEdit(e)) {
        // 可编辑的文本文件，点名字直接进编辑器——这是最常用的动作。
        label.onclick = function () { G.openEditor(e); };
        label.title = e.path + '　（点击在线编辑）';
      } else {
        label.onclick = function () { downloadEntry(e); };
      }
      cell.appendChild(label);
      if (e.archive) {
        var tag = document.createElement('span');
        tag.className = 'tag zip';
        tag.textContent = '可解压';
        cell.appendChild(tag);
      } else if (e.is_dir) {
        var dt = document.createElement('span');
        dt.className = 'tag';
        dt.textContent = '目录';
        cell.appendChild(dt);
      }
      tdName.appendChild(cell);
      tr.appendChild(tdName);

      // 大小
      var tdSize = document.createElement('td');
      tdSize.className = 'size-cell col-size';
      tdSize.textContent = e.is_dir ? '—' : fmtSize(e.size);
      tr.appendChild(tdSize);

      // 时间
      var tdTime = document.createElement('td');
      tdTime.className = 'time-cell col-time';
      tdTime.textContent = fmtTime(e.mtime);
      tr.appendChild(tdTime);

      // 操作
      var tdOps = document.createElement('td');
      tdOps.className = 'col-ops';
      var ops = document.createElement('div');
      ops.className = 'ops';

      // 先把「这一行能做哪些操作」列成数组，再决定怎么呈现。
      // 宽屏铺成一排按钮，窄屏收进「更多」菜单 —— 两处用同一份定义，
      // 不会出现「桌面点得到、手机点不到」的功能差异。
      var actions = [];
      if (e.is_dir) {
        actions.push({ kind: 'open', label: '打开', run: function () { navigate(e.path); } });
      } else {
        actions.push({ kind: 'download', label: '下载', run: function () { downloadEntry(e); } });

        if (e.editable && can('edit')) {
          var tooBig = !canEdit(e);
          actions.push({
            kind: 'edit', label: '编辑', disabled: tooBig,
            hint: tooBig ? '超过 ' + fmtSize(editMax()) : '',
            run: function () { G.openEditor(e); }
          });
        }
        if (e.archive && can('extract')) {
          actions.push({ kind: 'unzip', label: '解压', cls: 'zip', run: function () { openExtractDialog(e); } });
          actions.push({ kind: 'list', label: '内容', run: function () { openArchivePreview(e); } });
        }
      }
      if (can('write')) {
        actions.push({ kind: 'rename', label: '重命名', run: function () { openRenameDialog(e); } });
      }
      if (can('delete')) {
        actions.push({ kind: 'trash', label: '删除', cls: 'danger', run: function () { doDelete([e]); } });
      }

      actions.forEach(function (a) {
        var b = makeOpBtn(a.kind, a.label, a.cls);
        if (a.disabled) {
          b.disabled = true;
          b.title = a.label + '：' + a.hint;
        } else {
          b.onclick = a.run;
        }
        ops.appendChild(b);
      });

      // 窄屏把上面这些按钮全藏起来，只留这一个「更多」；宽屏反过来。
      // 显隐交给 CSS，避免 JS 的断点判断跟 CSS 打架。
      if (actions.length > 1) {
        var moreBtn = makeOpBtn('more', '更多', 'ops-more');
        moreBtn.title = '更多操作';
        moreBtn.setAttribute('aria-label', '更多操作');
        moreBtn.onclick = function () { openRowActions(e, actions); };
        ops.appendChild(moreBtn);
      }

      tdOps.appendChild(ops);
      tr.appendChild(tdOps);
      tbody.appendChild(tr);
    });

    syncSelection();
    renderSortHeader();
  }

  // openRowActions 是窄屏下的操作入口：把整行操作铺成一个列表。
  //
  // 手机上行内挤 5 个按钮会把名称列压到只剩 100px 出头，而且每个按钮
  // 都远小于可点尺寸。收进菜单后，每一项都能做到 48px 高、占满宽度。
  function openRowActions(entry, actions) {
    var body = document.createElement('div');
    body.className = 'action-sheet';

    var head = document.createElement('div');
    head.className = 'action-path';
    head.textContent = entry.path;
    body.appendChild(head);

    var m = openModal({
      title: entry.name,
      body: body,
      // 手机上习惯有一个明确的「取消」出口，而不是只能点右上角的小叉。
      footer: modalFoot([{ label: '取消', onClick: function () { m.close(); } }])
    });

    actions.forEach(function (a) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'action-item' + (a.cls ? ' ' + a.cls : '');
      b.dataset.kind = a.kind;

      var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      svg.setAttribute('viewBox', '0 0 24 24');
      svg.setAttribute('aria-hidden', 'true');
      svg.innerHTML = OP_ICONS[a.kind] || OP_ICONS.list;
      b.appendChild(svg);

      var sp = document.createElement('span');
      sp.textContent = a.label;
      b.appendChild(sp);

      if (a.disabled) {
        b.disabled = true;
        if (a.hint) {
          var em = document.createElement('em');
          em.textContent = a.hint;
          b.appendChild(em);
        }
      } else {
        b.onclick = function () { m.close(); a.run(); };
      }
      body.appendChild(b);
    });
  }

  function renderSortHeader() {
    document.querySelectorAll('.listing th.sortable').forEach(function (th) {
      var active = th.dataset.sort === state.sortKey;
      th.classList.toggle('active', active);
      var arrow = th.querySelector('.arrow');
      if (arrow) arrow.remove();
      if (active) {
        var s = document.createElement('span');
        s.className = 'arrow';
        s.textContent = state.sortDir > 0 ? '▲' : '▼';
        th.appendChild(s);
      }
    });
  }

  function syncSelection() {
    var listed = state.entries.length;
    var n = state.selected.size;
    // 全选框要跟「当前实际列出/命中的条目」比，而不是跟磁盘上的条目总数比：
    // 被截断时用户看到的就只有这些，全选自然也只能覆盖这些。
    $('check-all').checked = listed > 0 && n >= listed;
    $('check-all').indeterminate = n > 0 && n < listed;

    var stat = $('stat');
    stat.textContent = statText();
    stat.title = '「文件合计」只统计当前列表里的文件大小，不含子目录里的内容；' +
      '目录本身不计入大小。';

    var btnDel = $('btn-delete-sel');
    btnDel.hidden = !(can('delete') && n > 0);
    if (n > 0) btnDel.querySelector('span').textContent = '删除所选 (' + n + ')';

    // 打包按钮跟着选中数变脸：0 项时是「打包下载」（整个目录），
    // 有选中时是「打包所选 (n)」，点击前就能看出会打包什么。
    var btnZip = $('btn-zip');
    var zipLabel = btnZip.querySelector('span');
    if (n > 0) {
      zipLabel.textContent = '打包所选 (' + n + ')';
      btnZip.title = '把选中的 ' + n + ' 项打包成一个 zip 下载（选中的目录会连同内容一起打包）';
    } else {
      zipLabel.textContent = '打包下载';
      btnZip.title = '把当前目录整个打包成 zip 下载';
    }
  }

  function renderPerms() {
    $('btn-upload').hidden = !can('write');
    $('btn-upload-dir').hidden = !can('write');
    $('btn-mkdir').hidden = !can('write');
    $('btn-zip').hidden = !can('archive');
    $('btn-keys').hidden = !allowKeys();
    $('btn-users').hidden = !allowUsers();
    $('btn-settings').hidden = !allowSettings();
    if (can('write')) {
      $('btn-upload').title = uploadLimitHint();
      // 文件夹上传的提示：说清「会保留结构」，这是它和普通上传最大的区别。
      $('btn-upload-dir').title = '上传整个文件夹（保留目录结构）　·　' +
        uploadLimitHint();
    }
    if (!$('btn-keys').hidden) {
      $('btn-keys').title = '上传密钥：给脚本一个只能上传、可设有效期、可随时撤销的凭证';
    }
    if (!$('btn-users').hidden) {
      $('btn-users').title = '用户与权限：新建账号、按路径分配只读 / 读写权限';
    }
    if (!$('btn-settings').hidden) {
      $('btn-settings').title = '服务设置：切换服务根目录、调整单文件上传上限';
    }
    updateAuthChip();
    $('btn-refresh').title = DATA.auth_on ? '刷新（当前：' + (DATA.user || '匿名') + '）' : '刷新';
    renderDropZone();
    // 页脚也要跟着权限刷新 —— 它以前只在启动时算一次，
    // 结果登录之后一直显示「只读模式」，比不显示更误导。
    renderFooter();
  }

  // uploadLimitText 返回单文件上限的说明文字（未下发时返回空串）。
  function uploadLimitText() {
    var max = DATA.upload_max_size;
    if (typeof max !== 'number') return '';
    return max > 0 ? '单文件上限 ' + fmtSize(max) : '单文件不限大小';
  }

  // uploadLimitHint 生成「上传」按钮的提示，说明落点与大小上限。
  function uploadLimitHint() {
    var parts = ['上传文件（也可直接把文件拖进页面）'];
    var dir = uploadTargetDir(state.path);
    parts.push('存入 ' + (dir === '/' ? '/' : dir + '/'));
    // 只有真的会归档时才提这件事。判断用「实际落点 ≠ 当前目录」，
    // 与投放区、上传弹窗完全同一套口径。
    if (dir !== state.path) parts.push('按日期自动归档');
    var lim = uploadLimitText();
    if (lim) parts.push(lim);
    return parts.join('　·　');
  }

  // renderDropZone 更新常驻拖拽投放区的文案与显隐。
  //
  // 为什么要有常驻区：只有拖拽过程中才出现的浮层，在用户**开始拖之前**
  // 是不可见的，等于没有告诉任何人「这个页面支持拖拽」。把入口画在页面上，
  // 并写明落点，才算真的把能力暴露出来。
  //
  // 窄屏是另一种东西：手机不能拖拽，所以那句「拖到这里」是错的，
  // 整条改写成一行可点的上传入口（点一下就是文件选择框），
  // 并且搬到了列表上方 —— 长列表下面的一行字等于不存在。
  function renderDropZone() {
    var zone = $('drop-zone');
    if (!zone) return;
    var allowed = can('write');
    zone.hidden = !allowed;
    if (!allowed) return;

    var narrow = isNarrow();
    var dir = uploadTargetDir(state.path);
    var lim = uploadLimitText();

    if (narrow) {
      // 一行放得下才不会被截断：手机上一句「存入 /2026/09/28/ · 自动归档 · ≤ 10 GiB」
      // 约 240px，紧凑但完整；<strong> 那句「拖到这里」由 CSS 隐藏。
      $('drop-zone-lead').textContent = '上传文件';
      $('drop-zone-target').textContent = '存入 ' + (dir === '/' ? '/' : dir + '/') +
        (dir !== state.path ? ' · 自动归档' : '') +
        ' · ≤ ' + (uploadMax() > 0 ? fmtSize(uploadMax()) : '不限');
    } else {
      $('drop-zone-lead').textContent = '把文件或文件夹拖到这里即可上传';
      $('drop-zone-target').textContent = '存入 ' + (dir === '/' ? '/' : dir + '/') +
        (dir !== state.path ? '（按日期自动归档）' : '');
      if (lim) $('drop-zone-target').textContent += '　·　' + lim;
    }
    zone.title = narrow ? '选择文件或文件夹上传到 ' + dir + '/' : '';
  }

  // renderFooter 渲染页脚。它随登录状态与权限实时变化，因此必须在
  // renderPerms 里调用，而不是只在启动时算一次。
  function renderFooter() {
    $('footer-left').textContent = 'gofs ' + (DATA.version || '');

    var narrow = isNarrow();
    var who;
    if (!DATA.auth_on) {
      // 服务本身没开鉴权，提「登录」只会让人困惑。
      who = can('write') ? '无需登录 · 可读写' : '无需登录 · 只读';
    } else if (DATA.user) {
      // 窄屏省掉「已登录」三个字：页脚只有一行位置，
      // 「admin · 可读写」已经把所有信息说完了。
      who = narrow
        ? DATA.user + ' · ' + (can('write') ? '可读写' : '只读')
        : '已登录 ' + DATA.user + ' · ' + (can('write') ? '可读写' : '只读');
    } else {
      who = '未登录 · 只读';
    }

    var caps = [];
    if (can('write')) caps.push(narrow ? '上传' : '可上传');
    if (can('delete')) caps.push(narrow ? '删除' : '可删除');
    if (can('edit')) caps.push(narrow ? '编辑' : '可在线编辑');
    if (can('extract')) caps.push(narrow ? '解压' : '可在线解压');
    // 归档只在根目录生效，页脚也要跟着当前目录变 —— 站在子目录里还写
    // 「上传归档到 2026/09/28/」就是假的。
    if (can('write') && archiveAt(state.path)) {
      caps.push(narrow ? '归档 ' + uploadDateDir() + '/' : '上传归档到 ' + uploadDateDir() + '/');
    }

    // 窄屏用「·」连成一串能力词，宽屏保持原来的「可××」长写法。
    $('footer-right').textContent = caps.length
      ? who + '　·　' + caps.join(narrow ? '·' : ' · ')
      : who;
  }

  function render() {
    renderBreadcrumb();
    renderTable();
    renderPerms();
    document.title = (state.path === '/' ? '' : state.name + ' — ') + 'gofs';
  }

  // ---------------------------------------------------------------- 导航与刷新

  async function fetchListing(path) {
    var res = await apiFetch(raw(encPath(path)) + '?json', {
      headers: { 'Accept': 'application/json' }
    });
    if (res.status === 401) throw new Error('需要登录（401）');
    if (!res.ok) throw new Error(await res.text());
    return res.json();
  }

  async function navigate(path, push) {
    // 已经在导航中：**不要静默丢弃**这次请求，把它记下来，等当前这次结束后补跑。
    //
    // 静默 return 的症状就是「点了没反应」：用户点「根目录」的那一刻，如果恰好
    // 有一次「上传完成后的列表刷新」在途（那是很常见的），这次点击就白点了 ——
    // 地址栏不动、列表不动、也没有任何提示，用户只能反复点。
    // 队列里只保留最后一次请求即可（中间那些本来就会被覆盖），被取代的那次
    // 立刻以 false 结掉，免得它的调用方一直等下去。
    if (state.busy) {
      if (window.__navDebug) console.log('[nav] 排队', path);
      if (pendingNav) pendingNav.resolve(false);
      return new Promise(function (resolve) {
        // 返回的 Promise 要等到这次排队的导航真正跑完才 resolve，
        // 调用方才能用 `await navigate(...)` 判断「到了没有」。
        pendingNav = { path: path, push: push, resolve: resolve };
      });
    }
    // 打开 window.__navDebug = true 就能看到每次导航的调用来源，
    // 排查「点了没反应 / 列表没刷新」这类问题时比一步步断点快得多。
    if (window.__navDebug) {
      console.log('[nav] 进入', path, 'busy=false',
        new Error().stack.split('\n')[2]);
    }
    state.busy = true;
    try {
      var data = await fetchListing(path);
      state.path = data.path || path;
      state.name = data.name || path;
      state.entries = data.entries || [];
      state.listing = data;
      state.query = '';
      state.selected.clear();
      $('search-input').value = '';
      // 换了目录，顶栏的「更多」下拉不该跨页留着。
      if (G.closeMore) G.closeMore();
      if (push !== false && DATA.perms.read) {
        // 地址栏更新是「锦上添花」，不能让它拖垮导航：目录已经取回来了，
        // 万一 pushState 因为任何原因失败（URL 构造问题、跨源限制、
        // 浏览器历史条目上限），也应该照常把列表渲染出来。
        try {
          history.pushState({ path: state.path }, '', dirURL(state.path));
        } catch (e) {
          if (window.console && console.warn) console.warn('更新地址栏失败：', e);
        }
      }
      render();
    } catch (err) {
      toast('err', '打开目录失败：' + err.message);
      if (pendingNav) pendingNav.resolve(false);
      pendingNav = null;
      return false;
    } finally {
      state.busy = false;
      var next = pendingNav;
      pendingNav = null;
      if (next) {
        // 补跑排队中的那次；结果回传给它的调用方（async 函数体在第一个
        // await 之前是同步执行的，所以这里忙碌标志已经被重新置上）。
        navigate(next.path, next.push).then(next.resolve, function () { next.resolve(false); });
      }
    }
    return true;
  }

  async function doSearch(q) {
    if (!q) { navigate(state.path); return; }
    try {
      var res = await apiFetch(raw(encPath(state.path)) + '?q=' + encodeURIComponent(q) + '&json');
      if (!res.ok) throw new Error(await res.text());
      var data = await res.json();
      state.query = q;
      state.entries = data.entries || [];
      state.listing = data;
      state.selected.clear();
      render();
      $('search-input').value = q;
    } catch (err) {
      toast('err', '搜索失败：' + err.message);
    }
  }

  // ---------------------------------------------------------------- 上传

  // filenameFromResponse 从 Content-Disposition 里取服务端建议的文件名。
  // 优先 RFC 5987 的 filename*（能承载中文），回退到 ASCII 的 filename。
  function filenameFromResponse(res) {
    var cd = res.headers.get('content-disposition') || '';
    var m = /filename\*=UTF-8''([^;]+)/i.exec(cd);
    if (m) {
      try { return decodeURIComponent(m[1].trim()); } catch (e) { return m[1].trim(); }
    }
    m = /filename="([^"]*)"/i.exec(cd);
    return m ? m[1] : '';
  }

  // downloadFile 用 fetch 拉取再触发本地保存。
  //
  // 不能用 window.open / location.href 直接指向文件：那种导航不会带
  // Authorization 头，开启鉴权时会直接 401。通过 fetch 拿 blob 可以
  // 复用应用自己的凭据，顺带还能显示下载进度。
  //
  // filename 留空时沿用响应头里的建议名——打包下载的文件名由服务端决定
  // （按「全部」还是「所选」各不相同），让服务端当唯一真相。
  async function downloadFile(url, filename, onProgress, opts) {
    var res = await apiFetch(url, opts);
    if (!res.ok) {
      var msg = (await res.text()).trim();
      throw new Error(msg || ('HTTP ' + res.status));
    }

    var blob;
    var len = parseInt(res.headers.get('content-length') || '0', 10);
    if (onProgress && res.body && len > 0) {
      var reader = res.body.getReader();
      var chunks = [];
      var received = 0;
      for (;;) {
        var step = await reader.read();
        if (step.done) break;
        chunks.push(step.value);
        received += step.value.length;
        onProgress(received, len);
      }
      blob = new Blob(chunks, { type: res.headers.get('content-type') || 'application/octet-stream' });
    } else {
      blob = await res.blob();
    }

    var href = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = href;
    a.download = filename || filenameFromResponse(res) || '';
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(function () { URL.revokeObjectURL(href); }, 60000);
    return res;
  }

  // baseName 取路径的文件名。
  function baseName(p) {
    var s = String(p).replace(/\/+$/, '');
    var i = s.lastIndexOf('/');
    return i >= 0 ? s.slice(i + 1) : s;
  }

  // putFile 用 XHR 上传，因为只有 XHR 能提供上传进度。
  // resumeMinSize 是启用断点续传的门槛：文件比它小的时候，
  // 重传一遍比「查询断点 + 续传」更快也更简单。
  var resumeMinSize = 8 * 1024 * 1024;
  var resumeMaxRetry = 2;

  // putOnce 发一次 PUT。offset 为 0 表示从头传（覆盖写）。
  function putOnce(url, file, offset, onProgress) {
    return new Promise(function (resolve, reject) {
      var xhr = new XMLHttpRequest();
      xhr.open('PUT', url, true);
      var headers = authHeaders();
      if (offset > 0) headers['X-Update-Range'] = 'append';
      for (var k in headers) {
        if (Object.prototype.hasOwnProperty.call(headers, k)) {
          try { xhr.setRequestHeader(k, headers[k]); } catch (e) { /* 忽略 */ }
        }
      }
      var body = offset > 0 ? file.slice(offset) : file;
      if (xhr.upload && onProgress) {
        xhr.upload.onprogress = function (e) {
          if (e.lengthComputable) onProgress(offset + e.loaded, file.size);
        };
      }
      xhr.onload = function () {
        if (xhr.status >= 200 && xhr.status < 300) {
          // X-Gofs-Offset 是本次写入的字节数，续传后用它校验收到的总量。
          resolve({
            text: xhr.responseText,
            written: parseInt(xhr.getResponseHeader('X-Gofs-Offset') || '0', 10) || 0
          });
          return;
        }
        // 4xx 是请求本身的问题，重试没有意义；5xx 与网络错误才值得续传。
        var err = new Error('HTTP ' + xhr.status + ' ' + (xhr.responseText || ''));
        err.retryable = xhr.status >= 500;
        reject(err);
      };
      xhr.onerror = function () {
        var err = new Error('网络错误');
        err.retryable = true;
        reject(err);
      };
      xhr.onabort = function () { reject(new Error('已取消')); };
      xhr.send(body);
    });
  }

  // remoteSize 询问服务端目标文件当前有多少字节。
  async function remoteSize(url) {
    try {
      var res = await apiFetch(url, { method: 'HEAD' });
      if (!res.ok) return 0;
      return parseInt(res.headers.get('content-length') || '0', 10) || 0;
    } catch (e) {
      return 0;
    }
  }

  // putFile 上传单个文件，带断点续传。
  //
  // 大文件传到一半断网是常事，从头上传几百 MB 很浪费。这里在中断之后
  // 先问服务端已经落盘多少字节，再从那个位置接着传：
  //   - 首传是覆盖写（O_TRUNC），所以失败后残留的那部分**一定是本次的内容**，
  //     接着往后写不会把别的文件拼进来；
  //   - 续传用 X-Update-Range: append，服务端追加到末尾，语义正好吻合；
  //   - 服务端会返回本次写入的字节数，用来确认最终大小一致。
  async function putFile(url, file, onProgress) {
    var offset = 0;
    for (var attempt = 0; ; attempt++) {
      try {
        var out = await putOnce(url, file, offset, onProgress);
        // 续传之后大小不对，说明中间有内容丢失（服务端被别的写法覆盖过、
        // 或断点算错了）。宁可报错让用户重传，也不要留下一个看起来成功、
        // 实际残缺的文件。
        if (offset > 0 && file.size >= resumeMinSize &&
            offset + out.written !== file.size) {
          throw new Error('续传后大小不一致（' + (offset + out.written) +
            ' / ' + file.size + ' 字节），请重新上传');
        }
        return out.text;
      } catch (err) {
        if (!err.retryable || file.size < resumeMinSize || attempt >= resumeMaxRetry) throw err;

        var got = await remoteSize(url);
        // 只有「确实写了一部分、但还没写完」才值得续传；
        // 否则（0 字节或已完整）重传更稳妥。
        if (!(got > 0 && got < file.size)) throw err;
        offset = got;
        if (onProgress) onProgress(offset, file.size);
        toast('warn', '上传中断，已从 ' + fmtSize(offset) + ' 处继续');
      }
    }
  }

  // ---- 文件夹上传 ----------------------------------------------------------
  //
  // 浏览器给的两条路都能拿到「相对路径」，从而实现保留目录结构：
  //   1) <input webkitdirectory> 选目录 → 每个 File 自带 webkitRelativePath；
  //   2) 拖拽目录 → 只有 DataTransferItem.webkitGetAsEntry() 能递归拿到
  //      FileSystemDirectoryEntry，它的 file() 返回的 File **没有**
  //      webkitRelativePath，所以边遍历边自己拼一个。
  // 两条路最终都收敛成「带相对路径的 File 列表」，后面的上传逻辑完全共用。

  // uploadConcurrency 同时进行的上传请求数。
  //
  // 为什么不是 1：传文件夹时绝大多数是几 KB 的小文件，串行的话每个文件都要
  // 等一个 RTT，几百个文件会慢得离谱。取 3 是个折中 —— 小文件能吃到并行，
  // 又远低于服务端的并发闸门，且总带宽不会被切得太碎（速度读数也还稳）。
  var uploadConcurrency = 3;

  // uploadBatchWarn 超过这个数量先弹一次确认。
  // 传文件夹时手滑拖进整个 home 目录是很容易发生的，先问一句。
  var uploadBatchWarn = 200;

  // relPathOf 取文件的相对路径（没有就退化成文件名）。
  function relPathOf(f) {
    return f.webkitRelativePath || f.__relPath || f.name;
  }

  // sanitizeRel 清洗相对路径：丢掉空段、`.`、`..`，以及含分隔符/反斜杠的段。
  // 正常来源（浏览器给的相对路径、遍历得到的 entry.name）都不会命中，
  // 但这是从「用户设备上的文件名」拼出来的路径，值得在下发前挡一道。
  function sanitizeRel(rel) {
    var parts = String(rel || '').split('/');
    var out = [];
    for (var i = 0; i < parts.length; i++) {
      var seg = parts[i];
      if (!seg || seg === '.' || seg === '..') continue;
      if (seg.indexOf('\\') >= 0 || seg.indexOf(':') >= 0) continue;
      out.push(seg);
    }
    return out.join('/');
  }

  // readAllEntries 反复调 readEntries 直到返回空数组。
  // 规范允许一次只返回一批（Chromium 是 100 条），只调一次会漏文件。
  function readAllEntries(reader) {
    return new Promise(function (resolve) {
      var acc = [];
      var step = function () {
        reader.readEntries(function (batch) {
          if (!batch || !batch.length) { resolve(acc); return; }
          acc = acc.concat(Array.prototype.slice.call(batch));
          step();
        }, function () { resolve(acc); });
      };
      step();
    });
  }

  // collectEntryFiles 递归展开拖进来的目录，返回带相对路径的 File 列表。
  async function collectEntryFiles(items) {
    var out = [];
    var skipped = 0;

    async function walk(entry, prefix) {
      if (!entry) return;
      if (entry.isFile) {
        var f = await new Promise(function (resolve) {
          entry.file(resolve, function () { resolve(null); });
        });
        if (!f) { skipped++; return; }
        // 原生 File 没有 webkitRelativePath，自己挂一个（只读属性，用 defineProperty）
        var rel = sanitizeRel(prefix + entry.name);
        if (!rel) { skipped++; return; }
        try {
          Object.defineProperty(f, '__relPath', { value: rel });
        } catch (e) { /* 挂不上就退化用 name */ }
        out.push(f);
        return;
      }
      if (entry.isDirectory) {
        var reader = entry.createReader();
        var children = await readAllEntries(reader);
        for (var i = 0; i < children.length; i++) {
          await walk(children[i], prefix + entry.name + '/');
        }
      }
    }

    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      if (!it || it.kind !== 'file' || typeof it.webkitGetAsEntry !== 'function') continue;
      await walk(it.webkitGetAsEntry(), '');
    }
    return { files: out, skipped: skipped };
  }

  // pickDroppedFiles 从一次 drop 里取出文件。
  //
  // ⚠️ DataTransferItemList 只在事件派发期间有效，任何 await 之后都会失效，
  // 所以必须**同步**先把 items 取出来（并立刻调用 webkitGetAsEntry 拿到 entry），
  // 再去异步遍历。
  async function pickDroppedFiles(dt) {
    if (!dt) return { files: [], skipped: 0 };
    var entries = [];
    var items = dt.items;
    if (items && items.length) {
      for (var i = 0; i < items.length; i++) {
        var it = items[i];
        if (!it || it.kind !== 'file') continue;
        if (typeof it.webkitGetAsEntry === 'function') {
          var e = null;
          try { e = it.webkitGetAsEntry(); } catch (err) { e = null; }
          if (e) { entries.push(e); continue; }
        }
        // 拿不到 entry（老浏览器 / 非文件项）：退回 getAsFile
        if (typeof it.getAsFile === 'function') {
          var f0 = it.getAsFile();
          if (f0) entries.push({ isFile: true, _file: f0, name: f0.name });
        }
      }
    }
    if (entries.length) {
      var res = await collectEntryFiles(entries.map(function (e) {
        // 统一成「像 DataTransferItem」的形状，复用同一段遍历代码
        return { kind: 'file', webkitGetAsEntry: function () { return e; } };
      }));
      if (res.files.length) return res;
    }
    // 最后兜底：扁平文件列表（拖文件夹时这里会拿不到内容，但至少不丢普通文件）
    var files = Array.prototype.slice.call(dt.files || []);
    return { files: files, skipped: 0 };
  }

  // countDroppedItems 数出拖拽浮层里要显示的条目数。
  function countDroppedItems(dt) {
    if (!dt) return 0;
    if (dt.items && dt.items.length) {
      var n = 0;
      for (var i = 0; i < dt.items.length; i++) {
        if (dt.items[i] && dt.items[i].kind === 'file') n++;
      }
      return n;
    }
    return (dt.files && dt.files.length) || 0;
  }

  // ---- 上传 ----------------------------------------------------------------

  async function uploadFiles(files, targetDir) {
    if (!files || !files.length) return;
    if (!can('write')) { toast('warn', '当前没有上传权限'); return; }

    var jobs = [];
    var skippedNames = 0;
    for (var i = 0; i < files.length; i++) {
      var f = files[i];
      // 相对路径用于文件夹上传，保留目录结构。
      var rel = sanitizeRel(relPathOf(f));
      if (!rel) { skippedNames++; continue; }
      jobs.push({ file: f, rel: rel, dest: joinPath(targetDir, rel) });
    }
    if (!jobs.length) {
      toast('warn', skippedNames ? '这些条目没有可用的文件名，已跳过' : '没有可上传的文件');
      return;
    }

    // 传文件夹很容易一次带上成千上万个文件。批量确认一次，
    // 免得手滑把整个 home 目录拖进来。
    var isTree = jobs.length > 1 && jobs.some(function (j) { return j.rel.indexOf('/') >= 0; });
    if (jobs.length > uploadBatchWarn) {
      var okGo = await confirmDialog({
        title: '确认上传 ' + jobs.length + ' 个文件？',
        message: '这次包含 ' + (isTree ? '文件夹，共 ' : '') + jobs.length + ' 个文件，合计 ' +
          fmtSize(jobs.reduce(function (s, j) { return s + j.file.size; }, 0)) + '。',
        detail: '会保留原有的文件夹结构逐个写入服务器；上传过程中点右上角 ✕ 可取消剩余文件。',
        okLabel: '开始上传'
      });
      if (!okGo) return;
    }

    var totalBytes = jobs.reduce(function (s, j) { return s + j.file.size; }, 0);
    var destDir = uploadTargetDir(targetDir);
    // 每个文件已传字节，用来在并发时汇总总进度。
    var loadedByJob = new Array(jobs.length);
    for (var z = 0; z < jobs.length; z++) loadedByJob[z] = 0;
    var loadedTotal = 0;
    var finished = 0;
    var failures = [];
    var cancelled = false;
    var poolDone = false;

    var body = document.createElement('div');

    // 无论是否开启归档，都明确写出「文件会落到哪个目录」——
    // 上传最让人不安的就是不知道东西存哪去了。
    var note = document.createElement('div');
    note.className = 'hint-box';
    note.style.marginBottom = '14px';
    var nb = document.createElement('b');
    nb.textContent = '存入';
    note.appendChild(nb);
    note.appendChild(document.createTextNode('：'));
    var code = document.createElement('code');
    code.textContent = destDir + '/';
    note.appendChild(code);
    if (destDir !== targetDir) {
      note.appendChild(document.createTextNode('（按日期自动归档）'));
    }
    note.appendChild(document.createTextNode('　·　单文件上限：' +
      (uploadMax() > 0 ? fmtSize(uploadMax()) : '不限')));
    body.appendChild(note);

    var bar = document.createElement('i');
    var wrap = document.createElement('div');
    wrap.className = 'progress-wrap';
    var pb = document.createElement('div');
    pb.className = 'progress-bar';
    pb.appendChild(bar);
    wrap.appendChild(pb);
    var meta = document.createElement('div');
    meta.className = 'progress-meta';
    var metaL = document.createElement('span');
    var metaR = document.createElement('span');
    meta.appendChild(metaL);
    meta.appendChild(metaR);
    wrap.appendChild(meta);
    var cur = document.createElement('div');
    cur.className = 'progress-current';
    wrap.appendChild(cur);
    body.appendChild(wrap);

    var m = openModal({
      title: (isTree ? '上传文件夹' : '上传 ' + jobs.length + ' 个文件'),
      body: body,
      locked: true,
      // locked 的弹窗不能用 ESC / 点遮罩关掉，但右上角的 ✕ 仍然可点 ——
      // 传文件夹时那正是「我不想再传了」的出口。只有池子还没跑完时才算取消：
      // 正常收尾时我们自己调 m.close()，那一刻不能再把自己标成取消。
      beforeClose: function () {
        if (!poolDone) cancelled = true;
        return true;
      }
    });

    var sp = makeSpeedometer();
    function repaint(name, seq) {
      var pct = totalBytes > 0 ? Math.min(100, (loadedTotal / totalBytes) * 100) : 0;
      bar.style.width = pct.toFixed(1) + '%';
      metaL.textContent = Math.round(pct) + '%';
      metaR.textContent = fmtSize(loadedTotal) + ' / ' + fmtSize(totalBytes) +
        speedSuffix(sp.sample(loadedTotal, totalBytes));
      // 传文件夹时把「第几个 / 共几个」和当前相对路径都写出来，
      // 否则一整个目录只看得到数字在动，完全不知道进行到哪了。
      if (name !== undefined) {
        cur.textContent = (seq && jobs.length > 1 ? seq + '/' + jobs.length + '　' : '') + name;
      }
    }
    repaint();

    // 有界并发：worker 池 + 共享游标，避免一次性发起 N 个请求。
    var cursor = 0;
    async function worker() {
      for (;;) {
        if (cancelled) return;
        var idx = cursor++;
        if (idx >= jobs.length) return;
        var job = jobs[idx];
        repaint(job.rel, idx + 1);
        try {
          await putFile(fileURL(job.dest), job.file, function (loaded) {
            loadedTotal += loaded - loadedByJob[idx];
            loadedByJob[idx] = loaded;
            repaint(job.rel, idx + 1);
          });
          loadedTotal += job.file.size - loadedByJob[idx];
          loadedByJob[idx] = job.file.size;
        } catch (err) {
          failures.push(job.rel + '：' + err.message);
        }
        finished++;
      }
    }

    var n = Math.min(uploadConcurrency, jobs.length);
    var pool = [];
    for (var w = 0; w < n; w++) pool.push(worker());
    await Promise.all(pool);
    poolDone = true;

    var okCount = jobs.length - failures.length;
    bar.style.width = '100%';
    if (!failures.length) bar.style.background = 'var(--ok)';
    m.close();

    var msg = (cancelled ? '已取消，' : '') + '已上传 ' + okCount + '/' + jobs.length + ' 个文件（共 ' +
      fmtSize(totalBytes) + '）';
    if (destDir !== targetDir) msg += '\n存入 ' + destDir + '/';
    if (cancelled) msg += '\n余下 ' + (jobs.length - finished) + ' 个文件未上传';
    if (failures.length) {
      // 传文件夹时失败很容易成片出现，逐个弹 toast 会把界面刷爆 ——
      // 汇总成一条，列出前几个具体原因。
      msg += '\n失败 ' + failures.length + ' 个：' + failures.slice(0, 3).join('；') +
        (failures.length > 3 ? ' …等' : '');
    }
    toast(failures.length ? 'err' : (cancelled ? 'warn' : 'ok'), msg,
      null, okCount > 0 ? {
        label: '查看',
        onClick: function () { navigate(destDir); }
      } : null);

    // 停留在原目录，这样连续拖拽不会越陷越深（不会生成 日期/日期 的嵌套）。
    navigate(state.path, false);
  }

  // ---------------------------------------------------------------- 新建目录

  function openMkdirDialog() {
    var input = document.createElement('input');
    input.type = 'text';
    input.placeholder = '新目录名称';

    var field = document.createElement('div');
    field.className = 'field';
    var lb = document.createElement('label');
    lb.textContent = '目录名称';
    field.appendChild(lb);
    field.appendChild(input);
    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '将创建在：' + (state.path === '/' ? '/' : state.path + '/');
    field.appendChild(hint);

    var m = openModal({
      title: '新建目录',
      body: field,
      footer: modalFoot([
        { label: '取消', onClick: function () { m.close(); } },
        { label: '创建', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    async function submit() {
      var name = input.value.trim();
      if (!name) { toast('warn', '请输入目录名称'); return; }
      if (name.indexOf('/') >= 0) { toast('warn', '名称不能包含 /'); return; }
      try {
        var res = await apiFetch(fileURL(joinPath(state.path, name)), { method: 'MKCOL' });
        if (res.status === 201) {
          toast('ok', '已创建 ' + name);
          m.close();
          navigate(state.path, false);
        } else {
          throw new Error(await res.text());
        }
      } catch (err) {
        toast('err', '创建失败：' + err.message);
      }
    }

    input.onkeydown = function (e) { if (e.key === 'Enter') submit(); };
  }

  // ---------------------------------------------------------------- 重命名 / 移动

  function openRenameDialog(entry) {
    var input = document.createElement('input');
    input.type = 'text';
    input.value = entry.name;

    var field = document.createElement('div');
    field.className = 'field';
    var lb = document.createElement('label');
    lb.textContent = '新名称或新路径';
    field.appendChild(lb);
    field.appendChild(input);
    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '填写相对路径可移动到其他目录，例如 sub/new.txt';
    field.appendChild(hint);

    var m = openModal({
      title: '重命名 / 移动',
      body: field,
      footer: modalFoot([
        { label: '取消', onClick: function () { m.close(); } },
        { label: '确定', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    async function submit() {
      var name = input.value.trim();
      if (!name || name === entry.name) { m.close(); return; }
      var target = name.indexOf('/') >= 0
        ? joinPath(state.path, name.replace(/^\/+/, ''))
        : joinPath(state.path, name);
      try {
        var res = await apiFetch(fileURL(entry.path), {
          method: 'MOVE',
          headers: {
            'Destination': location.origin + fileURL(target),
            'Overwrite': 'F'
          }
        });
        if (res.status === 201 || res.status === 204) {
          toast('ok', '已重命名为 ' + name);
          m.close();
          navigate(state.path, false);
        } else {
          throw new Error(await res.text());
        }
      } catch (err) {
        toast('err', '操作失败：' + err.message);
      }
    }

    input.onkeydown = function (e) { if (e.key === 'Enter') submit(); };
  }

  // ---------------------------------------------------------------- 删除

  function doDelete(entries) {
    if (!can('delete')) { toast('warn', '没有删除权限'); return; }

    var box = document.createElement('div');
    var p = document.createElement('p');
    p.style.margin = '0 0 12px';
    p.textContent = '即将删除以下 ' + entries.length + ' 项，此操作不可撤销：';
    box.appendChild(p);

    var ul = document.createElement('div');
    ul.className = 'hint-box';
    entries.slice(0, 20).forEach(function (e) {
      var line = document.createElement('div');
      line.textContent = '· ' + (e.is_dir ? e.name + '/' : e.name);
      ul.appendChild(line);
    });
    if (entries.length > 20) {
      var more = document.createElement('div');
      more.textContent = '… 以及另外 ' + (entries.length - 20) + ' 项';
      ul.appendChild(more);
    }
    box.appendChild(ul);

    var m = openModal({
      title: '确认删除',
      body: box,
      footer: modalFoot([
        { label: '取消', onClick: function () { m.close(); } },
        {
          label: entries.length > 1 ? '删除 ' + entries.length + ' 项' : '删除',
          cls: 'danger-solid',
          onClick: function () { submit(); }
        }
      ])
    });

    async function submit() {
      var failed = [];
      for (var i = 0; i < entries.length; i++) {
        var e = entries[i];
        try {
          var url = fileURL(e.path) + (e.is_dir ? '?recursive' : '');
          var res = await apiFetch(url, { method: 'DELETE' });
          if (res.status !== 204 && res.status !== 200) {
            failed.push(e.name + ': ' + (await res.text()).trim());
          }
        } catch (err) {
          failed.push(e.name + ': ' + err.message);
        }
      }
      m.close();
      state.selected.clear();
      if (failed.length) {
        toast('err', '部分删除失败：\n' + failed.join('\n'));
      } else {
        toast('ok', '已删除 ' + entries.length + ' 项');
      }
      navigate(state.path, false);
    }
  }

  // ---------------------------------------------------------------- 压缩包预览

  async function openArchivePreview(entry) {
    var body = document.createElement('div');
    body.textContent = '正在读取压缩包…';
    var m = openModal({
      title: entry.name,
      body: body,
      wide: true,
      footer: modalFoot([
        { label: '关闭', onClick: function () { m.close(); } },
        // 看完内容通常就是想解压，把入口放在这里比退回列表再点更顺手。
        can('extract') && {
          label: '解压到…',
          cls: 'zip',
          onClick: function () {
            m.close();
            openExtractDialog(entry);
          }
        }
      ])
    });

    try {
      var res = await apiFetch(API + '?path=' + encodeURIComponent(entry.path));
      if (!res.ok) throw new Error(await res.text());
      var data = await res.json();
      body.textContent = '';

      var info = document.createElement('div');
      info.className = 'hint-box';
      info.style.marginBottom = '12px';
      var fmtName = { zip: 'ZIP', tar: 'TAR', 'tar.gz': 'TAR.GZ', gz: 'GZIP' }[data.format] || data.format;
      info.innerHTML = '';
      var f1 = document.createElement('div');
      f1.innerHTML = '格式：<b></b>　条目：<b></b>　默认解压到：<code></code>';
      f1.querySelectorAll('b')[0].textContent = fmtName;
      f1.querySelectorAll('b')[1].textContent = data.total;
      f1.querySelector('code').textContent = data.default_dest;
      info.appendChild(f1);
      body.appendChild(info);

      var table = document.createElement('table');
      table.className = 'zip-table';
      data.entries.forEach(function (en) {
        var tr = document.createElement('tr');
        if (en.is_dir) tr.className = 'is-dir';

        var tdName = document.createElement('td');
        tdName.className = 'z-name';
        tdName.textContent = (en.is_dir ? '📁 ' : '') + en.name;
        tr.appendChild(tdName);

        var tdSize = document.createElement('td');
        tdSize.className = 'z-size';
        tdSize.textContent = en.is_dir ? '—' : fmtSize(en.size);
        tr.appendChild(tdSize);

        var tdOp = document.createElement('td');
        tdOp.className = 'z-op';
        if (!en.is_dir) {
          var take = document.createElement('button');
          take.type = 'button';
          take.className = 'op-btn';
          take.dataset.kind = 'download';
          take.textContent = '取出';
          take.onclick = function () {
            take.disabled = true;
            take.textContent = '…';
            downloadFile(
              API + '?path=' + encodeURIComponent(entry.path) + '&file=' + encodeURIComponent(en.name),
              baseName(en.name)
            ).then(function () {
              toast('ok', '已取出 ' + baseName(en.name));
            }).catch(function (err) {
              toast('err', '取出失败：' + err.message);
            }).finally(function () {
              take.disabled = false;
              take.textContent = '取出';
            });
          };
          tdOp.appendChild(take);
        }
        tr.appendChild(tdOp);
        table.appendChild(tr);
      });
      body.appendChild(table);
    } catch (err) {
      body.textContent = '读取失败：' + err.message;
    }
  }

  // ---------------------------------------------------------------- 在线解压

  var DEST_CACHE = {};

  function openExtractDialog(entry) {
    var input = document.createElement('input');
    input.type = 'text';
    input.value = DEST_CACHE[entry.path] || defaultDest(entry.path);

    var field = document.createElement('div');
    field.className = 'field';
    var lb = document.createElement('label');
    lb.textContent = '解压到（相对服务根目录）';
    field.appendChild(lb);
    field.appendChild(input);
    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '目录不存在会自动创建；已存在且非空时需要勾选覆盖。';
    field.appendChild(hint);

    var owWrap = document.createElement('label');
    owWrap.className = 'checkbox';
    owWrap.style.marginTop = '12px';
    var ow = document.createElement('input');
    ow.type = 'checkbox';
    var owText = document.createElement('span');
    owText.textContent = '覆盖已存在的同名文件';
    owWrap.appendChild(ow);
    owWrap.appendChild(owText);

    var limits = DATA.extract_limits || {};
    var limitBox = document.createElement('div');
    limitBox.className = 'hint-box';
    limitBox.style.marginTop = '14px';
    limitBox.textContent = '安全上限：最多 ' + (limits.max_files || '-') + ' 个条目 / ' +
      fmtSize(limits.max_total_bytes || 0) + ' 总量 / 压缩比 ' + (limits.max_ratio || '-') + 'x。' +
      '包含软链接的条目会被自动跳过。';

    var wrap = document.createElement('div');
    wrap.appendChild(field);
    wrap.appendChild(owWrap);
    wrap.appendChild(limitBox);

    var m = openModal({
      title: '解压 ' + entry.name,
      body: wrap,
      footer: modalFoot([
        { label: '取消', onClick: function () { m.close(); } },
        { label: '开始解压', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    async function submit() {
      var dest = input.value.trim();
      if (!dest) { toast('warn', '请输入目标目录'); return; }
      DEST_CACHE[entry.path] = dest;
      m.close();
      startExtract(entry, dest, ow.checked);
    }

    input.onkeydown = function (e) { if (e.key === 'Enter') submit(); };
  }

  function defaultDest(p) {
    var dir = p.replace(/\/[^/]*$/, '') || '';
    var name = p.replace(/^.*\//, '');
    var exts = ['.tar.gz', '.tgz', '.tar', '.zip', '.gz'];
    for (var i = 0; i < exts.length; i++) {
      if (name.toLowerCase().endsWith(exts[i])) {
        name = name.slice(0, name.length - exts[i].length);
        break;
      }
    }
    if (!name) name = 'extracted';
    return (dir === '' ? '/' + name : dir + '/' + name);
  }

  // 解析 SSE 流并驱动进度条。
  async function startExtract(entry, dest, overwrite) {
    var bar = document.createElement('i');
    var pb = document.createElement('div');
    pb.className = 'progress-bar';
    pb.appendChild(bar);
    var meta = document.createElement('div');
    meta.className = 'progress-meta';
    var ml = document.createElement('span');
    var mr = document.createElement('span');
    meta.appendChild(ml);
    meta.appendChild(mr);
    var cur = document.createElement('div');
    cur.className = 'progress-current';
    var logBox = document.createElement('div');
    logBox.className = 'hint-box';
    logBox.style.marginTop = '14px';
    logBox.style.maxHeight = '160px';
    logBox.style.overflow = 'auto';
    logBox.textContent = '准备中…';

    var wrap = document.createElement('div');
    wrap.appendChild(pb);
    wrap.appendChild(meta);
    wrap.appendChild(cur);
    wrap.appendChild(logBox);

    var m = openModal({
      title: '解压 ' + entry.name,
      body: wrap,
      locked: true,
      footer: modalFoot([
        // 解压是后台任务，关掉弹窗不会中断它 —— 按钮文案如实说明这一点。
        { label: '后台运行', onClick: function () { m.close(); } }
      ])
    });

    function log(msg) {
      logBox.textContent = msg;
    }

    ml.textContent = '连接中…';
    mr.textContent = dest;

    var res;
    try {
      res = await apiFetch(API, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path: entry.path, dest: dest, overwrite: overwrite })
      });
    } catch (err) {
      logBox.textContent = '请求失败：' + err.message;
      ml.textContent = '失败';
      return;
    }

    if (!res.ok) {
      var msg = await res.text();
      console.error('[gofs] extract failed', res.status, msg);
      logBox.textContent = '解压失败（HTTP ' + res.status + '）：\n' + msg.trim();
      ml.textContent = '失败';
      bar.style.background = 'var(--danger)';
      bar.style.width = '100%';
      toast('err', '解压失败：' + msg.trim().split('\n')[0]);
      return;
    }

    var reader = res.body.getReader();
    var decoder = new TextDecoder();
    var buf = '';
    var total = 0;
    var started = Date.now();

    while (true) {
      var chunk = await reader.read();
      if (chunk.done) break;
      buf += decoder.decode(chunk.value, { stream: true });

      var idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        var block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        handleBlock(block);
      }
    }

    function handleBlock(block) {
      var ev = 'message', dataLine = '';
      block.split('\n').forEach(function (line) {
        if (line.indexOf('event:') === 0) ev = line.slice(6).trim();
        else if (line.indexOf('data:') === 0) dataLine += line.slice(5).trim();
      });
      var payload = {};
      try { payload = JSON.parse(dataLine || '{}'); } catch (e) { payload = {}; }

      if (ev === 'start') {
        total = payload.total || 0;
        pb.classList.add('indet');
        ml.textContent = total > 0 ? '0 / ' + total : '解压中…';
        mr.textContent = payload.format + ' → ' + payload.dest;
        log('开始解压，共 ' + total + ' 个条目');
      } else if (ev === 'progress') {
        var p = payload;
        if (total > 0) {
          pb.classList.remove('indet');
          bar.style.width = Math.min(100, (p.files / total) * 100).toFixed(1) + '%';
          ml.textContent = p.files + ' / ' + total + ' 个文件';
        } else {
          ml.textContent = '已写入 ' + p.files + ' 个文件';
        }
        mr.textContent = fmtSize(p.bytes) + (p.skipped ? '　跳过 ' + p.skipped : '');
        if (p.current) cur.textContent = p.current;
        if (!pb.classList.contains('indet')) {
          log('解压中：' + p.files + ' 个文件 / ' + fmtSize(p.bytes) +
            (p.skipped ? '，跳过 ' + p.skipped + ' 项' : ''));
        } else {
          log('解压中：' + p.files + ' 个文件 / ' + fmtSize(p.bytes));
        }
      } else if (ev === 'done') {
        var r = payload.report || {};
        pb.classList.remove('indet');
        bar.style.width = '100%';
        bar.style.background = 'var(--ok)';
        ml.textContent = '完成';
        mr.textContent = fmtDuration(r.elapsed_ms || (Date.now() - started));
        var lines = [];
        lines.push('解压完成：' + r.files + ' 个文件，' + r.dirs + ' 个目录，共 ' + fmtSize(r.bytes));
        lines.push('目标目录：' + (payload.url || dest));
        if (r.skipped) lines.push('跳过 ' + r.skipped + ' 项');
        if (r.warnings && r.warnings.length) {
          lines.push('警告：');
          r.warnings.slice(0, 8).forEach(function (w) { lines.push('  · ' + w); });
          if (r.warnings.length > 8) lines.push('  … 另有 ' + (r.warnings.length - 8) + ' 条');
        }
        logBox.textContent = lines.join('\n');
        closeBtn.textContent = '关闭';
        toast('ok', '解压完成：' + r.files + ' 个文件，共 ' + fmtSize(r.bytes));
        navigate(state.path, false);
      } else if (ev === 'error') {
        pb.classList.remove('indet');
        bar.style.width = '100%';
        bar.style.background = 'var(--danger)';
        ml.textContent = '失败';
        var pt = payload.partial || {};
        logBox.textContent = '解压失败：' + (payload.message || '未知错误') +
          '\n\n已写入：' + (pt.files || 0) + ' 个文件，' + fmtSize(pt.bytes || 0) +
          '\n目标目录：' + (pt.dest || dest);
        closeBtn.textContent = '关闭';
        toast('err', '解压失败：' + (payload.message || '未知错误'));
      }
    }
  }

  // ---------------------------------------------------------------- 主题

  function initTheme() {
    var saved = localStorage.getItem('gofs-theme') || 'auto';
    document.documentElement.setAttribute('data-theme', saved);
    $('btn-theme').onclick = function () {
      var order = ['auto', 'light', 'dark'];
      var cur = document.documentElement.getAttribute('data-theme') || 'auto';
      var next = order[(order.indexOf(cur) + 1) % order.length];
      document.documentElement.setAttribute('data-theme', next);
      localStorage.setItem('gofs-theme', next);
      toast('info', '主题：' + { auto: '跟随系统', light: '浅色', dark: '深色' }[next]);
    };
  }

  // ---------------------------------------------------------------- 事件绑定

  function bindEvents() {
    // 上传文件 / 上传文件夹：两个输入框共用同一段处理逻辑，
    // 区别只是 dir-input 带 webkitdirectory（浏览器会带上相对路径）。
    //
    // ⚠️ onchange 必须在这里一次性绑好，不能放在按钮的 onclick 里 ——
    // 投放区整块可点时也会直接调 $('file-input').click()，那条路径不经过
    // 按钮，onchange 漏绑就等于「选了文件什么也没发生」。
    function onPicked(e) {
      var files = Array.prototype.slice.call(e.target.files || []);
      // 清空 value，否则连续选择同一个目录时不会再触发 change。
      e.target.value = '';
      uploadFiles(files, state.path);
    }
    $('file-input').onchange = onPicked;
    $('dir-input').onchange = onPicked;

    $('btn-upload').onclick = function () { $('file-input').click(); };
    $('btn-upload-dir').onclick = function () { $('dir-input').click(); };

    // ---- 顶栏「更多」折叠菜单（仅手机端可见） ----
    //
    // 桌面端 .more-wrap/.more-panel 是 display: contents，菜单项本来就摆在
    // 顶栏上，这个开关点不到也不影响任何东西；手机端才把低频操作收进来。
    //
    // 三个「关闭」路径都要有，缺一个就会留下一个关不掉的浮层：
    // 再点一次开关、点面板外的任意位置、按 Esc。
    var moreWrap = $('more-wrap');
    var morePanel = $('more-panel');
    var moreToggle = $('btn-more');

    function closeMore() {
      if (!morePanel) return;
      morePanel.classList.remove('open');
      if (moreToggle) moreToggle.setAttribute('aria-expanded', 'false');
    }
    function toggleMore() {
      if (!morePanel) return;
      var open = morePanel.classList.toggle('open');
      if (moreToggle) moreToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    }

    if (moreToggle) {
      moreToggle.onclick = function (e) {
        e.stopPropagation();   // 别让下面的 document 监听立刻把它关掉
        toggleMore();
      };
    }
    // 点菜单项后立刻收起：否则改完设置回到页面，菜单还挂在那儿。
    if (morePanel) {
      morePanel.addEventListener('click', function (e) {
        var b = e.target.closest ? e.target.closest('button') : null;
        if (b && !b.hidden) closeMore();
      });
    }
    document.addEventListener('click', function (e) {
      if (!moreWrap || moreWrap.contains(e.target)) return;
      closeMore();
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') closeMore();
    });
    // 换目录时菜单不该跨页留着。
    G.closeMore = closeMore;

    // 旋转屏幕 / 拖动窗口跨过 560px 断点时，有三处文案是按屏宽选的
    // （工具条统计口径、页脚能力、投放区那句话），要重算一次；
    // 不重算就得刷新页面才正确。只在真的跨过断点时才算，避免拖动时抖动。
    var wasNarrow = isNarrow();
    window.addEventListener('resize', function () {
      var now = isNarrow();
      if (now === wasNarrow) return;
      wasNarrow = now;
      syncSelection();
      renderFooter();
      renderDropZone();
    });

    $('btn-users').onclick = function () { G.openUsers(); };
    $('btn-mkdir').onclick = openMkdirDialog;
    $('btn-refresh').onclick = function () { navigate(state.path, false); };

    // 打包下载：默认打包整个目录；一旦勾选了条目，就只打包所选。
    $('btn-zip').onclick = function () {
      var dirName = baseName(state.path) || 'root';
      var picked = pickedEntries();
      var label = picked.length ? ('所选 ' + picked.length + ' 项') : dirName;
      var t = progressToast('打包 ' + label);

      var onProgress = function (loaded, total) { t.update(loaded, total); };
      var task;
      if (picked.length) {
        // 用 POST 携带选中列表：选中几百个文件时 URL 会超长被网关拒绝。
        task = downloadFile(fileURL(state.path) + '?zip', '', onProgress, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ picks: picked.map(function (e) { return e.path; }) })
        });
      } else {
        task = downloadFile(fileURL(state.path) + '?zip', '', onProgress);
      }

      task.then(function (res) {
        t.done('已打包 ' + label);
        var skipped = parseInt(res.headers.get('x-gofs-archive-skipped') || '0', 10);
        if (skipped > 0) {
          // 服务端跳过了部分选中项（不存在 / 越界 / 无权限），如实告知。
          toast('warn', '有 ' + skipped + ' 项被跳过（不存在或没有读取权限）');
        }
      }).catch(function (err) {
        t.fail('打包失败：' + err.message);
      });
    };

    $('btn-keys').onclick = function () { G.openKeys(); };
    $('btn-settings').onclick = function () { G.openSettings(); };

    $('btn-delete-sel').onclick = function () {
      var picked = pickedEntries();
      if (picked.length) doDelete(picked);
    };

    $('check-all').onchange = function (e) {
      state.selected.clear();
      if (e.target.checked) state.entries.forEach(function (x) { state.selected.add(x.path); });
      renderTable();
    };
    document.querySelectorAll('.listing th.sortable').forEach(function (th) {
      th.onclick = function () {
        var k = th.dataset.sort;
        if (state.sortKey === k) state.sortDir = -state.sortDir;
        else { state.sortKey = k; state.sortDir = 1; }
        renderTable();
      };
    });

    // 搜索：输入防抖
    var timer = null;
    $('search-input').oninput = function (e) {
      var v = e.target.value.trim();
      clearTimeout(timer);
      timer = setTimeout(function () { doSearch(v); }, 260);
    };
    $('search-input').onkeydown = function (e) {
      if (e.key === 'Escape') { e.target.value = ''; doSearch(''); }
    };

    // 拖拽上传。两层提示：
    //   - 常驻的 .drop-zone 让「可以拖」这件事在没有拖拽时也能看见；
    //   - 拖进窗口后的全屏浮层显示**落点**，避免拖到别的目录里去。
    var dragDepth = 0;

    // dropFileCount 尽量数出正在拖的条目数量。
    // dragenter 阶段 dataTransfer.files 还是空的，只有 items 可用。
    // 数的是「拖进来的顶层条目」——一个文件夹算 1 项，展开后可能是几百个文件，
    // 真实数量要等 drop 之后异步遍历才知道（那时会另开进度弹窗）。
    function dropFileCount(dt) {
      return countDroppedItems(dt);
    }

    function dragHasFiles(dt) {
      if (!dt || !dt.types) return false;
      return Array.prototype.indexOf.call(dt.types, 'Files') >= 0;
    }

    function showDropHint(n) {
      var dir = uploadTargetDir(state.path);
      $('drop-target').textContent = '存入 ' + (dir === '/' ? '/' : dir + '/');
      var notes = [];
      if (n > 0) notes.push(n + ' 项');
      var lim = uploadLimitText();
      if (lim) notes.push(lim);
      $('drop-note').textContent = notes.join('　·　');
      $('drop-hint').classList.add('on');
      $('drop-zone').classList.add('on');
    }

    function hideDropHint() {
      $('drop-hint').classList.remove('on');
      $('drop-zone').classList.remove('on');
    }

    window.addEventListener('dragenter', function (e) {
      if (!can('write')) return;
      if (!dragHasFiles(e.dataTransfer)) return;
      e.preventDefault();
      dragDepth++;
      showDropHint(dropFileCount(e.dataTransfer));
    });
    window.addEventListener('dragover', function (e) {
      if (can('write')) e.preventDefault();
    });
    window.addEventListener('dragleave', function (e) {
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) hideDropHint();
    });
    window.addEventListener('drop', async function (e) {
      if (!can('write')) return;
      e.preventDefault();
      dragDepth = 0;
      hideDropHint();
      // ⚠️ dataTransfer.items 只在事件派发期间有效，必须先同步取出来，
      // 再交给异步的目录遍历（否则 items 已经清空，拖文件夹会什么都拿不到）。
      var picked = await pickDroppedFiles(e.dataTransfer);
      if (picked.skipped) toast('warn', '有 ' + picked.skipped + ' 个条目没有可用的文件名，已跳过');
      if (picked.files.length) uploadFiles(picked.files, state.path);
    });

    // 常驻投放区整块可点：点哪儿都能打开文件选择框，符合网盘的直觉。
    // 「选择文件夹…」按钮要 stopPropagation，否则会连带触发外层的选文件。
    $('drop-zone').onclick = function () { $('file-input').click(); };
    $('drop-zone-pick').onclick = function (e) { e.stopPropagation(); $('file-input').click(); };
    $('drop-zone-dir').onclick = function (e) { e.stopPropagation(); $('dir-input').click(); };

    // 浏览器前进/后退
    window.addEventListener('popstate', function (e) {
      var p = (e.state && e.state.path) || pathFromLocation();
      navigate(p, false);
    });

    // 快捷键
    document.addEventListener('keydown', function (e) {
      if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
      if (e.key === 'r' && !e.metaKey && !e.ctrlKey) navigate(state.path, false);
      if (e.key === '/' ) { e.preventDefault(); $('search-input').focus(); }
    });
  }

  function pathFromLocation() {
    var p = decodeURIComponent(location.pathname);
    if (BASE && p.indexOf(BASE) === 0) p = p.slice(BASE.length);
    return p || '/';
  }

  // ---------------------------------------------------------------- 启动

  creds = loadCreds();

  // 刷新页面时浏览器不会自动带 Authorization 头，服务端只会返回空壳，
  // 里面既没有用户名、权限位也全是 false。先把本地凭据里的用户名填上，
  // 免得界面闪一下「匿名」、登出按钮也跟着消失。
  if (creds && DATA.auth_required) {
    DATA.user = creds.user;
    DATA.auth_on = true;
  }

  initTheme();
  bindEvents();

  if (DATA.auth_required) {
    // 服务端只返回了空壳：先渲染骨架，再决定是恢复会话还是弹登录框。
    render();
    if (creds) {
      resumeSession().then(function (ok) {
        if (!ok) {
          if (!$('login-mask')) showLogin('保存的登录已失效，请重新登录');
          return;
        }
        // 补上用户名、登出按钮与各功能入口的显隐。
        renderPerms();
        navigate(state.path || '/', false);
      });
    } else {
      showLogin('');
    }
  } else {
    render();
  }

  // 页脚与权限相关的显隐由 render() -> renderPerms() -> renderFooter() 负责，
  // 这里不再单独算一次 —— 之前那份「只在启动时执行」的代码就是
  // 「登录后仍显示只读模式」的原因。

  // 暴露给 editor.js / keys.js 复用，避免它们重复实现请求封装与 UI 组件。
  G.apiFetch = apiFetch;
  G.authHeaders = authHeaders;
  G.toast = toast;
  G.progressToast = progressToast;
  G.openModal = openModal;
  G.modalFoot = modalFoot;
  G.confirm = confirmDialog;
  G.btn = btn;
  G.fmtSize = fmtSize;
  G.fmtTime = fmtTime;
  G.fmtDuration = fmtDuration;
  G.fileURL = fileURL;
  G.raw = raw;
  G.encPath = encPath;
  G.baseName = baseName;
  G.joinPath = joinPath;
  G.navigate = navigate;
  G.refresh = function () { return navigate(state.path, false); };
  G.downloadFile = downloadFile;
  G.downloadEntry = downloadEntry;
  G.state = state;
  G.data = DATA;
  G.can = can;
  G.allowKeys = allowKeys;
  G.allowUsers = allowUsers;
  G.isNarrow = isNarrow;
  G.canEdit = canEdit;
  G.editMax = editMax;
  G.applySession = applySession;
})();
