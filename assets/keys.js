/* gofs 上传密钥管理。
 *
 * 上传密钥是给脚本 / 第三方用的最小权限凭证：只能上传、可限定目标目录、
 * 可设有效期、可随时撤销。明文只在创建时显示一次，服务端只保存摘要。
 *
 * 复用 app.js 暴露的 window.GOFS。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var KEYS_EP = G.raw('/fs/keys');

  var TTL_OPTIONS = [
    { label: '1 小时', seconds: 3600 },
    { label: '6 小时', seconds: 21600 },
    { label: '1 天', seconds: 86400 },
    { label: '7 天', seconds: 604800 },
    { label: '30 天', seconds: 2592000 },
    { label: '永不过期', seconds: 0 }
  ];

  // ---------------------------------------------------------------- 工具

  function fmtRemaining(sec) {
    if (sec <= 0) return '已过期';
    var d = Math.floor(sec / 86400);
    var h = Math.floor((sec % 86400) / 3600);
    var m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + ' 天后过期';
    if (h > 0) return h + ' 小时后过期';
    if (m > 0) return m + ' 分钟后过期';
    return '不到 1 分钟后过期';
  }

  // kindLabel 把后端的凭证类型翻成列表里显示的人话。
  // 认不出来的类型按上传密钥显示 —— 与后端「缺省即上传」的归一化一致。
  function kindLabel(kind) {
    if (kind === 'readwrite') return '分享链接（可读写）';
    if (kind === 'read') return '分享链接（只读）';
    return '上传密钥';
  }

  // copyText 复用 app.js 导出的实现，避免同一段逻辑维护两份。
  var copyText = G.copyText;

  function serviceBase() {
    return location.origin + (G.data.path_prefix || '');
  }

  // ---------------------------------------------------------------- 主界面

  // keysPanel 构建「上传密钥」面板，供设置弹窗挂到标签页里。
  //
  // 它只负责内容：弹窗外壳（标题、底部按钮、尺寸）由调用方决定，
  // 所以这里不调 openModal。返回的 buttons 是底部按钮描述，
  // 由调用方用 G.modalFoot 组装 —— 直接返回 .modal-foot 节点会与外层嵌套。
  //
  // 返回 { el, buttons, refresh }。创建成功后的一次性明文弹窗
  // （showToken）仍是独立弹窗，叠加在设置弹窗之上。
  function keysPanel() {
    var body = document.createElement('div');

    // 顶部说明
    var intro = document.createElement('div');
    intro.className = 'hint-box';
    intro.style.marginBottom = '14px';
    intro.textContent = '给脚本或第三方的最小上传凭证：只能上传，' +
      '可限定目录与有效期，随时撤销。';
    body.appendChild(intro);

    // 创建区
    var createBox = document.createElement('div');
    createBox.className = 'key-create';
    body.appendChild(createBox);

    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.placeholder = '用途，如：备份脚本';
    nameInput.className = 'key-input';

    var scopeInput = document.createElement('input');
    scopeInput.type = 'text';
    scopeInput.className = 'key-input';
    scopeInput.value = G.state.path === '/' ? '/' : G.state.path;
    scopeInput.placeholder = '/uploads';

    var ttlSelect = document.createElement('select');
    ttlSelect.className = 'key-select';
    TTL_OPTIONS.forEach(function (o, i) {
      var opt = document.createElement('option');
      opt.value = String(o.seconds);
      opt.textContent = o.label;
      ttlSelect.appendChild(opt);
    });
    ttlSelect.value = '604800'; // 默认 7 天

    function row(labelText, control, hint) {
      var w = document.createElement('div');
      w.className = 'key-field';
      var lb = document.createElement('label');
      lb.textContent = labelText;
      w.appendChild(lb);
      w.appendChild(control);
      if (hint) {
        var h = document.createElement('div');
        h.className = 'key-hint';
        h.textContent = hint;
        w.appendChild(h);
      }
      return w;
    }

    createBox.appendChild(row('用途名称', nameInput));
    createBox.appendChild(row('允许上传到', scopeInput,
      '限定写入目录及子目录；留 / 不限。'));
    createBox.appendChild(row('有效期', ttlSelect));

    var createBtn = G.btn('生成密钥', 'primary', function () { create(); });
    var createRow = document.createElement('div');
    createRow.style.marginTop = '4px';
    createRow.appendChild(createBtn);
    createBox.appendChild(createRow);

    // 列表区：标题与批量操作放在同一行
    var listHead = document.createElement('div');
    listHead.className = 'key-list-head';

    var listTitle = document.createElement('div');
    listTitle.className = 'key-list-title';
    listHead.appendChild(listTitle);

    var listTools = document.createElement('div');
    listTools.className = 'key-bulk';

    var selectAllLabel = document.createElement('label');
    selectAllLabel.className = 'key-select-all';
    var selectAll = document.createElement('input');
    selectAll.type = 'checkbox';
    selectAll.disabled = true;
    var selectAllText = document.createElement('span');
    selectAllText.textContent = '全选';
    selectAllLabel.appendChild(selectAll);
    selectAllLabel.appendChild(selectAllText);
    listTools.appendChild(selectAllLabel);

    var bulkBtn = G.btn('撤销所选', 'danger', function () { revokeSelected(); });
    bulkBtn.classList.add('btn-sm');
    bulkBtn.disabled = true;
    listTools.appendChild(bulkBtn);
    listHead.appendChild(listTools);
    body.appendChild(listHead);

    var listBox = document.createElement('div');
    listBox.className = 'key-list';
    body.appendChild(listBox);

    var currentKeys = [];
    var selected = {};

    function selectedCount() {
      var n = 0;
      for (var id in selected) {
        if (Object.prototype.hasOwnProperty.call(selected, id)) n++;
      }
      return n;
    }

    function updateBulk() {
      var n = selectedCount();
      bulkBtn.textContent = n ? '撤销所选（' + n + '）' : '撤销所选';
      bulkBtn.disabled = n === 0;
      selectAll.disabled = currentKeys.length === 0;
      selectAll.checked = currentKeys.length > 0 && n === currentKeys.length;
      selectAll.indeterminate = n > 0 && n < currentKeys.length;
    }

    function setAllChecked(checked) {
      selected = {};
      if (checked) {
        currentKeys.forEach(function (k) { selected[k.id] = k; });
      }
      var boxes = listBox.querySelectorAll('.key-check input');
      for (var i = 0; i < boxes.length; i++) {
        boxes[i].checked = checked;
        var item = boxes[i].closest ? boxes[i].closest('.key-item') : null;
        if (item) item.classList.toggle('selected', checked);
      }
      updateBulk();
    }

    selectAll.onchange = function () { setAllChecked(selectAll.checked); };

    function revokeSelected() {
      var items = currentKeys.filter(function (k) { return selected[k.id]; });
      if (!items.length) return;
      var names = items.slice(0, 3).map(function (k) { return '「' + k.name + '」'; }).join('、');
      if (items.length > 3) names += ' 等 ' + items.length + ' 个';
      G.confirm({
        title: '撤销所选密钥',
        message: '确定要撤销选中的 ' + items.length + ' 个密钥吗？',
        detail: names + ' 会立即失效，相关脚本将收到 401，无法恢复。',
        okLabel: '撤销',
        danger: true
      }).then(function (yes) {
        if (yes) bulkRevoke(items);
      });
    }

    async function bulkRevoke(items) {
      bulkBtn.disabled = true;
      bulkBtn.textContent = '撤销中…';
      var ok = 0;
      var failed = [];
      for (var i = 0; i < items.length; i++) {
        var k = items[i];
        bulkBtn.textContent = '撤销中…（' + (i + 1) + '/' + items.length + '）';
        try {
          var res = await G.apiFetch(KEYS_EP + '?id=' + encodeURIComponent(k.id),
            { method: 'DELETE' });
          if (res.status !== 204 && !res.ok) {
            throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
          }
          ok++;
        } catch (e) {
          failed.push('「' + k.name + '」：' + e.message);
        }
      }
      selected = {};
      await load();
      if (failed.length) {
        G.toast('err', '已撤销 ' + ok + ' 个，' + failed.length +
          ' 个失败。' + failed[0], 6000);
      } else {
        G.toast('ok', '已撤销 ' + ok + ' 个密钥');
      }
    }

    // ---- 创建 ----

    async function create() {
      createBtn.disabled = true;
      createBtn.textContent = '生成中…';
      try {
        var res = await G.apiFetch(KEYS_EP, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: nameInput.value.trim(),
            scope: scopeInput.value.trim() || '/',
            ttl_seconds: parseInt(ttlSelect.value, 10) || 0
          })
        });
        if (!res.ok) {
          throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        }
        var data = await res.json();
        nameInput.value = '';
        await load();
        showToken(data);
      } catch (e) {
        G.toast('err', '生成失败：' + e.message);
      } finally {
        createBtn.disabled = false;
        createBtn.textContent = '生成密钥';
      }
    }

    // ---- 展示一次性明文 ----

    function showToken(data) {
      var token = data.token;
      var box = document.createElement('div');

      var warn = document.createElement('div');
      warn.className = 'key-warn';
      warn.textContent = '⚠ 明文只显示一次，关闭后无法查看。';
      box.appendChild(warn);

      var tokWrap = document.createElement('div');
      tokWrap.className = 'key-token';
      var code = document.createElement('code');
      code.textContent = token;
      tokWrap.appendChild(code);
      var copyBtn = G.btn('复制', 'primary', function () {
        copyText(token).then(function () {
          copyBtn.textContent = '已复制 ✓';
          setTimeout(function () { copyBtn.textContent = '复制'; }, 2000);
        }).catch(function (e) {
          G.toast('err', '复制失败：' + e.message);
        });
      });
      tokWrap.appendChild(copyBtn);
      box.appendChild(tokWrap);

      var meta = document.createElement('div');
      meta.className = 'hint-box';
      meta.style.marginTop = '12px';
      meta.textContent = '名称：' + data.key.name +
        '\n允许目录：' + data.key.scope +
        '\n' + (data.key.permanent ? '有效期：永不过期' : '有效期至：' + data.key.expires_at);
      box.appendChild(meta);

      var usage = document.createElement('div');
      usage.className = 'key-usage';
      var ut = document.createElement('div');
      ut.className = 'key-usage-title';
      ut.textContent = '用法示例';
      usage.appendChild(ut);

      var base = serviceBase();
      var target = (data.key.scope === '/' ? '/' : data.key.scope + '/') + '文件名.txt';
      // 一键上传：服务端把密钥和地址都填好，直接下发一段可执行脚本。
      // 用法就是下面这一行，不用存文件、不用改任何地方。
      var scriptURL = base + '/up?key=' + encodeURIComponent(token);
      var samples = [
        {
          label: '一键脚本上传（推荐）',
          // 复制的只有命令本身；文件/目录参数另行展示，粘进终端后自己补。
          cmd: 'bash <(curl -sS "' + scriptURL + '")',
          example: 'bash <(curl -sS "' + scriptURL + '") 文件或目录... [目标子目录]',
          note: '目录保留层级；省略目标目录则传到 ' + data.key.scope
        },
        {
          label: '请求头',
          cmd: "curl -T local.txt -H 'X-Gofs-Upload-Key: " + token + "' \"" + base + target + "\""
        },
        {
          label: '查询参数',
          cmd: 'curl -T local.txt "' + base + target + '?key=' + token + '"'
        },
        {
          label: '保存脚本备用',
          cmd: 'curl -sSLo gofs-up.sh "' + scriptURL + '" && chmod +x gofs-up.sh'
        }
      ];
      samples.forEach(function (s) {
        // 标签与「复制」放在同一行：命令普遍很长，复制按钮贴着标签更显眼，
        // 也不用在 pre 上浮一层（长命令折行时浮层会跟着错位）。
        var head = document.createElement('div');
        head.className = 'key-usage-head';
        var lb = document.createElement('div');
        lb.className = 'key-usage-label';
        lb.textContent = s.label;
        head.appendChild(lb);

        var copy = G.btn('复制', 'btn-sm', function () {
          copyText(s.cmd).then(function () {
            copy.textContent = '已复制 ✓';
            copy.disabled = true;
            setTimeout(function () {
              copy.textContent = '复制';
              copy.disabled = false;
            }, 2000);
          }).catch(function (e) {
            G.toast('err', '复制失败：' + e.message);
          });
        });
        copy.title = '复制这段命令';
        copy.setAttribute('aria-label', '复制「' + s.label + '」命令');
        head.appendChild(copy);
        usage.appendChild(head);

        var pre = document.createElement('pre');
        pre.className = 'ed-code key-cmd';
        pre.textContent = s.cmd;
        usage.appendChild(pre);

        // 带参数的完整用法单独展示：不在复制内容里，避免把占位符
        // 当成真实路径粘进终端。
        if (s.example) {
          var ex = document.createElement('div');
          ex.className = 'key-usage-example';
          ex.textContent = '示例：' + s.example;
          usage.appendChild(ex);
        }
        if (s.note) {
          var note = document.createElement('div');
          note.className = 'key-usage-note';
          note.textContent = s.note;
          usage.appendChild(note);
        }
      });
      box.appendChild(usage);

      var modal = G.openModal({
        title: '密钥已生成',
        body: box,
        wide: true,
        footer: G.modalFoot([
          { label: '我已保存', cls: 'primary', onClick: function () { modal.close(); } }
        ])
      });
    }

    // ---- 列表 ----

    async function load() {
      listBox.textContent = '';
      listTitle.textContent = '已有密钥';
      var res;
      try {
        res = await G.apiFetch(KEYS_EP);
      } catch (e) {
        listBox.textContent = '加载失败：' + e.message;
        return;
      }
      if (!res.ok) {
        listBox.textContent = '加载失败：' + (await res.text());
        return;
      }
      var data = await res.json();
      var keys = data.keys || [];

      currentKeys = keys;
      selected = {};
      selectAll.checked = false;
      selectAll.indeterminate = false;
      updateBulk();

      listTitle.textContent = '已有密钥（' + keys.length + '）' +
        (data.persistent ? '' : '　·　当前为内存存储，重启后失效');

      if (!keys.length) {
        var empty = document.createElement('div');
        empty.className = 'key-empty';
        empty.textContent = '还没有任何密钥。';
        listBox.appendChild(empty);
        return;
      }

      keys.forEach(function (k) {
        var item = document.createElement('div');
        item.className = 'key-item selectable' + (k.status === 'expired' ? ' expired' : '');

        var checkWrap = document.createElement('label');
        checkWrap.className = 'key-check';
        var cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.setAttribute('aria-label', '选择「' + k.name + '」');
        cb.onchange = function () {
          if (cb.checked) {
            selected[k.id] = k;
          } else {
            delete selected[k.id];
          }
          item.classList.toggle('selected', cb.checked);
          updateBulk();
        };
        checkWrap.appendChild(cb);
        item.appendChild(checkWrap);

        var main = document.createElement('div');
        main.className = 'key-main';

        var head = document.createElement('div');
        head.className = 'key-name';
        head.textContent = k.name;
        var badge = document.createElement('span');
        badge.className = 'key-badge ' + k.status;
        badge.textContent = k.status === 'expired' ? '已过期' : '有效';
        head.appendChild(badge);
        main.appendChild(head);

        var codeLine = document.createElement('code');
        codeLine.className = 'key-prefix';
        codeLine.textContent = k.prefix + '…';
        main.appendChild(codeLine);

        var details = [
          '范围 ' + k.scope,
          // 类型必须写出来：列表里「只读分享」和「可读写分享」长得一模一样，
          // 而后者拿到链接的人能删目录里的东西 —— 撤销哪一条得能一眼分清。
          '类型 ' + kindLabel(k.kind),
          '创建者 ' + (k.creator || '—'),
          '创建于 ' + G.fmtTime(k.created_at),
          k.permanent ? '永不过期' : '有效期至 ' + G.fmtTime(k.expires_at) +
            '（' + fmtRemaining(k.remaining_seconds) + '）',
          k.use_count > 0
            ? '已使用 ' + k.use_count + ' 次，最近 ' + G.fmtTime(k.last_used_at)
            : '尚未使用'
        ];
        details.forEach(function (d) {
          var line = document.createElement('div');
          line.className = 'key-detail';
          line.textContent = d;
          main.appendChild(line);
        });

        item.appendChild(main);

        var actions = document.createElement('div');
        actions.className = 'key-actions';
        var del = G.btn('撤销', 'danger', function () {
          G.confirm({
            title: '撤销密钥',
            message: '确定要撤销「' + k.name + '」吗？',
            detail: '使用它的脚本会立即收到 401，无法恢复。',
            okLabel: '撤销',
            danger: true
          }).then(function (yes) {
            if (!yes) return;
            revoke(k);
          });
        });
        del.classList.add('btn-sm');
        actions.appendChild(del);
        item.appendChild(actions);

        listBox.appendChild(item);
      });
    }

    async function revoke(k) {
      try {
        var res = await G.apiFetch(KEYS_EP + '?id=' + encodeURIComponent(k.id), { method: 'DELETE' });
        if (res.status !== 204 && !res.ok) {
          throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        }
        G.toast('ok', '已撤销「' + k.name + '」');
        await load();
      } catch (e) {
        G.toast('err', '撤销失败：' + e.message);
      }
    }

    // 初始加载（不阻塞返回：列表先显示「正在读取…」，回来再填）。
    load();

    return { el: body, buttons: [], refresh: function () { return load(); } };
  }

  G.keysPanel = keysPanel;
})();
