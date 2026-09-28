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
    { label: 'MiB', factor: 1024 * 1024 },
    { label: 'GiB', factor: 1024 * 1024 * 1024 },
    { label: 'TiB', factor: 1024 * 1024 * 1024 * 1024 }
  ];

  // pickUnit 选一个让数值落在 1~1024 之间的单位，避免显示成
  // 「10737418240 B」这种要数零的写法。
  function pickUnit(bytes) {
    var best = UNITS[1];
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

    var maxField = makeField('单文件上传上限');
    var maxRow = document.createElement('div');
    maxRow.className = 'row';

    var initial = splitSize(cur.upload_max_size);
    var maxInput = textInput(cur.upload_max_size ? initial.value : '');
    maxInput.style.flex = '1';
    maxInput.style.minWidth = '0';
    maxInput.placeholder = '例如 10';
    maxRow.appendChild(maxInput);

    var unitSel = document.createElement('select');
    unitSel.className = 'select';
    UNITS.forEach(function (u) {
      var o = document.createElement('option');
      o.value = String(u.factor);
      o.textContent = u.label;
      if (u.label === initial.unit.label) o.selected = true;
      unitSel.appendChild(o);
    });
    maxRow.appendChild(unitSel);

    var unlimited = document.createElement('label');
    unlimited.className = 'checkbox';
    var unlimitedCb = document.createElement('input');
    unlimitedCb.type = 'checkbox';
    unlimitedCb.checked = !cur.upload_max_size;
    var unlimitedText = document.createElement('span');
    unlimitedText.textContent = '不限制';
    unlimited.appendChild(unlimitedCb);
    unlimited.appendChild(unlimitedText);
    maxRow.appendChild(unlimited);

    maxField.appendChild(maxRow);
    var maxHint = addHint(maxField, '');
    body.appendChild(maxField);

    function refreshMaxHint() {
      maxHint.textContent = '';
      maxHint.appendChild(document.createTextNode(
        unlimitedCb.checked
          ? '不限制单文件大小 —— 只要磁盘放得下就能传。'
          : '超过该大小的上传会被拒绝（HTTP 413），且不会留下残缺文件。'));
      maxHint.appendChild(document.createTextNode('　当前生效：'));
      var b = document.createElement('b');
      b.textContent = cur.upload_max_size > 0 ? G.fmtSize(cur.upload_max_size) : '不限制';
      maxHint.appendChild(b);
      maxHint.appendChild(document.createTextNode(
        '　·　启动参数默认值：' + (cur.upload_max_size_default > 0
          ? G.fmtSize(cur.upload_max_size_default) : '不限制')));

      var quick = document.createElement('button');
      quick.type = 'button';
      quick.className = 'btn btn-sm';
      quick.style.marginTop = '8px';
      quick.textContent = '恢复启动时的默认值';
      quick.onclick = function () {
        var d = splitSize(cur.upload_max_size_default);
        unlimitedCb.checked = cur.upload_max_size_default === 0;
        maxInput.value = d.value;
        // 让下拉选到对应单位
        for (var i = 0; i < unitSel.options.length; i++) {
          if (unitSel.options[i].value === String(d.unit.factor)) unitSel.selectedIndex = i;
        }
        syncInputs();
        refreshMaxHint();
      };
      maxHint.appendChild(document.createElement('br'));
      maxHint.appendChild(quick);
    }

    function syncInputs() {
      maxInput.disabled = unlimitedCb.checked;
      unitSel.disabled = unlimitedCb.checked;
    }

    function readMax() {
      if (unlimitedCb.checked) return 0;
      var v = parseFloat(String(maxInput.value).replace(/,/g, '').trim());
      if (!isFinite(v) || v < 0) return NaN;
      var factor = parseInt(unitSel.value, 10) || 1;
      return Math.round(v * factor);
    }

    unlimitedCb.onchange = function () { syncInputs(); refreshMaxHint(); };
    maxInput.oninput = refreshMaxHint;
    unitSel.onchange = refreshMaxHint;

    syncInputs();
    refreshMaxHint();

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

      var nextMax = readMax();
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

        // 把新值同步回页面状态，页脚 / 上传按钮提示才会跟着更新。
        G.data.upload_max_size = out.upload_max_size;

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
          G.toast('ok', '上传上限已设为 ' + mm);
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
