/* gofs 服务设置面板：切换服务根目录、调整单文件上传上限。
 *
 * 这两项原本只能由启动参数决定，改一次就得重启进程。放到页面上之后，
 * 换个数据目录、临时放宽上传上限都不需要中断服务。
 *
 * 权限：只有对服务根拥有读写权限的账号能看到入口（服务端 requireAdmin
 * 也会再校验一次，界面隐藏只是不让无关的人误点）。 */
(function () {
  'use strict';

  var G = window.GOFS = window.GOFS || {};

  var BASE = (G.data && G.data.path_prefix) || '';
  var EP = BASE + '/__gofs__/settings';

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

  // openSettings 打开设置面板。每次打开都重新拉一次当前值，
  // 避免面板里显示的还是上一次的旧数据。
  async function openSettings() {
    var body = document.createElement('div');
    body.className = 'settings-panel';
    var loading = document.createElement('div');
    loading.className = 'hint-box';
    loading.textContent = '正在读取当前设置…';
    body.appendChild(loading);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;

    var m = G.openModal({
      title: '服务设置',
      body: body,
      wide: true,
      footer: G.modalFoot([
        { label: '取消', onClick: function () { m.close(); } },
        {
          label: '保存',
          cls: 'primary',
          id: 'settings-save',
          onClick: function () { save(); }
        }
      ])
    });

    var cur = null;

    try {
      var res = await G.apiFetch(EP, { headers: { 'Accept': 'application/json' } });
      if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
      cur = await res.json();
    } catch (e) {
      loading.textContent = '读取设置失败：' + e.message;
      return;
    }

    body.textContent = '';

    // ---- 服务根目录 ----

    var rootField = makeField('服务根目录');
    var rootRow = document.createElement('div');
    rootRow.className = 'row';
    var rootInput = textInput(cur.root || '');
    rootInput.placeholder = cur.root || '/srv/data';
    if (!cur.root_switchable) rootInput.disabled = true;
    rootRow.appendChild(rootInput);
    rootField.appendChild(rootRow);
    addHint(rootField, cur.root_switchable
      ? '当前根目录：' + (cur.root || '（未知）') +
        '　·　可切换的范围：' + (cur.allow_roots || []).join('、') +
        '　·　填相对路径表示「从当前根再往下走」'
      : '本服务已通过 --no-root-switch 禁止运行期切换根目录');
    body.appendChild(rootField);

    // ---- 单文件上传上限 ----

    // ---- 单文件上传上限 ----
    var uploadField = makeSizeField({
      label: '单文件上传上限',
      value: cur.upload_max_size,
      def: cur.upload_max_size_default,
      current: function () { return cur.upload_max_size; },
      placeholder: '例如 10',
      unlimitedHint: '不限制单文件大小 —— 只要磁盘放得下就能传。',
      limitedHint: '超过该大小的上传会被拒绝（HTTP 413），且不会留下残缺文件。'
    });
    body.appendChild(uploadField.field);

    // ---- 在线编辑大小上限 ----
    //
    // 超过这个大小的文件不提供「编辑」入口，服务端也会拒绝读写 ——
    // 编辑器要把整个文件读进内存，放开等于给一个几百 MB 的文件留个口子。
    var editField = makeSizeField({
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
    body.appendChild(editField.field);

    body.appendChild(errBox);

    // ---- 保存 ----

    function showErr(msg) {
      errBox.hidden = false;
      errBox.textContent = msg;
    }

    async function save() {
      errBox.hidden = true;

      var patch = {};

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
        m.close();
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

        var changedRoot = patch.root !== undefined;
        var mm = out.upload_max_size > 0 ? G.fmtSize(out.upload_max_size) : '不限制';
        m.close();

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
  }

  G.openSettings = openSettings;
})();
