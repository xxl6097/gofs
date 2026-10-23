/* gofs 设置面板：常规（服务根目录 / 上传上限 / 编辑上限）、上传密钥、
 * 用户与权限、升级与重启，四块做成标签页。
 *
 * 为什么合成一个入口：这四块都是管理员/高权限操作，平铺在工具栏上既占位
 * 又零散；而且升级与重启挤在设置页底部会把弹窗拉得很长。分标签之后
 * 每块只在自己那一页出现，底部按钮也跟着当前页变（常规是「取消/保存」，
 * 其余是「关闭」，用户页多一个「新建用户」）。
 *
 * 密钥与用户的内容来自 keys.js / users.js 的 keysPanel()/usersPanel()，
 * 这里只负责标签外壳与底部按钮 —— 那两个文件不再自己开弹窗。
 *
 * 权限：标签按子功能可用性显示。只要任一项可用就能打开本弹窗 ——
 * 一个只对 /docs 可写的账号能管密钥，但看不到「常规」（那需要服务根权限，
 * 服务端 requireAdmin 也会再校验一次，界面隐藏只是不让无关的人误点）。 */
(function () {
  'use strict';

  var G = window.GOFS = window.GOFS || {};

  var BASE = (G.data && G.data.path_prefix) || '';
  var EP = BASE + '/fs/settings';
  var EP_UPGRADE = BASE + '/fs/upgrade';
  var EP_RESTART = BASE + '/fs/restart';
  var EP_UNINSTALL = BASE + '/fs/uninstall';
  // 重启期间要靠它判断「服务回来没有」。这个接口不需要鉴权，是故意挑的。
  var EP_HEALTH = BASE + '/fs/health';

  var UNITS = [
    // KiB 是给「在线编辑上限」用的：512 KiB 这类取值很常见，
    // 单位表从 MiB 起的话，它会显示成 0.0 GiB。
    { label: 'KiB', factor: 1024 },
    { label: 'MiB', factor: 1024 * 1024 },
    { label: 'GiB', factor: 1024 * 1024 * 1024 },
    { label: 'TiB', factor: 1024 * 1024 * 1024 * 1024 }
  ];

  // pickUnit 选一个让数值落在 1~1024 之间的单位，避免显示成
  // 「10737418240 B」这种要数零的写法。
  function pickUnit(bytes) {
    var best = UNITS[0];
    for (var i = 0; i < UNITS.length; i++) {
      if (bytes >= UNITS[i].factor) best = UNITS[i];
    }
    return best;
  }

  // splitSize 把字节数拆成「数值 + 单位」。
  function splitSize(bytes) {
    if (!bytes) return { value: '', unit: UNITS[1] };
    var u = pickUnit(bytes);
    var v = bytes / u.factor;
    // 整数就不带小数点，否则保留一位（10.5 GiB 比 10.5000001 GiB 好读）。
    return { value: Number.isInteger(v) ? String(v) : v.toFixed(1), unit: u };
  }

  // makeSizeField 造一个「数值 + 单位 + 不限制」的大小输入控件。
  //
  // 上传上限与编辑上限长得一模一样，抽出来一份：两段几乎相同的代码
  // 各写一遍，改了一处忘了另一处是迟早的事。
  //
  // 返回 { field, read, refresh }：
  //   read()    读出字节数，0 表示不限制，NaN 表示输入非法
  //   refresh() 重算提示文案（当前值 + 启动默认值 + 恢复默认按钮）
  function makeSizeField(opts) {
    var field = makeField(opts.label);
    var row = document.createElement('div');
    row.className = 'row';

    var initial = splitSize(opts.value);
    var input = textInput(opts.value ? initial.value : '');
    input.style.flex = '1';
    input.style.minWidth = '0';
    input.placeholder = opts.placeholder || '';
    row.appendChild(input);

    var unitSel = document.createElement('select');
    unitSel.className = 'select';
    UNITS.forEach(function (u) {
      var o = document.createElement('option');
      o.value = String(u.factor);
      o.textContent = u.label;
      if (u.label === initial.unit.label) o.selected = true;
      unitSel.appendChild(o);
    });
    row.appendChild(unitSel);

    // 有些上限没有「不限制」这一档（编辑上限就是 —— 服务端只收正整数）。
    // 界面上就不该给出这个选项，否则用户勾了会拿到 400。
    var allowUnlimited = opts.allowUnlimited !== false;
    var cb = document.createElement('input');
    cb.type = 'checkbox';
    if (allowUnlimited) {
      var unlimited = document.createElement('label');
      unlimited.className = 'checkbox';
      cb.checked = !opts.value;
      var text = document.createElement('span');
      text.textContent = '不限制';
      unlimited.appendChild(cb);
      unlimited.appendChild(text);
      row.appendChild(unlimited);
    }

    field.appendChild(row);
    var hint = addHint(field, '');

    function syncInputs() {
      input.disabled = cb.checked;
      unitSel.disabled = cb.checked;
    }

    function fmt(v) { return v > 0 ? G.fmtSize(v) : '不限制'; }

    function refresh() {
      hint.textContent = '';
      // 当前服务端值放到字段标题旁；提示区只留“这个设置怎么工作”。
      setCurrentChip(field, fmt(opts.current()));
      // 只有当前值和启动默认值不同（或输入了非法值）时才展示默认值与
      // 「恢复」入口。两者都相等时再写一遍「默认值：10 GiB」只是噪音。
      var changed = !isFinite(read()) || read() !== opts.def;
      hint.appendChild(document.createTextNode(
        (allowUnlimited && cb.checked) ? opts.unlimitedHint : opts.limitedHint));
      if (opts.persistHint) {
        hint.appendChild(document.createTextNode('　·　' + opts.persistHint));
      }

      if (changed) {
        addDefaultMeta(hint, fmt(opts.def), function () {
          var d = splitSize(opts.def);
          if (allowUnlimited) cb.checked = opts.def === 0;
          input.value = d.value;
          for (var i = 0; i < unitSel.options.length; i++) {
            if (unitSel.options[i].value === String(d.unit.factor)) unitSel.selectedIndex = i;
          }
          syncInputs();
          refresh();
        });
      }
    }

    function read() {
      if (allowUnlimited && cb.checked) return 0;
      var v = parseFloat(String(input.value).replace(/,/g, '').trim());
      if (!isFinite(v) || v < 0) return NaN;
      var factor = parseInt(unitSel.value, 10) || 1;
      return Math.round(v * factor);
    }

    cb.onchange = function () { syncInputs(); refresh(); };
    input.oninput = refresh;
    unitSel.onchange = refresh;

    syncInputs();
    refresh();
    return { field: field, read: read, refresh: refresh };
  }

  // makeTextField 造一个「一行文本 + 当前生效值 + 恢复默认」的输入控件。
  //
  // 与 makeSizeField 同构：两者都要「当前生效 / 启动参数默认值 / 一键恢复」
  // 这三件信息，交互保持一致，用户不必学两套。
  //
  // 返回 { field, read, refresh }：read() 已 trim，空串交给服务端去报错
  // （服务端才知道规则，前端只负责别把空白当有效输入）。
  function makeTextField(opts) {
    var field = makeField(opts.label);
    var row = document.createElement('div');
    row.className = 'row';

    var input = textInput(opts.value || '');
    input.style.flex = '1';
    input.style.minWidth = '0';
    input.placeholder = opts.def || '';
    // 与服务端的 maxTitleRunes 对齐：让它在前端就被拦住，
    // 而不是写完点保存才吃一个 400。
    if (opts.maxLength) input.maxLength = opts.maxLength;
    row.appendChild(input);
    field.appendChild(row);

    var hint = addHint(field, '');

    function refresh() {
      hint.textContent = '';
      // 走 setRichText：这些说明里带着 `**立即**` 这类强调标记，
      // 用 createTextNode 贴上去用户看到的是两颗星号（见 app.js 的说明）。
      G.setRichText(hint, opts.hintText);
      // 当前值放标题旁，说明里不再重复一遍。
      setCurrentChip(field, opts.current());

      // 同 makeSizeField：默认值没被动过时，不显示默认值也不显示恢复入口。
      if (String(input.value).trim() !== String(opts.def || '').trim()) {
        addDefaultMeta(hint, opts.def || '（空）', function () {
          input.value = opts.def || '';
          refresh();
        });
      }
    }

    input.oninput = refresh;
    refresh();
    return {
      field: field,
      read: function () { return String(input.value).trim(); },
      refresh: refresh
    };
  }

  function makeField(labelText) {
    var wrap = document.createElement('div');
    wrap.className = 'field';
    // 标题行右侧留给「当前生效」chip。以前这句话跟在说明末尾，
    // 每个字段都要读一整段才能在最后找到当前值。
    var head = document.createElement('div');
    head.className = 'field-head';
    var lb = document.createElement('label');
    lb.textContent = labelText;
    var meta = document.createElement('span');
    meta.className = 'field-current';
    meta.hidden = true;
    head.appendChild(lb);
    head.appendChild(meta);
    wrap.appendChild(head);
    wrap._fieldMeta = meta;
    return wrap;
  }

  // setCurrentChip 把服务端当前生效值写成字段标题旁的胶囊。
  function setCurrentChip(field, value) {
    var chip = field && field._fieldMeta;
    if (!chip) return;
    var isDefault = (value === '' || value === null || value === undefined);
    var text = isDefault ? '（默认）' : String(value);
    chip.textContent = isDefault ? '当前（默认）' : '当前 ' + text;
    chip.title = '当前生效：' + text;
    chip.hidden = false;
  }

  function addHint(wrap, text) {
    var d = document.createElement('div');
    d.className = 'hint';
    // 统一走 setRichText：说明文字里的 `**强调**` 要渲染成加粗，
    // 而不是把两颗星号原样摆给用户看（见 app.js 的说明）。
    G.setRichText(d, text);
    wrap.appendChild(d);
    return d;
  }

  // addDefaultMeta 生成「启动默认值 + 恢复默认」这一行。
  // 以前它是一段文字加一个带 <br> 的按钮，读起来像说明的一部分；
  // 收成独立的一行后，说明归说明，恢复操作归操作。
  function addDefaultMeta(hint, defaultValue, onReset) {
    var row = document.createElement('div');
    row.className = 'field-meta';
    var label = document.createElement('span');
    label.textContent = '启动默认值：' + defaultValue;
    var quick = document.createElement('button');
    quick.type = 'button';
    quick.className = 'btn btn-sm ghost reset-control';
    quick.textContent = '恢复默认';
    quick.onclick = onReset;
    row.appendChild(label);
    row.appendChild(quick);
    hint.appendChild(row);
  }

  // addDisclosure 把低频但重要的长说明折进原生 <details>。
  // 默认只露出第一行警告，需要时再展开；比把 3 行灰字直接铺在表单里清爽。
  function addDisclosure(wrap, summaryText, bodyText) {
    var d = document.createElement('details');
    d.className = 'hint-disclosure';
    var s = document.createElement('summary');
    s.textContent = summaryText;
    d.appendChild(s);
    var body = document.createElement('div');
    body.className = 'hint';
    G.setRichText(body, bodyText);
    d.appendChild(body);
    wrap.appendChild(d);
    return d;
  }

  // addSection 在长表单里插入分组标题。设置项本身不变，只是让用户
  // 能先看到「这一段在管什么」，而不是一路往下读说明。
  function addSection(wrap, text) {
    var d = document.createElement('div');
    d.className = 'settings-section-title';
    d.textContent = text;
    wrap.appendChild(d);
    return d;
  }

  function textInput(value) {
    var input = document.createElement('input');
    input.type = 'text';
    input.value = value || '';
    input.spellcheck = false;
    input.autocomplete = 'off';
    return input;
  }

  // ---------------------------------------------------------------- 数据

  // fetchSettings 读取当前服务设置。需要管理员权限，所以只有「常规」
  // 面板会调它 —— 那个面板只对管理员出现，非管理员不会白白撞一次 403。
  async function fetchSettings() {
    var res = await G.apiFetch(EP, { headers: { 'Accept': 'application/json' } });
    if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
    return res.json();
  }

  // 关于「跳转前先等新地址起来」这件事：
  //
  // 重启那套（buildRestartBox）是**同源**轮询 /fs/health，没问题；
  // 但改监听地址**不能**照搬。改端口就意味着换 origin，
  // 而换 origin 就是跨域请求 —— 服务端只在 --enable-cors 时才发
  // Access-Control-Allow-Origin（默认关），于是这里的 fetch 一定被浏览器
  // 拦下、promise 一定 reject，轮询永远等不到结果：白等 10 秒，再报一句
  // 「新地址没有应答」的假警报（服务其实好好的）。也轮询不了：跨域下我们
  // 拿不到对方的任何响应，这恰恰是浏览器该拦的。
  //
  // 所以那条路径直接跳转。可以放心：服务端的 Relisten 是「新监听建好并开始
  // serve 之后」才返回的，跳过去时端口已经在 accept 了；万一真连不上，
  // 浏览器自己会给出比我们编的文案更准的错误页。
  //
  // 这里的轮询留给**同源**的场景（重启：同 origin 的 /fs/health）。

  // ---------------------------------------------------------------- 各标签

  // buildSitesField 造「附加站点」那一块：列出已挂的，允许增删。
  //
  // 增删是**即时动作** —— 直接改服务端状态并返回新列表，不并入「取消/保存」。
  // 塞进 patch/dirty 那套就得做待定操作与回滚，而站点本来就没有「草稿」概念。
  //
  // onChanged 由调用方给：增删可能改变「主目录能否切换」（有站点时不能），
  // 主目录那一栏得跟着重画。
  function buildSitesField(onChanged) {
    var field = makeField('附加站点');

    var listBox = document.createElement('div');
    listBox.className = 'site-list';
    field.appendChild(listBox);

    // 独立的错误行：这里的失败（不在允许范围、目录不存在、互相嵌套…）
    // 与「保存」那套是两回事，混用一个 errBox 会互相盖掉。
    var siteErr = document.createElement('div');
    siteErr.className = 'hint-box danger';
    siteErr.hidden = true;
    field.appendChild(siteErr);

    // 提示只建一次、只改文字：每次重画都 addHint 会堆出一摞。
    var siteHint = addHint(field, '');

    // 新增表单也建一次，靠 editable 控制显隐（避免每次重画重建 DOM）。
    var form = document.createElement('div');
    form.className = 'site-add';
    var pathInput = textInput('');
    pathInput.placeholder = '要新增的目录绝对路径，如 /srv/backup';
    pathInput.className = 'key-input';
    var labelInput = textInput('');
    labelInput.placeholder = '标签（可留空，默认用目录名）';
    labelInput.className = 'key-input';
    var addBtn = G.btn('新增站点', 'primary', function () { submit(); });
    form.appendChild(pathInput);
    form.appendChild(labelInput);
    form.appendChild(addBtn);
    field.appendChild(form);

    function fail(msg) {
      siteErr.hidden = false;
      siteErr.textContent = msg;
    }

    function act(body) {
      siteErr.hidden = true;
      return G.apiFetch(EP, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      }).then(function (res) {
        return res.text().then(function (text) {
          var t = (text || '').trim();
          if (!res.ok) throw new Error(t || ('HTTP ' + res.status));
          return JSON.parse(t);
        });
      });
    }

    function reload() {
      return G.apiFetch(EP, { headers: { 'Accept': 'application/json' } })
        .then(function (res) { return res.json(); });
    }

    function submit() {
      var p = pathInput.value.trim();
      if (!p) { fail('请填写目录路径'); return; }
      addBtn.disabled = true;
      addBtn.textContent = '新增中…';
      act({ add_site: { path: p, label: labelInput.value.trim() } })
        .then(function (data) {
          pathInput.value = '';
          labelInput.value = '';
          render(data);
        })
        .catch(function (e) { fail('新增失败：' + e.message); })
        .then(function () {
          addBtn.disabled = false;
          addBtn.textContent = '新增站点';
        });
    }

    function remove(label) {
      G.confirm({
        title: '移除站点 ' + label + '？',
        message: '只是不再对外提供这个目录，磁盘上的文件不受影响。',
        okLabel: '移除',
        danger: true
      }).then(function (ok) {
        if (!ok) return;
        act({ remove_site: label }).then(render).catch(function (e) {
          fail('移除失败：' + e.message);
        });
      });
    }

    // render 以服务端返回的设置为准重画 —— 不在本地猜结果。
    function render(data) {
      listBox.textContent = '';
      var extras = (data.roots || []).filter(function (r) { return !r.primary; });
      var editable = !!data.sites_editable;

      if (!extras.length) {
        var empty = document.createElement('div');
        empty.className = 'hint';
        empty.textContent = '还没有附加站点：每个目录挂在 /<标签>/ 下，与主目录互不影响。';
        listBox.appendChild(empty);
      }

      extras.forEach(function (r) {
        var line = document.createElement('div');
        line.className = 'mount-line';
        var code = document.createElement('code');
        code.textContent = r.url + '/';
        line.appendChild(code);
        var span = document.createElement('span');
        span.textContent = r.root;
        line.appendChild(span);
        if (editable) {
          var rm = G.btn('移除', 'danger', function () { remove(r.label); });
          rm.classList.add('btn-sm');
          line.appendChild(rm);
        }
        listBox.appendChild(line);
      });

      form.hidden = !editable;
      // 可挂范围：默认只允许启动根的同级目录；--root-allow / --allow-any-site
      // 决定最终范围。返回空数组表示不限制（allow-any-site）。
      var roots = data.site_roots || [];
      G.setRichText(siteHint, editable
        ? '新增的目录挂在 /<标签>/ 下，**立即生效**（不必点保存）。' +
          (roots.length ? '可挂范围：' + roots.join('、') : '可挂任意已存在的目录') +
          '　·　注意：标签会遮蔽主目录下的同名子目录。'
        : '本服务已通过 --no-root-switch 禁止运行期改动服务目录。');

      if (onChanged) onChanged(data);
    }

    reload().then(render).catch(function (e) {
      fail('读取站点列表失败：' + e.message);
    });

    // refresh 供外部（保存成功后）重新拉取。
    return { field: field, refresh: function () { return reload().then(render); } };
  }

  // buildGeneralTab 造「常规」页：服务根目录 / 上传上限 / 编辑上限。
  //
  // 同步返回、内部异步读取：先摆一个「正在读取…」占位，好让弹窗的底部
  // 按钮（取消 / 保存）立刻到位，不用等网络往返。
  function buildGeneralTab(ctx) {
    var el = document.createElement('div');

    var loading = document.createElement('div');
    loading.className = 'hint-box';
    loading.textContent = '正在读取当前设置…';
    el.appendChild(loading);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;

    var cur = null;
    var rootInput = null;
    var rootCurRoot = '';
    var uploadField = null;
    var editField = null;
    var titleField = null;
    var prefixField = null;
    var listenAddrField = null;
    var listenPortField = null;

    function showErr(msg) {
      errBox.hidden = false;
      errBox.textContent = msg;
    }

    // fill 拿到设置之后再把表单铺出来。
    function fill() {
      el.textContent = '';

      // ---- 站点标题 ----
      //
      // 放在最前面：它是最「面向人」的一项，改完立刻能看到变化。
      titleField = makeTextField({
        label: '站点标题',
        value: cur.title,
        def: cur.title_default,
        current: function () { return cur.title; },
        maxLength: 60,
        hintText: '显示在浏览器标签页与顶栏品牌名上；保存后本页生效，' +
          '其它已打开的页面需刷新。'
      });
      el.appendChild(titleField.field);

      // ---- 访问路径前缀 ----
      //
      // 放在标题后面：它管的也是「这个站点长什么样」，只是管的是地址那一半。
      addSection(el, '访问地址');
      //
      // 这是本页唯一一个**改错了会把自己锁在门外**的设置 —— 前缀必须与反向
      // 代理（nginx）的 location 一致，改完不同步改代理的话，新旧两个前缀下的
      // 页面都打不开，而页面正是改回它的唯一入口。所以提示写得重，保存前还要
      // 再确认一次（见 save）。
      prefixField = makeTextField({
        label: '访问路径前缀',
        value: cur.path_prefix,
        def: cur.path_prefix_default,
        current: function () { return cur.path_prefix; },
        hintText: '留空表示服务在根路径（/）；改错会让新旧地址都打不开。'
      });
      el.appendChild(prefixField.field);
      addDisclosure(prefixField.field, '反向代理下怎么填？',
        '挂在 nginx 等反向代理的子路径下时，这里必须与代理的 location 一致；' +
        '保存后本页会跳到新地址，在代理改好之前它会打不开。');
      if (cur.path_prefix_locked) {
        addHint(prefixField.field,
          '启动参数优先：这里的改动只在本次运行有效，重启后会回到参数值。');
      }

      // ---- 监听地址 / 端口 ----
      //
      // 与上一项一样是「改错了会把自己锁在门外」的设置，而且它的连带面更大：
      // 端口往往还牵着 Docker 端口映射、防火墙、systemd unit、反代的 upstream。
      // 服务端只在「当前请求确实是直达监听端口」时才给跳转地址（见
      // listen_jump_url），其余情况（unix socket、前面挡着代理）只提示不跳。
      if (cur.listen_editable) {
        // 监听地址/端口是低频且高风险的设置（牵扯 Docker 端口映射、防火墙、
        // systemd、反代 upstream），默认收起来，摘要里直接给出当前端点。
        var listenAddr = cur.listen_addr || '0.0.0.0';
        var endpoint = listenAddr.charAt(0) === '/'
          ? 'unix socket'
          : listenAddr + ':' + (cur.listen_port || '系统分配');
        var adv = document.createElement('details');
        adv.className = 'settings-advanced';
        var advSummary = document.createElement('summary');
        advSummary.textContent = '高级网络设置 · ' + endpoint;
        adv.appendChild(advSummary);
        var advBody = document.createElement('div');
        advBody.className = 'settings-advanced-body';
        adv.appendChild(advBody);

        listenAddrField = makeTextField({
          label: '监听地址',
          value: cur.listen_addr,
          def: cur.listen_addr_default,
          current: function () { return cur.listen_addr; },
          hintText: '留空表示所有网卡（0.0.0.0）。填 / 开头的绝对路径则改为监听 ' +
            'unix socket（如 /run/gofs.sock）。'
        });
        advBody.appendChild(listenAddrField.field);

        listenPortField = makeTextField({
          label: '监听端口',
          value: String(cur.listen_port),
          def: String(cur.listen_port_default),
          current: function () { return cur.listen_port; },
          hintText: '保存后**立即**切到新端口：已有传输不断，新连接走新端口。' +
            '0 表示由系统分配；监听 unix socket 时本项无效。'
        });
        advBody.appendChild(listenPortField.field);
        if (cur.listen_locked) {
          addHint(listenPortField.field,
            '启动参数优先：这里的改动只在本次运行有效，重启后会回到参数值。');
        }
        el.appendChild(adv);
      } else {
        // 不显示那两个输入框，但把原因说清楚 —— 直接消失会让人以为功能没了。
        addHint(el, '本实例不支持在页面上改监听地址：它没有自己的监听' +
          '（以库方式嵌在别的 HTTP 服务里，监听归宿主管）。');
      }

      // ---- 服务根目录（主目录）----
      //
      // 主目录永远服务在 `/`；附加站点挂在 /<标签>/ 下，是**运行期**可增删的，
      // 而且增删不动主目录的地址（否则加个站点就让所有书签失效）。
      //
      // 这一栏的文字与禁用状态随「有没有附加站点」变（有站点时不能换主根），
      // 所以抽成 syncRootField，增删站点后重画一次。
      addSection(el, '服务目录');
      var rootField = makeField('主服务目录');
      var rootRow = document.createElement('div');
      rootRow.className = 'row';
      rootInput = textInput(cur.root || '');
      rootInput.placeholder = cur.root || '/srv/data';
      rootRow.appendChild(rootInput);
      rootField.appendChild(rootRow);
      var rootHint = addHint(rootField, '');

      function syncRootField(data) {
        var c = data || cur;
        if (!c) return;
        if (rootInput.value === '' || rootInput.value === rootCurRoot) {
          rootInput.value = c.root || '';
        }
        rootCurRoot = c.root || '';
        rootInput.placeholder = c.root || '/srv/data';
        rootInput.disabled = !c.root_switchable;
        rootHint.textContent = c.root_switchable
          ? '服务在 /　·　可切换的范围：' + (c.allow_roots || []).join('、') +
            '　·　填相对路径表示「从当前根再往下走」'
          : (c.multi_root
              ? '服务在 /　·　有附加站点时不能切换主目录；先移除附加站点再换'
              : '本服务已通过 --no-root-switch 禁止运行期切换根目录');
      }
      syncRootField(cur);
      el.appendChild(rootField);

      // ---- 附加站点（即时生效，不跟着「取消/保存」走）----
      var sites = buildSitesField(function (data) { syncRootField(data); });
      el.appendChild(sites.field);

      // ---- 单文件上传上限 ----
      addSection(el, '上传与编辑');
      uploadField = makeSizeField({
        label: '单文件上传上限',
        value: cur.upload_max_size,
        def: cur.upload_max_size_default,
        current: function () { return cur.upload_max_size; },
        placeholder: '例如 10',
        unlimitedHint: '不限制单文件大小 —— 只要磁盘放得下就能传。',
        limitedHint: '超过该大小的上传会被拒绝（HTTP 413），且不会留下残缺文件。',
        persistHint: '改过之后会记住，重启仍生效。'
      });
      el.appendChild(uploadField.field);

      // ---- 在线编辑大小上限 ----
      //
      // 超过这个大小的文件不提供「编辑」入口，服务端也会拒绝读写 ——
      // 编辑器要把整个文件读进内存，放开等于给一个几百 MB 的文件留个口子。
      editField = makeSizeField({
        label: '在线编辑大小上限',
        value: cur.edit_max_size,
        def: cur.edit_max_size_default,
        current: function () { return cur.edit_max_size; },
        placeholder: '例如 2',
        // 编辑上限没有「不限制」：打开文件要把整份内容读进内存，
        // 放开等于给一个几 GB 的「文本」文件留个口子。服务端也只收正整数。
        allowUnlimited: false,
        limitedHint: '超过该大小的文件不显示「编辑」入口，服务端也会拒绝读写。',
        persistHint: '改过之后会记住，重启仍生效。'
      });
      el.appendChild(editField.field);

      el.appendChild(errBox);
    }

    // dirty 表示「常规」里有还没保存的改动，切标签/关窗时据此提醒。
    function dirty() {
      if (!cur) return false;
      if (titleField && titleField.read() !== (cur.title || '')) return true;
      if (prefixField && prefixField.read() !== (cur.path_prefix || '')) return true;
      if (listenAddrField && listenAddrField.read() !== (cur.listen_addr || '')) return true;
      if (listenPortField && listenPortField.read() !== String(cur.listen_port)) return true;
      if (cur.root_switchable && rootInput) {
        var nextRoot = rootInput.value.trim();
        if (nextRoot && nextRoot !== (cur.root || '')) return true;
      }
      if (uploadField && uploadField.read() !== cur.upload_max_size) return true;
      if (editField && editField.read() !== cur.edit_max_size) return true;
      return false;
    }

    async function save() {
      // 读取还没回来时字段是空的，这时候保存会把上限写成 0。
      if (!cur) {
        showErr('设置还没读取完，请稍候再保存。');
        return;
      }
      errBox.hidden = true;

      var patch = {};

      var nextTitle = titleField.read();
      // 空标题交给服务端去拒绝（规则在那边），但没必要白跑一趟往返。
      if (!nextTitle) {
        showErr('站点标题不能为空。想用启动参数里的默认值，请点「恢复启动时的默认值」。');
        return;
      }
      if (nextTitle !== (cur.title || '')) patch.title = nextTitle;

      var nextPrefix = prefixField ? prefixField.read() : (cur.path_prefix || '');
      if (nextPrefix !== (cur.path_prefix || '')) {
        if (nextPrefix && nextPrefix.charAt(0) !== '/') {
          showErr('访问路径前缀要以 / 开头，或留空表示服务在根路径。');
          return;
        }
        patch.path_prefix = nextPrefix;
      }

      // 监听地址/端口。校验只做「明显不对」的：真正的规则在服务端
      // （NormalizeListen），前端只负责别把空白与拼错的数字当成有效输入。
      if (listenAddrField) {
        var nextAddr = listenAddrField.read();
        if (nextAddr !== (cur.listen_addr || '')) {
          // 地址里不带端口：端口有单独的框，两个地方都能表达它只会让人犯迷糊。
          if (nextAddr && nextAddr.indexOf('/') !== 0 && nextAddr.indexOf(':') >= 0) {
            showErr('监听地址里不要写端口，端口请填在「监听端口」里。');
            return;
          }
          patch.listen_addr = nextAddr;
        }
      }
      if (listenPortField) {
        var nextPortRaw = listenPortField.read();
        if (nextPortRaw !== String(cur.listen_port)) {
          if (!/^[0-9]+$/.test(nextPortRaw)) {
            showErr('监听端口请填 0-65535 的整数（0 表示由系统分配）。');
            return;
          }
          var nextPort = parseInt(nextPortRaw, 10);
          if (nextPort > 65535) {
            showErr('监听端口请填 0-65535 的整数（0 表示由系统分配）。');
            return;
          }
          patch.listen_port = nextPort;
        }
      }

      if (cur.root_switchable) {
        var nextRoot = rootInput.value.trim();
        if (nextRoot && nextRoot !== (cur.root || '')) patch.root = nextRoot;
      }

      var nextMax = uploadField.read();
      if (isNaN(nextMax)) {
        showErr('上传上限请填写一个非负数字，或勾选「不限制」。');
        return;
      }
      if (nextMax !== cur.upload_max_size) {
        if (nextMax === 0) {
          var ok = await G.confirm({
            title: '确认取消上传上限？',
            message: '把单文件上传上限设为「不限制」后，一个请求就能把磁盘写满。',
            detail: '只有在完全可信的网络里才建议这么做。',
            okLabel: '仍然不限制',
            danger: true
          });
          if (!ok) return;
        }
        patch.upload_max_size = nextMax;
      }

      var nextEdit = editField.read();
      if (isNaN(nextEdit)) {
        showErr('编辑上限请填写一个非负数字，或勾选「不限制」。');
        return;
      }
      if (nextEdit !== cur.edit_max_size) patch.edit_max_size = nextEdit;

      if (!Object.keys(patch).length) {
        ctx.close();
        G.toast('info', '没有需要保存的改动');
        return;
      }

      // 前缀的确认框放在**所有校验之后、发请求之前**。
      //
      // 必须早于请求：服务端一改，本页所在的那个前缀就不再被路由了，
      // 那时再让用户「取消」他也回不到改动前的状态。
      // 也不必更早：前面的校验还可能失败并中止，先让人确认「我要跳转」、
      // 再告诉他「上传上限填错了」，顺序很别扭。
      if (patch.path_prefix !== undefined) {
        var target = patch.path_prefix || '/';
        var goOn = await G.confirm({
          title: '确认修改访问路径前缀？',
          message: '保存后本页会跳到 ' + target + '，服务只会按新地址路由。',
          detail: '挂在反向代理后面时，必须同步把代理的 location 改成 ' + target +
            '，否则新旧地址都打不开，页面上的设置也就进不去了。' +
            '改坏了可以用 --path-prefix 重启一次恢复。',
          okLabel: '改并跳转',
          danger: true
        });
        if (!goOn) return;
      }

      // 监听地址/端口的确认框：同样放在最后、发请求之前。
      if (patch.listen_addr !== undefined || patch.listen_port !== undefined) {
        var where = (patch.listen_addr !== undefined ? patch.listen_addr : cur.listen_addr) || '所有网卡';
        var port = patch.listen_port !== undefined ? patch.listen_port : cur.listen_port;
        var okListen = await G.confirm({
          title: '确认修改监听地址？',
          message: '服务将**立即**改到 ' + where + ':' + port + ' 上监听。',
          detail: '已经在进行的传输不会中断（连接不会被掐断），但**新**连接都会走新地址。' +
            '端口通常还牵着别的东西：Docker 的端口映射、防火墙规则、systemd unit、' +
            '反向代理的 upstream —— 这些没同步改，外面就连不上了。' +
            '改坏了可以用 --bind/--port 重启一次恢复。',
          okLabel: '改并切换',
          danger: true
        });
        if (!okListen) return;
      }

      var saveBtn = document.getElementById('settings-save');
      if (saveBtn) { saveBtn.disabled = true; saveBtn.textContent = '保存中…'; }

      try {
        var res = await G.apiFetch(EP, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(patch)
        });
        if (!res.ok) {
          throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        }
        var out = await res.json();

        // 把新值同步回页面状态：上传上限影响页脚提示，
        // 编辑上限决定列表里给不给「编辑」入口 —— 都要立刻生效，
        // 否则得刷新页面才看得到变化。
        G.data.upload_max_size = out.upload_max_size;
        G.data.edit_max_size = out.edit_max_size;

        // 标题当场生效：标签页与顶栏品牌名都立刻换成新值，
        // 不必等下一次请求（其它已打开的页面仍需刷新才会变）。
        if (out.title) G.data.title = out.title;
        if (G.applySiteTitle) G.applySiteTitle();

        var changedRoot = patch.root !== undefined;
        var mm = out.upload_max_size > 0 ? G.fmtSize(out.upload_max_size) : '不限制';
        ctx.close();

        // 监听地址变了 —— 这一支要放在前缀之前：服务端给的跳转地址里已经
        // 带上了新的访问前缀（前缀分支先执行），所以它是最完整的目标。
        if (out.listen_jump_url) {
          var listenLabel = (out.listen_addr || '所有网卡') + ':' + out.listen_port;
          G.toast('ok', '监听地址已改为 ' + listenLabel + '，正在跳转…');
          // 不等、不轮询 —— 跨域下轮询不出结果，理由见上面 waitForOrigin 那段。
          location.assign(out.listen_jump_url);
          return;
        }
        if (patch.listen_addr !== undefined || patch.listen_port !== undefined) {
          // 服务端没给跳转地址，只有两种可能，都说清楚别让人猜：
          var why = (out.listen_addr || '').charAt(0) === '/'
            ? '新地址是 unix socket，浏览器访问不到。'
            : '当前请求不是直达监听端口（前面多半挡着反向代理），' +
              '请同步把代理的 upstream 指到新地址。';
          G.toast('warn', '监听地址已改为 ' +
            (out.listen_addr || '所有网卡') + ':' + out.listen_port + '。本页不跳转：' + why);

          // 但如果**前缀**同时改了，就得跳到新前缀去：旧前缀已经不再被路由，
          // 留在原地只会满屏 404，而上面的提示只说了监听的事，用户不知道
          // 还得自己改地址。新前缀是同源的，跳转没有跨域问题。
          if (patch.path_prefix !== undefined) {
            var base2 = out.path_prefix || '';
            G.toast('ok', '访问前缀已改为 ' + (base2 || '（根路径）') + '，正在跳转…');
            location.assign(base2 + '/');
          }
          return;
        }

        // 前缀变了：只能整页跳转到新地址。
        //
        // 不能用 G.navigate —— 那是走 XHR 的站内跳转，而此刻 BASE 快照、
        // 前端的接口地址、服务端的路由都已经是新前缀，打在旧前缀上只会 404。
        // 会话 cookie 服务端已经搬到新 Path 上，跳过去不会掉登录态。
        if (patch.path_prefix !== undefined) {
          var base = out.path_prefix || '';
          G.toast('ok', '访问前缀已改为 ' + (base || '（根路径）') + '，正在跳转…');
          location.assign(base + '/');
          return;
        }

        if (changedRoot) {
          G.toast('ok', '服务根目录已切换到 ' + out.root, null, {
            label: '查看',
            onClick: function () { G.navigate('/'); }
          });
          // 原来的路径在新根下多半不存在，直接回到新根。
          G.navigate('/', false);
        } else {
          // 如实说改了哪一项 —— 两项都动过时说一句「上传上限已设为 X」
          // 会让人以为另一项没生效。
          var parts = [];
          if (patch.title !== undefined) parts.push('标题「' + out.title + '」');
          if (patch.upload_max_size !== undefined) parts.push('上传上限 ' + mm);
          if (patch.edit_max_size !== undefined) {
            parts.push('编辑上限 ' + G.fmtSize(out.edit_max_size));
          }
          G.toast('ok', '已保存：' + parts.join('，'));
          if (G.refresh) G.refresh();
        }
      } catch (e) {
        showErr('保存失败：' + e.message);
      } finally {
        if (saveBtn) { saveBtn.disabled = false; saveBtn.textContent = '保存'; }
      }
    }

    fetchSettings().then(function (data) {
      cur = data;
      fill();
    }).catch(function (e) {
      loading.textContent = '读取设置失败：' + e.message;
    });

    return {
      el: el,
      footerItems: [
        { label: '取消', onClick: function () { ctx.close(); } },
        { label: '保存', cls: 'primary', id: 'settings-save', onClick: function () { save(); } }
      ],
      dirty: dirty
    };
  }

  // 密钥 / 用户两页的内容由各自文件提供，这里只做适配：
  // 它们返回 { el, buttons, refresh }，buttons 转成底部按钮描述。
  function buildKeysTab() {
    var p = G.keysPanel();
    return { el: p.el, footerItems: p.buttons || [] };
  }

  function buildUsersTab() {
    var p = G.usersPanel();
    return { el: p.el, footerItems: p.buttons || [] };
  }

  // ---------------------------------------------------------------- 等服务回来

  // healthOK 探一次 /fs/health，返回服务是否已经能响应。
  //
  // 用裸 fetch 而不是 G.apiFetch：这个接口不需要鉴权，而 apiFetch 遇到 401
  // 会把用户弹到登录框 —— 服务刚重启、内存里的会话表被清空时，正需要避开它。
  async function healthOK() {
    try {
      var res = await fetch(EP_HEALTH, { cache: 'no-store' });
      return res.ok;
    } catch (e) {
      return false;   // 连不上：多半还没起来
    }
  }

  // waitUntilBack 每 intervalMs 探一次，最多 tries 次，返回服务是否恢复。
  //
  // 先等再探（而不是先探）：升级/重启刚发出去的那一刻服务往往还活着，
  // 立刻探会立刻「成功」，等于没等 —— 而它下一秒才真的开始换进程。
  async function waitUntilBack(tries, intervalMs) {
    for (var i = 0; i < tries; i++) {
      await new Promise(function (r) { setTimeout(r, intervalMs); });
      if (await healthOK()) return true;
    }
    return false;
  }

  function buildUpgradeTab() {
    var el = document.createElement('div');
    // 三块互相独立：注入了 Upgrader 才有「检查更新」，注入了 Restarter
    // 才有「重启」，那个 Restarter 还实现了 Uninstaller 才有「卸载」。
    // 都没注入的话这个标签压根不会出现。
    if (G.data.allow_upgrade) el.appendChild(buildUpgradeBox());
    if (G.data.allow_restart) el.appendChild(buildRestartBox());
    if (G.data.allow_uninstall) el.appendChild(buildUninstallBox());
    return { el: el, footerItems: [] };
  }

  // ---------------------------------------------------------------- 版本与升级

  // buildUpgradeBox 造出「版本与升级」面板。
  //
  // 三态：初始（有「检查更新」）→ 已是最新 / 有新版本 → 升级中。
  // 「检查」与「升级」都只由管理员触发；服务端还会再校验一次权限。
  function buildUpgradeBox() {
    var box = document.createElement('div');
    box.className = 'field upgrade-box';

    var lb = document.createElement('label');
    lb.textContent = '升级';
    box.appendChild(lb);

    var row = document.createElement('div');
    row.className = 'row';
    row.style.alignItems = 'center';

    var ver = document.createElement('span');
    ver.className = 'upgrade-ver';
    ver.textContent = '当前版本 ' + (G.data.version || '未知');
    row.appendChild(ver);

    var checkBtn = G.btn('检查更新', '', function () { check(); });
    row.appendChild(checkBtn);
    box.appendChild(row);

    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '升级由本部署自行实现的更新源提供；升级完成后需要重启服务生效。';
    box.appendChild(hint);

    var result = document.createElement('div');
    result.className = 'upgrade-result';
    box.appendChild(result);

    function setBusy(on, text) {
      checkBtn.disabled = on;
      checkBtn.textContent = on ? (text || '检查中…') : '检查更新';
    }

    async function check() {
      setBusy(true);
      result.textContent = '正在检查…';
      result.className = 'upgrade-result';
      try {
        var res = await G.apiFetch(EP_UPGRADE);
        var data = await res.json();
        if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
        if (!data.updateable) {
          result.className = 'upgrade-result ok';
          result.textContent = '已是最新版本（' + data.current + '）。';
          return;
        }
        renderRelease(data.release);
      } catch (e) {
        result.className = 'upgrade-result err';
        result.textContent = '检查更新失败：' + e.message;
      } finally {
        setBusy(false);
      }
    }

    function renderRelease(rel) {
      result.className = 'upgrade-result';
      result.textContent = '';

      var head = document.createElement('div');
      head.className = 'upgrade-head';
      head.textContent = '发现新版本 ' + rel.version +
        (rel.published_at ? '（' + rel.published_at + '）' : '');
      result.appendChild(head);

      if (rel.notes) {
        var notes = document.createElement('div');
        notes.className = 'upgrade-notes';
        // 发布说明通常是 Markdown（GitHub 的 release body 就是），
        // 渲染成预览，而不是把 `##`、`-`、`[x](y)` 这些标记原样摊给人看。
        //
        // 安全性由 renderMarkdown 负责（编辑器那套，见 editor.js）：
        // 它先把 HTML 转义、再只认自己支持的那几种子集，链接与图片地址
        // 还要过 safeURL（挡掉 javascript: / data:）。它本来就在渲染
        // 用户文件，用在这里同样成立 —— 所以不必坚持「远端内容只当纯文本」。
        if (G.renderMarkdown) {
          notes.classList.add('ed-markdown');
          notes.innerHTML = G.renderMarkdown(rel.notes);
        } else {
          // editor.js 没加载时的兜底：至少把换行留住。
          notes.classList.add('is-plain');
          notes.textContent = rel.notes;
        }
        result.appendChild(notes);
      }

      var actions = document.createElement('div');
      actions.className = 'row';
      actions.style.marginTop = '8px';

      // 发布页链接来自更新源，属于外部数据。只接受 https，避免自定义
      // Upgrader 返回 javascript:/data: 时变成一个可执行链接。
      var pageURL = String(rel.page_url || '');
      if (/^https:\/\//i.test(pageURL)) {
        var a = document.createElement('a');
        a.className = 'btn btn-sm';
        a.href = pageURL;
        a.target = '_blank';
        a.rel = 'noopener noreferrer';
        a.textContent = '查看发布页';
        actions.appendChild(a);
      }

      var upBtn = G.btn('立即升级', 'primary', function () { upgrade(rel, upBtn); });
      actions.appendChild(upBtn);
      result.appendChild(actions);

      var prog = document.createElement('div');
      prog.className = 'upgrade-progress';
      prog.hidden = true;
      result.appendChild(prog);
      upBtn._prog = prog;
    }

    function upgrade(rel, btn) {
      // 升级会替换正在运行的二进制，属于不可逆动作 —— 先确认。
      G.confirm({
        title: '确认升级到 ' + rel.version + '？',
        message: '服务会下载并替换当前的程序文件。',
        detail: '升级过程中请不要关闭本页面；完成后需要重启服务才会生效。',
        okLabel: '开始升级',
        danger: true
      }).then(function (ok) {
        if (!ok) return;
        runUpgrade(rel, btn);
      });
    }

    function runUpgrade(rel, btn) {
      var prog = btn._prog;
      btn.disabled = true;
      prog.hidden = false;

      // 用流式读取而不是 EventSource：事件源没法带 Authorization 头，
      // 而这里有鉴权。逐块解析 SSE 帧即可。
      G.apiFetch(EP_UPGRADE, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ release: rel })
      }).then(function (res) {
        if (!res.ok) {
          // 把服务端的说明原样带出来。只看状态码的话，管理员拿到的是
          // 「升级失败：HTTP 409」—— 而 409 的含义（已经有一次升级在跑）
          // 恰恰是唯一能解释问题的信息，不能丢。
          return res.text().then(function (body) {
            throw new Error((body || '').trim() || ('HTTP ' + res.status));
          });
        }
        readSSE(res.body, function (name, payload) {
          if (name === 'progress') {
            var pct = payload.total > 0
              ? '（' + Math.round((payload.loaded / payload.total) * 100) + '%）' : '';
            prog.className = 'upgrade-progress';
            prog.textContent = stageText(payload.stage) + ' ' +
              (payload.message || '') + pct;
          } else if (name === 'error') {
            prog.className = 'upgrade-progress err';
            prog.textContent = '升级失败：' + payload.message;
            btn.disabled = false;
          } else if (name === 'done') {
            btn.textContent = '已升级';
            reloadAfterUpgrade(prog, payload.message);
          }
        });
      }).catch(function (e) {
        prog.className = 'upgrade-progress err';
        prog.textContent = '升级失败：' + e.message;
        btn.disabled = false;
      });
    }

    // reloadAfterUpgrade 把「升级完成」显示出来，等服务就绪后**自动刷新一次**页面。
    //
    // 为什么不能收到 done 就直接 location.reload()：
    //   1) 实现方常常在升级末尾把进程换掉（原地重执行，或交给 supervisor 拉起），
    //      那一刻连接是断的。立刻刷只会刷到一个连不上的页面 —— 浏览器弹
    //      「无法访问」，看起来就像升级把服务搞挂了。
    //   2) 刷得太快也看不清结果，用户只看到弹窗凭空消失。
    // 所以先等服务重新能响应（waitUntilBack 已经先等一秒再探，没换进程的实现
    // 也会等够这一秒），再刷。
    //
    // 服务 60 秒都没回来时**不刷**：这时候刷过去就是一个连不上的页面，
    // 停在原地把话说清楚，比刷一个死页面强。
    async function reloadAfterUpgrade(prog, msg) {
      var text = msg || '升级完成';
      prog.className = 'upgrade-progress ok';
      prog.textContent = text + '　正在等服务就绪，随后自动刷新页面…';
      if (!(await waitUntilBack(60, 1000))) {
        prog.className = 'upgrade-progress err';
        prog.textContent = text + '，但服务 60 秒内没有响应，请手动刷新页面确认。';
        return;
      }
      prog.textContent = text + '　服务已就绪，正在刷新页面…';
      setTimeout(function () { location.reload(); }, 600);
    }

    return box;
  }

  // buildRestartBox 造出「重启服务」那一块。
  //
  // 怎么重启由服务端的 Restarter 实现决定（原地重执行 / 通知编排器 /
  // 退出交给 supervisor），前端只负责「发出请求 + 等它回来」。
  //
  // 为什么靠轮询 /fs/health 而不是看响应：实现方如果是原地重执行自己，
  // 进程镜像在写出响应之前就被换掉了，客户端看到的就是连接中断 ——
  // 那是重启的正常表现，不是失败。所以「连不上」不能当错误，只能等。
  function buildRestartBox() {
    var box = document.createElement('div');
    box.className = 'field upgrade-box';

    var lb = document.createElement('label');
    lb.textContent = '重启服务';
    box.appendChild(lb);

    var row = document.createElement('div');
    row.className = 'row';
    row.style.alignItems = 'center';
    var btn = G.btn('重启', 'danger', function () { askRestart(); });
    row.appendChild(btn);
    box.appendChild(row);

    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '重启方式由本部署自行实现；正在进行的上传与下载会中断。';
    box.appendChild(hint);

    var result = document.createElement('div');
    result.className = 'upgrade-progress';
    result.hidden = true;
    box.appendChild(result);

    function say(text, cls) {
      result.hidden = false;
      result.className = 'upgrade-progress' + (cls ? ' ' + cls : '');
      result.textContent = text;
    }

    function askRestart() {
      G.confirm({
        title: '确认重启服务？',
        message: '正在进行的上传与下载会中断。',
        // 会话表在内存里，重启会把它清空 —— 不说清楚的话，
        // 刷新出来的登录框会让人以为「重启把配置搞坏了」。
        detail: '重启期间页面会短暂断开，服务恢复后本页自动刷新。' +
          '重启后需要重新登录（会话是内存态的）。',
        okLabel: '重启',
        danger: true
      }).then(function (ok) { if (ok) run(); });
    }

    async function run() {
      btn.disabled = true;
      say('正在重启…');
      try {
        var res = await G.apiFetch(EP_RESTART, { method: 'POST' });
        if (!res.ok) {
          // 这里能拿到完整响应，说明重启**没有**发生（403/404/500），
          // 那就是真的失败，把服务端的说明原样显示。
          var body = (await res.text()).trim();
          say('重启失败：' + (body || ('HTTP ' + res.status)), 'err');
          btn.disabled = false;
          return;
        }
      } catch (e) {
        // 连接被掐断 —— 进程多半已经没了，这恰恰说明重启生效了。
        // 不当错误处理，直接进等待。
      }
      await waitForBack();
    }

    async function waitForBack() {
      // 最多等 60 秒（见 waitUntilBack）。
      if (await waitUntilBack(60, 1000)) {
        say('服务已重启，正在刷新页面…', 'ok');
        setTimeout(function () { location.reload(); }, 800);
        return;
      }
      say('等了 60 秒服务仍未恢复，请手动刷新页面确认。', 'err');
      btn.disabled = false;
    }

    return box;
  }

  // buildUninstallBox 造出「卸载服务」那一块。
  //
  // 与重启最大的不同在**怎么判断成功**：重启是等它**回来**，卸载是等它**别回来**。
  // 所以那套 60 秒「恢复了吗」的循环在这里要反过来用 —— 等满一小段时间仍
  // 连不上，才算卸载成功；连得上反而说明没卸干净，得如实说出来。
  //
  // 库自己**不删任何东西**：删二进制、注销 systemd 单元、清配置目录都由
  // 服务端的 Uninstaller 实现决定（见 config.Uninstaller）。
  function buildUninstallBox() {
    var box = document.createElement('div');
    box.className = 'field upgrade-box';

    var lb = document.createElement('label');
    lb.textContent = '卸载服务';
    box.appendChild(lb);

    var row = document.createElement('div');
    row.className = 'row';
    row.style.alignItems = 'center';
    var btn = G.btn('卸载', 'danger', function () { askUninstall(); });
    row.appendChild(btn);
    box.appendChild(row);

    var hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = '删掉什么由本部署的实现决定（二进制 / systemd 单元 / ' +
      '配置目录…）；gofs 自身不删任何东西。这一步不可逆。';
    box.appendChild(hint);

    var result = document.createElement('div');
    result.className = 'upgrade-progress';
    result.hidden = true;
    box.appendChild(result);

    function say(text, cls) {
      result.hidden = false;
      result.className = 'upgrade-progress' + (cls ? ' ' + cls : '');
      result.textContent = text;
    }

    function askUninstall() {
      G.confirm({
        title: '确认卸载服务？',
        message: '这一步不可逆，服务会被移除。',
        // 把「不会自动恢复」说在最前面：重启会自己回来，卸载不会 ——
        // 这一点不说清楚，用户会像等重启那样干等下去。
        detail: '卸载后本服务将被移除，页面会断开且**不会**自动恢复。' +
          '具体删掉什么由本部署的实现决定（二进制、systemd 单元、配置目录等）。' +
          '要恢复只能重新部署。',
        okLabel: '卸载',
        danger: true
      }).then(function (ok) { if (ok) run(); });
    }

    async function run() {
      btn.disabled = true;
      say('正在卸载…');
      try {
        var res = await G.apiFetch(EP_UNINSTALL, { method: 'POST' });
        if (!res.ok) {
          // 能拿到完整响应，说明卸载**没有**开始（403/404/409/500），
          // 那就是真的失败，把服务端的说明原样显示。
          var body = (await res.text()).trim();
          say('卸载失败：' + (body || ('HTTP ' + res.status)), 'err');
          btn.disabled = false;
          return;
        }
      } catch (e) {
        // 连接被掐断 —— 实现方多半已经把自己删掉并退出了。这是卸载的
        // 正常表现（响应是尽力而为），继续往下确认。
      }
      await waitForGone();
    }

    // waitForGone 确认服务**没有**回来。
    //
    // 用裸 fetch 而不是 apiFetch：health 不需要鉴权，而 apiFetch 遇到 401
    // 会把用户弹到登录框 —— 服务都没了，弹一个登录框只会让人更糊涂。
    async function waitForGone() {
      // 15 秒：只是个观察窗口，不是「卸载要多久」。实现方若是异步收尾
      // （比如交给 systemd 停服务），这里可能还是连得上 —— 那种情况
      // 下面的分支会如实说「仍在运行」，让人去服务端确认，而不是假报成功。
      for (var i = 0; i < 15; i++) {
        await new Promise(function (r) { setTimeout(r, 1000); });
        try {
          var res = await fetch(EP_HEALTH, { cache: 'no-store' });
          if (res.ok) {
            say('卸载请求已受理，但服务仍在运行（可能只做了一半或还在收尾），' +
              '请到服务端确认。', 'err');
            btn.disabled = false;
            return;
          }
        } catch (e) { /* 连不上 —— 正是我们要的结果，继续数满窗口 */ }
      }
      // 连不上且窗口走完：服务确实停了。
      // 不 reload：页面已经是死的，刷新只会得到一个连接错误。
      say('服务已停止，卸载完成。本页可以关闭。', 'ok');
      btn.disabled = true;
    }

    return box;
  }

  // joinCapabilities 按可用能力拼标签名，如「升级与重启」「升级、重启与卸载」。
  //
  // 参数成对：先真假、再名字。三个能力全上时直接写死字符串会变成一堆
  // 三目嵌套，加第四个就彻底读不动了。
  function joinCapabilities() {
    var names = [];
    for (var i = 0; i < arguments.length; i += 2) {
      if (arguments[i]) names.push(arguments[i + 1]);
    }
    if (names.length <= 1) return names[0] || '';
    // 「与」只连最后一项，前面用顿号 —— 与界面里原有的「升级与重启」一致。
    return names.slice(0, -1).join('、') + '与' + names[names.length - 1];
  }

  function stageText(stage) {
    return {
      checking: '准备中…',
      downloading: '下载中…',
      verifying: '校验中…',
      replacing: '替换中…',
      done: '完成'
    }[stage] || stage;
  }

  // readSSE 逐块解析 SSE 帧并回调。
  //
  // 自己写而不是用 EventSource，是因为 EventSource 不能自定义请求头 ——
  // 而这条接口要带 Authorization。
  async function readSSE(stream, onEvent) {
    if (!stream) return;
    var reader = stream.getReader();
    var dec = new TextDecoder();
    var buf = '';
    for (;;) {
      var step = await reader.read();
      if (step.done) break;
      buf += dec.decode(step.value, { stream: true });
      var i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        var frame = buf.slice(0, i);
        buf = buf.slice(i + 2);
        var name = '', data = '';
        frame.split('\n').forEach(function (line) {
          if (line.indexOf('event: ') === 0) name = line.slice(7);
          else if (line.indexOf('data: ') === 0) data += line.slice(6);
        });
        if (!name) continue;
        var payload = {};
        try { payload = JSON.parse(data); } catch (e) { /* 忽略坏帧 */ }
        onEvent(name, payload);
      }
    }
  }

  // ---------------------------------------------------------------- 弹窗

  // openSettings 打开设置弹窗。
  //
  // 标签按权限/是否启用条件显示，第一个可用的标签默认激活；每页只在
  // 第一次切到时才构建（惰性），切回来则复用 —— 复用是必须的，否则
  // 来回切会把「常规」里没保存的输入丢掉。
  async function openSettings() {
    var defs = [];
    if (G.data.allow_settings) {
      defs.push({ id: 'general', label: '常规', build: buildGeneralTab });
    }
    if (G.data.allow_keys) {
      defs.push({ id: 'keys', label: '密钥', build: buildKeysTab });
    }
    if (G.data.allow_users) {
      defs.push({ id: 'users', label: '用户与权限', build: buildUsersTab });
    }
    // 升级 / 重启 / 卸载接口都要管理员权限，所以没「常规」就没有这一页 ——
    // 否则会给出一个点了必然 403 的标签。
    // 只要有一个可用就显示：只重启不升级（或只卸载不重启）的部署很常见，
    // 页内再各自判断显示哪一块。
    var canUp = !!G.data.allow_upgrade;
    var canRestart = !!G.data.allow_restart;
    var canUninstall = !!G.data.allow_uninstall;
    if (G.data.allow_settings && (canUp || canRestart || canUninstall)) {
      // 标签名跟着实际有的能力走 —— 只注入了 Upgrader 却写着「重启」，
      // 点进去找不到那个按钮，比不写更让人困惑。
      defs.push({ id: 'upgrade', label: joinCapabilities(
        canUp, '升级', canRestart, '重启', canUninstall, '卸载'),
        build: buildUpgradeTab });
    }
    if (!defs.length) return;

    var body = document.createElement('div');
    body.className = 'settings-panel';

    var tabBar = document.createElement('div');
    tabBar.className = 'tabs';
    tabBar.hidden = defs.length < 2; // 只有一个面板就不摆标签条
    body.appendChild(tabBar);

    var panes = document.createElement('div');
    panes.className = 'settings-panes';
    body.appendChild(panes);

    var m = null;
    var allowClose = false;   // 主动关闭（点按钮）时跳过「未保存」拦截
    var activeDef = null;

    function closeSelf() {
      allowClose = true;
      if (m) m.close();
    }
    var ctx = { close: closeSelf };

    // 先把默认页建好，这样底部按钮一上来就是对的，不会闪一下空条。
    // mount 建出来的页默认是 hidden 的（留给切换逻辑），默认页要当场放开，
    // 否则弹窗一打开就是「有标签条、下面空白」。
    var first = defs[0];
    mount(first);
    first.node.hidden = false;
    activeDef = first;

    defs.forEach(function (def) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'tab' + (def === first ? ' active' : '');
      b.textContent = def.label;
      b.onclick = function () { activate(def); };
      tabBar.appendChild(b);
      def.btn = b;
    });

    m = G.openModal({
      // 只有一个面板时不摆标签条，标题直接用它自己的名字，省得
      // 一个只管密钥的账号对着「设置」猜里面是什么。
      title: defs.length === 1 ? defs[0].label : '设置',
      body: body,
      full: true,
      cls: 'settings-modal',
      beforeClose: guardClose
    });

    // 自己建底部容器，并直接把按钮塞进去：openModal 会把传进去的
    // footer 节点再包一层 .modal-foot，那样会嵌套两层。
    var footEl = document.createElement('div');
    footEl.className = 'modal-foot';
    m.el.appendChild(footEl);
    setFooter(first);

    // mount 首次构建某个标签并按需挂载（已建好的直接复用）。
    function mount(def) {
      if (def.tab) return;
      def.node = document.createElement('div');
      def.node.className = 'settings-pane';
      def.node.hidden = true;
      panes.appendChild(def.node);
      def.tab = def.build(ctx);
      def.node.appendChild(def.tab.el);
    }

    async function activate(def) {
      if (def === activeDef) return;
      // 离开「常规」前先确认，别让未保存的改动悄悄没了。
      if (!(await confirmLeaveDirty())) return;
      mount(def);
      activeDef.node.hidden = true;
      activeDef.btn.classList.remove('active');
      activeDef = def;
      def.node.hidden = false;
      def.btn.classList.add('active');
      setFooter(def);
      panes.scrollTop = 0;
    }

    // footItemsFor 给出某页的底部按钮：常规页自带「取消/保存」，
    // 其余页面都是「关闭」加上该页自己的动作（用户页的「新建用户」）。
    function footItemsFor(def) {
      if (def.id === 'general') return def.tab.footerItems;
      return [{ label: '关闭', onClick: function () { closeSelf(); } }]
        .concat(def.tab.footerItems || []);
    }

    function setFooter(def) {
      footEl.textContent = '';
      var src = G.modalFoot(footItemsFor(def));
      while (src.firstChild) footEl.appendChild(src.firstChild);
    }

    function hasDirtyGeneral() {
      var d = activeDef;
      return !!(d && d.id === 'general' && d.tab && d.tab.dirty && d.tab.dirty());
    }

    function confirmLeaveDirty() {
      if (!hasDirtyGeneral()) return Promise.resolve(true);
      return G.confirm({
        title: '放弃未保存的改动？',
        message: '「常规」里有还没保存的设置，切走或关闭都会丢掉。',
        okLabel: '放弃',
        danger: true
      });
    }

    // guardClose 只在「常规」有未保存改动时拦一下 —— 点底部按钮属于
    // 明确意图，走 closeSelf 不经过这里，不会自找麻烦地再问一次。
    function guardClose() {
      if (allowClose) return true;
      // 上面还压着嵌套弹窗（密钥明文 / 新建用户）时不介入：
      // 每个弹窗都注册了自己的 ESC 处理，这里再弹确认会张冠李戴。
      if (document.querySelectorAll('#modal-root .modal-mask').length > 1) return true;
      if (!hasDirtyGeneral()) return true;
      G.confirm({
        title: '放弃未保存的改动？',
        message: '「常规」里有还没保存的设置。',
        okLabel: '放弃',
        danger: true
      }).then(function (ok) { if (ok) closeSelf(); });
      return false;
    }
  }

  G.openSettings = openSettings;
})();
