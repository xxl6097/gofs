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
      hint.appendChild(document.createTextNode(
        (allowUnlimited && cb.checked) ? opts.unlimitedHint : opts.limitedHint));
      hint.appendChild(document.createTextNode('　当前生效：'));
      var b = document.createElement('b');
      b.textContent = fmt(opts.current());
      hint.appendChild(b);
      hint.appendChild(document.createTextNode(
        '　·　启动参数默认值：' + fmt(opts.def)));

      var quick = document.createElement('button');
      quick.type = 'button';
      quick.className = 'btn btn-sm';
      quick.style.marginTop = '8px';
      quick.textContent = '恢复启动时的默认值';
      quick.onclick = function () {
        var d = splitSize(opts.def);
        if (allowUnlimited) cb.checked = opts.def === 0;
        input.value = d.value;
        for (var i = 0; i < unitSel.options.length; i++) {
          if (unitSel.options[i].value === String(d.unit.factor)) unitSel.selectedIndex = i;
        }
        syncInputs();
        refresh();
      };
      hint.appendChild(document.createElement('br'));
      hint.appendChild(quick);
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
      hint.appendChild(document.createTextNode(opts.hintText));
      hint.appendChild(document.createTextNode('　当前生效：'));
      var b = document.createElement('b');
      b.textContent = opts.current() || '（默认）';
      hint.appendChild(b);
      hint.appendChild(document.createTextNode('　·　启动参数默认值：' + (opts.def || '（空）')));

      var quick = document.createElement('button');
      quick.type = 'button';
      quick.className = 'btn btn-sm';
      quick.style.marginTop = '8px';
      quick.textContent = '恢复启动时的默认值';
      quick.onclick = function () {
        input.value = opts.def || '';
        refresh();
      };
      hint.appendChild(document.createElement('br'));
      hint.appendChild(quick);
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
    var lb = document.createElement('label');
    lb.textContent = labelText;
    wrap.appendChild(lb);
    return wrap;
  }

  function addHint(wrap, text) {
    var d = document.createElement('div');
    d.className = 'hint';
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
      // 可挂范围：没配白名单就是不限制（门槛是管理员），配了才显示出来。
      var roots = data.site_roots || [];
      siteHint.textContent = editable
        ? '新增的目录挂在 /<标签>/ 下，**立即生效**（不必点保存）。' +
          (roots.length ? '可挂范围：' + roots.join('、') : '可挂任意已存在的目录') +
          '　·　注意：标签会遮蔽主目录下的同名子目录。'
        : '本服务已通过 --no-root-switch 禁止运行期改动服务目录。';

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
        hintText: '显示在浏览器标签页与顶栏品牌名上。保存后本页立即生效；' +
          '其它已打开的页面要刷新才看得到。'
      });
      el.appendChild(titleField.field);

      // ---- 服务根目录（主目录）----
      //
      // 主目录永远服务在 `/`；附加站点挂在 /<标签>/ 下，是**运行期**可增删的，
      // 而且增删不动主目录的地址（否则加个站点就让所有书签失效）。
      //
      // 这一栏的文字与禁用状态随「有没有附加站点」变（有站点时不能换主根），
      // 所以抽成 syncRootField，增删站点后重画一次。
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
      // ---- 单文件上传上限 ----
      uploadField = makeSizeField({
        label: '单文件上传上限',
        value: cur.upload_max_size,
        def: cur.upload_max_size_default,
        current: function () { return cur.upload_max_size; },
        placeholder: '例如 10',
        unlimitedHint: '不限制单文件大小 —— 只要磁盘放得下就能传。',
        limitedHint: '超过该大小的上传会被拒绝（HTTP 413），且不会留下残缺文件。'
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
        limitedHint: '超过该大小的文件不显示「编辑」入口，服务端也会拒绝读写。'
      });
      el.appendChild(editField.field);

      el.appendChild(errBox);
    }

    // dirty 表示「常规」里有还没保存的改动，切标签/关窗时据此提醒。
    function dirty() {
      if (!cur) return false;
      if (titleField && titleField.read() !== (cur.title || '')) return true;
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

  function buildUpgradeTab() {
    var el = document.createElement('div');
    // 两块互相独立：注入了 Upgrader 才有「检查更新」，注入了 Restarter
    // 才有「重启」。两个都没注入的话这个标签压根不会出现。
    if (G.data.allow_upgrade) el.appendChild(buildUpgradeBox());
    if (G.data.allow_restart) el.appendChild(buildRestartBox());
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

      if (rel.page_url) {
        var a = document.createElement('a');
        a.className = 'btn btn-sm';
        a.href = rel.page_url;
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
            prog.className = 'upgrade-progress ok';
            prog.textContent = payload.message || '升级完成';
            btn.textContent = '已升级';
          }
        });
      }).catch(function (e) {
        prog.className = 'upgrade-progress err';
        prog.textContent = '升级失败：' + e.message;
        btn.disabled = false;
      });
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
      // 最多等 60 秒。用裸 fetch 而不是 apiFetch：这个接口不需要鉴权，
      // 而 apiFetch 遇到 401 会把用户弹到登录框 —— 重启期间正需要避开它。
      for (var i = 0; i < 60; i++) {
        await new Promise(function (r) { setTimeout(r, 1000); });
        try {
          var res = await fetch(EP_HEALTH, { cache: 'no-store' });
          if (res.ok) {
            say('服务已重启，正在刷新页面…', 'ok');
            setTimeout(function () { location.reload(); }, 800);
            return;
          }
        } catch (e) { /* 还没起来，继续等 */ }
      }
      say('等了 60 秒服务仍未恢复，请手动刷新页面确认。', 'err');
      btn.disabled = false;
    }

    return box;
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
    // 升级与重启接口都要管理员权限，所以没「常规」就没有这一页 ——
    // 否则会给出一个点了必然 403 的标签。
    // 两者只要注入了一个就显示：只重启不升级（或反之）的部署很常见，
    // 页内再各自判断显示哪一块。
    var canUp = !!G.data.allow_upgrade, canRestart = !!G.data.allow_restart;
    if (G.data.allow_settings && (canUp || canRestart)) {
      // 标签名跟着实际有的能力走 —— 只注入了 Upgrader 却写着「重启」，
      // 点进去找不到那个按钮，比不写更让人困惑。
      var label = canUp && canRestart ? '升级与重启' : (canUp ? '升级' : '重启');
      defs.push({ id: 'upgrade', label: label, build: buildUpgradeTab });
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
