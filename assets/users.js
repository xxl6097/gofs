/* gofs 用户管理。
 *
 * 账号有两个来源，界面上分得很清楚：
 *   - **来自启动参数**（-a / GOFS_AUTH）：只读，改它要去改命令行；
 *   - **用户表**（users.json）：就是在这一页里增删改的，密码只存 PBKDF2 摘要。
 *
 * 权限模型沿用命令行那套「路径 → rw/r」，按最长前缀匹配，未命中的路径一律拒绝。
 * 复用 app.js 暴露的 window.GOFS。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var EP = G.raw('/__gofs__/users');

  var PERMS = [
    { value: 'rw', label: '可读写' },
    { value: 'r', label: '只读' }
  ];

  // ---------------------------------------------------------------- 小工具

  // randomPassword 生成一个便于手抄的随机密码（去掉了容易看混的 0/O/1/l/I）。
  function randomPassword(len) {
    var alphabet = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789';
    var out = '';
    var buf = new Uint8Array(len);
    if (window.crypto && window.crypto.getRandomValues) {
      window.crypto.getRandomValues(buf);
    } else {
      for (var j = 0; j < len; j++) buf[j] = Math.floor(Math.random() * 256);
    }
    for (var i = 0; i < len; i++) out += alphabet.charAt(buf[i] % alphabet.length);
    return out;
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      ta.setSelectionRange(0, text.length);
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      ta.remove();
      ok ? resolve() : reject(new Error('浏览器拒绝了复制操作，请手动选中复制'));
    });
  }

  // ruleText 把规则列表拼成一行摘要，例如「/ 可读写 · /docs 只读」。
  function ruleText(rules) {
    return (rules || []).map(function (r) {
      return r.path + ' ' + (r.perm === 'rw' ? '可读写' : '只读');
    }).join('　·　');
  }

  async function fetchUsers() {
    var res = await G.apiFetch(EP, { headers: { 'Accept': 'application/json' } });
    if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
    return res.json();
  }

  // ---------------------------------------------------------------- 规则编辑器

  // ruleEditor 维护一组「路径 + 权限」行。返回 { el, rows(), setRows() }。
  function ruleEditor(initial) {
    var wrap = document.createElement('div');
    wrap.className = 'rule-list';

    function addRow(path, perm) {
      var row = document.createElement('div');
      row.className = 'rule-row';

      var pathInput = document.createElement('input');
      pathInput.type = 'text';
      pathInput.className = 'rule-path';
      pathInput.placeholder = '/docs';
      pathInput.spellcheck = false;
      pathInput.value = path || '';
      row.appendChild(pathInput);

      var sel = document.createElement('select');
      sel.className = 'select rule-perm';
      PERMS.forEach(function (p) {
        var o = document.createElement('option');
        o.value = p.value;
        o.textContent = p.label;
        sel.appendChild(o);
      });
      sel.value = perm === 'r' ? 'r' : 'rw';
      row.appendChild(sel);

      var del = G.btn('移除', 'danger btn-sm', function () { row.remove(); });
      row.appendChild(del);

      wrap.appendChild(row);
    }

    (initial && initial.length ? initial : [{ path: '/', perm: 'r' }])
      .forEach(function (r) { addRow(r.path, r.perm); });

    var add = G.btn('添加路径', '', function () { addRow('', 'r'); });
    add.style.marginTop = '6px';

    var box = document.createElement('div');
    box.appendChild(wrap);
    box.appendChild(add);

    return {
      el: box,
      rows: function () {
        return Array.prototype.slice.call(wrap.querySelectorAll('.rule-row'))
          .map(function (row) {
            return {
              path: row.querySelector('.rule-path').value.trim(),
              perm: row.querySelector('.rule-perm').value
            };
          })
          .filter(function (r) { return r.path !== ''; });
      },
      setRows: function (rules) {
        wrap.textContent = '';
        (rules || []).forEach(function (r) { addRow(r.path, r.perm); });
      }
    };
  }

  // ---------------------------------------------------------------- 编辑表单

  // openEditor 打开「新建 / 编辑用户」表单。existing 为空表示新建。
  function openEditor(existing, onSaved) {
    var editing = !!existing;
    var body = document.createElement('div');

    // 用户名
    var nameField = document.createElement('div');
    nameField.className = 'key-field';
    var nameLabel = document.createElement('label');
    nameLabel.textContent = '用户名';
    nameField.appendChild(nameLabel);
    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.className = 'key-input';
    nameInput.placeholder = '例如 zhangsan';
    nameInput.spellcheck = false;
    nameInput.autocomplete = 'off';
    nameInput.value = editing ? existing.name : '';
    if (editing) nameInput.disabled = true; // 用户名即身份，改名等于换个人
    nameField.appendChild(nameInput);
    var nameHint = document.createElement('div');
    nameHint.className = 'key-hint';
    nameHint.textContent = editing
      ? '用户名不可修改（它就是账号身份）。'
      : '不能包含 @ : , / 空格等字符。';
    nameField.appendChild(nameHint);
    body.appendChild(nameField);

    // 密码
    var pwField = document.createElement('div');
    pwField.className = 'key-field';
    var pwLabel = document.createElement('label');
    pwLabel.textContent = editing ? '重置密码（留空则不修改）' : '密码';
    pwField.appendChild(pwLabel);

    var pwRow = document.createElement('div');
    pwRow.className = 'pw-row';
    var pwInput = document.createElement('input');
    pwInput.type = 'password';
    pwInput.className = 'key-input';
    pwInput.placeholder = editing ? '不修改就留空' : '至少 6 位';
    pwInput.autocomplete = 'new-password';
    pwRow.appendChild(pwInput);

    var showBtn = G.btn('显示', 'btn-sm', function () {
      var hidden = pwInput.type === 'password';
      pwInput.type = hidden ? 'text' : 'password';
      showBtn.textContent = hidden ? '隐藏' : '显示';
    });
    pwRow.appendChild(showBtn);

    var genBtn = G.btn('随机生成', 'btn-sm', function () {
      pwInput.type = 'text';
      showBtn.textContent = '隐藏';
      pwInput.value = randomPassword(16);
      copyText(pwInput.value).then(function () {
        G.toast('ok', '已生成并复制到剪贴板');
      }).catch(function () {
        G.toast('warn', '已生成随机密码，请手动记下');
      });
    });
    pwRow.appendChild(genBtn);
    pwField.appendChild(pwRow);

    if (!editing) {
      var pwHint = document.createElement('div');
      pwHint.className = 'key-hint';
      pwHint.textContent = '密码只以 PBKDF2 摘要保存在服务端（users.json），服务端也拿不回明文。';
      pwField.appendChild(pwHint);
    }
    body.appendChild(pwField);

    // 权限
    var ruleField = document.createElement('div');
    ruleField.className = 'key-field';
    var ruleLabel = document.createElement('label');
    ruleLabel.textContent = '访问权限';
    ruleField.appendChild(ruleLabel);
    var editor = ruleEditor(editing ? existing.rules : null);
    ruleField.appendChild(editor.el);
    var ruleHint = document.createElement('div');
    ruleHint.className = 'key-hint';
    ruleHint.innerHTML = '';
    ruleHint.appendChild(document.createTextNode(
      '按「最长前缀」匹配：例如同时给 / 只读、/docs 可读写，则该账号在 /docs 下可写、' +
      '其他位置只读。没有命中任何一条路径的请求一律拒绝。'));
    ruleField.appendChild(ruleHint);
    body.appendChild(ruleField);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;
    body.appendChild(errBox);

    var modal = G.openModal({
      title: editing ? '编辑用户 ' + existing.name : '新建用户',
      body: body,
      wide: true,
      footer: G.modalFoot([
        { label: '取消', onClick: function () { modal.close(); } },
        {
          label: editing ? '保存' : '创建',
          cls: 'primary',
          onClick: function () { submit(); }
        }
      ])
    });

    function fail(msg) {
      errBox.hidden = false;
      errBox.textContent = msg;
    }

    async function submit() {
      errBox.hidden = true;
      var name = nameInput.value.trim();
      var pw = pwInput.value;
      if (!name) { fail('请填写用户名'); return; }
      if (!editing && pw.length < 6) { fail('密码至少 6 位'); return; }
      if (editing && pw !== '' && pw.length < 6) { fail('密码至少 6 位'); return; }
      var rules = editor.rows();
      if (!rules.length) { fail('至少要给一条路径权限'); return; }

      var payload = { name: name, rules: rules };
      // 编辑时留空表示不改密码 —— 不传这个字段，服务端就不会动它。
      if (!editing || pw !== '') payload.password = pw;

      try {
        var res = await G.apiFetch(EP, {
          method: editing ? 'PUT' : 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        modal.close();
        G.toast('ok', editing ? '已保存用户 ' + name : '已创建用户 ' + name);
        onSaved(await res.json());
      } catch (e) {
        fail((editing ? '保存失败：' : '创建失败：') + e.message);
      }
    }
  }

  // ---------------------------------------------------------------- 主界面

  async function openUsers() {
    var body = document.createElement('div');

    var intro = document.createElement('div');
    intro.className = 'hint-box';
    intro.style.marginBottom = '14px';
    intro.textContent = '在这里创建的账号可以登录网页、按路径获得不同的读写权限。' +
      '密码只以摘要形式保存在服务端，创建后无法再查看（忘了就重置）。';
    body.appendChild(intro);

    var listTitle = document.createElement('div');
    listTitle.className = 'key-list-title';
    body.appendChild(listTitle);

    var listBox = document.createElement('div');
    listBox.className = 'key-list';
    body.appendChild(listBox);

    var anonBox = document.createElement('div');
    body.appendChild(anonBox);

    var modal = G.openModal({
      title: '用户与权限',
      body: body,
      wide: true,
      footer: G.modalFoot([
        { label: '关闭', onClick: function () { modal.close(); } },
        {
          label: '新建用户',
          cls: 'primary',
          onClick: function () { openEditor(null, render); }
        }
      ])
    });

    async function render(data) {
      if (!data) {
        listBox.textContent = '正在读取…';
        try {
          data = await fetchUsers();
        } catch (e) {
          listBox.textContent = '加载失败：' + e.message;
          return;
        }
      }
      var users = data.users || [];
      listTitle.textContent = '账号（' + users.length + '）' +
        (data.user_file ? '' : '　·　当前为内存存储，重启后失效');

      listBox.textContent = '';
      if (!users.length) {
        var empty = document.createElement('div');
        empty.className = 'key-empty';
        empty.textContent = '还没有任何账号。';
        listBox.appendChild(empty);
      }

      users.forEach(function (u) {
        var item = document.createElement('div');
        item.className = 'key-item';

        var main = document.createElement('div');
        main.className = 'key-main';

        var head = document.createElement('div');
        head.className = 'key-name';
        head.textContent = u.name;
        if (u.from_startup) {
          var tag = document.createElement('span');
          tag.className = 'user-tag';
          tag.textContent = '来自启动参数';
          tag.title = '在 -a / GOFS_AUTH 里定义，只能在命令行修改，页面不提供编辑';
          head.appendChild(tag);
        }
        main.appendChild(head);

        var rules = document.createElement('div');
        rules.className = 'user-rules';
        (u.rules || []).forEach(function (r) {
          var chip = document.createElement('span');
          chip.className = 'rule-chip' + (r.perm === 'rw' ? ' rw' : '');
          chip.textContent = r.path + (r.perm === 'rw' ? ' 可读写' : ' 只读');
          rules.appendChild(chip);
        });
        main.appendChild(rules);

        var meta = document.createElement('div');
        meta.className = 'key-detail';
        meta.textContent = u.from_startup
          ? '权限来自启动参数'
          : '创建于 ' + G.fmtTime(u.created_at) +
            (u.updated_at ? '　·　更新于 ' + G.fmtTime(u.updated_at) : '');
        main.appendChild(meta);
        item.appendChild(main);

        var actions = document.createElement('div');
        actions.className = 'key-actions';
        if (u.from_startup) {
          var lock = document.createElement('span');
          lock.className = 'user-locked';
          lock.textContent = '只读';
          actions.appendChild(lock);
        } else {
          actions.appendChild(G.btn('编辑', '', function () {
            openEditor(u, render);
          }));
          actions.appendChild(G.btn('删除', 'danger', function () {
            doDelete(u);
          }));
        }
        item.appendChild(actions);
        listBox.appendChild(item);
      });

      // 匿名规则：由启动参数决定，界面上只做说明，避免「为什么没登录也能看」这种困惑。
      anonBox.textContent = '';
      var anon = data.anonymous || [];
      if (anon.length) {
        var box = document.createElement('div');
        box.className = 'hint-box';
        box.style.marginTop = '12px';
        box.textContent = '匿名访问（由启动参数决定，界面上不可改）：' + ruleText(anon);
        anonBox.appendChild(box);
      } else if (data.auth_on) {
        var box2 = document.createElement('div');
        box2.className = 'hint-box';
        box2.style.marginTop = '12px';
        box2.textContent = '未配置匿名访问：所有请求都必须带账号密码。';
        anonBox.appendChild(box2);
      }
    }

    async function doDelete(u) {
      var mine = G.data.user && G.data.user === u.name;
      var ok = await G.confirm({
        title: '删除用户 ' + u.name + '？',
        message: mine
          ? '你正在删除当前登录的账号，删除后这个页面会立刻失去权限。'
          : '该账号将无法再登录，已签发的上传密钥不受影响。',
        detail: '删除后无法恢复，需要重新创建。',
        okLabel: '删除',
        danger: true
      });
      if (!ok) return;
      try {
        var res = await G.apiFetch(EP + '?name=' + encodeURIComponent(u.name),
          { method: 'DELETE' });
        if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        G.toast('ok', '已删除用户 ' + u.name);
        render(await res.json());
      } catch (e) {
        G.toast('err', '删除失败：' + e.message);
      }
    }

    await render();
  }

  G.openUsers = openUsers;
})();
