/* gofs WebDAV 账号管理。
 *
 * WebDAV 账号只用于 WebDAV 协议（Basic Auth）访问文件，与网页登录账号分开：
 *   - 创建账号并指定可访问的目录
 *   - 修改账号的目录权限
 *   - 重置账号密码
 *   - 删除账号
 *
 * 复用 app.js 暴露的 window.GOFS（G），弹窗一律走 G.openModal + G.modalFoot，
 * 样式复用用户管理面板的 .key-* / .rule-* 类，保证与其它设置页一致（含深色主题）。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var EP = G.raw('/api/webdav/accounts');

  // ---------------------------------------------------------------- 小工具

  // randomPassword 生成便于手抄的随机密码（去掉 0/O/1/l/I 等易混字符）。
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

  function fetchAccounts() {
    return G.apiFetch(EP, { headers: { 'Accept': 'application/json' } })
      .then(function (res) {
        if (!res.ok) {
          return res.text().then(function (t) {
            throw new Error(t.trim() || ('HTTP ' + res.status));
          });
        }
        return res.json();
      });
  }

  // dirURL 把授权目录拼成 WebDAV 连接地址："/docs" → origin/dav/docs/，
  // "/" → origin/dav/。客户端要连的正是这个地址，连根目录可能是 403。
  function dirURL(dir) {
    var clean = String(dir || '').replace(/^\/+/, '').replace(/\/+$/, '');
    return location.origin + G.raw('/dav/' + (clean ? clean + '/' : ''));
  }

  function errText(res) {
    return res.text().then(function (t) {
      return t.trim() || ('HTTP ' + res.status);
    });
  }

  // ---------------------------------------------------------------- 目录编辑

  // dirEditor 维护一组「允许访问的目录」输入行。返回 { el, dirs() }。
  function dirEditor(initial) {
    var wrap = document.createElement('div');
    wrap.className = 'rule-list';

    function addRow(path) {
      var row = document.createElement('div');
      row.className = 'rule-row';

      var input = document.createElement('input');
      input.type = 'text';
      input.className = 'rule-path';
      input.placeholder = '/docs';
      input.spellcheck = false;
      input.autocomplete = 'off';
      input.value = path || '';
      row.appendChild(input);

      row.appendChild(G.btn('移除', 'danger btn-sm', function () { row.remove(); }));
      wrap.appendChild(row);
      return input;
    }

    (initial && initial.length ? initial : ['/'])
      .forEach(function (d) { addRow(d); });

    var add = G.btn('添加目录', '', function () {
      addRow('').focus();
    });
    add.style.marginTop = '6px';

    var box = document.createElement('div');
    box.appendChild(wrap);
    box.appendChild(add);

    return {
      el: box,
      dirs: function () {
        var inputs = wrap.querySelectorAll('.rule-path');
        var out = [];
        for (var i = 0; i < inputs.length; i++) {
          var v = inputs[i].value.trim();
          if (!v) continue;
          // 统一成以 / 开头，避免 "/docs" 与 "docs" 在 ACL 里被当成两条。
          if (v.charAt(0) !== '/') v = '/' + v;
          out.push(v);
        }
        return out;
      }
    };
  }

  // 密码输入行：输入框 + 显示/隐藏 + 随机生成。
  function passwordRow(placeholder) {
    var wrap = document.createElement('div');
    wrap.className = 'pw-row';

    var input = document.createElement('input');
    input.type = 'password';
    input.className = 'key-input';
    input.placeholder = placeholder || '输入密码';
    input.autocomplete = 'new-password';
    wrap.appendChild(input);

    var showBtn = G.btn('显示', 'btn-sm', function () {
      var hidden = input.type === 'password';
      input.type = hidden ? 'text' : 'password';
      showBtn.textContent = hidden ? '隐藏' : '显示';
    });
    wrap.appendChild(showBtn);

    wrap.appendChild(G.btn('随机生成', 'btn-sm', function () {
      input.type = 'text';
      showBtn.textContent = '隐藏';
      input.value = randomPassword(16);
    }));

    return { el: wrap, input: input };
  }

  // ---------------------------------------------------------------- 创建账号

  function openCreateDialog(onDone) {
    var body = document.createElement('div');

    var nameField = document.createElement('div');
    nameField.className = 'key-field';
    var nameLabel = document.createElement('label');
    nameLabel.textContent = '用户名';
    nameField.appendChild(nameLabel);
    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.className = 'key-input';
    nameInput.placeholder = '例如 alice';
    nameInput.spellcheck = false;
    nameInput.autocomplete = 'off';
    nameField.appendChild(nameInput);
    var nameHint = document.createElement('div');
    nameHint.className = 'key-hint';
    nameHint.textContent = '用于 WebDAV Basic Auth，不能与已有账号重名。';
    nameField.appendChild(nameHint);
    body.appendChild(nameField);

    var pwField = document.createElement('div');
    pwField.className = 'key-field';
    var pwLabel = document.createElement('label');
    pwLabel.textContent = '密码';
    pwField.appendChild(pwLabel);
    var pw = passwordRow('输入密码');
    pwField.appendChild(pw.el);
    var pwHint = document.createElement('div');
    pwHint.className = 'key-hint';
    pwHint.textContent = '密码只保存 PBKDF2 摘要，服务端无法找回；忘记请重置。';
    pwField.appendChild(pwHint);
    body.appendChild(pwField);

    var dirField = document.createElement('div');
    dirField.className = 'key-field';
    var dirLabel = document.createElement('label');
    dirLabel.textContent = '允许访问的目录';
    dirField.appendChild(dirLabel);
    var dirs = dirEditor(['/']);
    dirField.appendChild(dirs.el);
    var dirHint = document.createElement('div');
    dirHint.className = 'key-hint';
    dirHint.textContent = '只能访问列出的目录及其子目录；其它路径一律拒绝。';
    dirField.appendChild(dirHint);
    body.appendChild(dirField);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;
    body.appendChild(errBox);

    var modal = G.openModal({
      title: '创建 WebDAV 账号',
      body: body,
      wide: true,
      footer: G.modalFoot([
        { label: '取消', onClick: function () { modal.close(); } },
        { label: '创建', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    function fail(msg) {
      errBox.hidden = false;
      errBox.textContent = msg;
    }

    function submit() {
      errBox.hidden = true;
      var username = nameInput.value.trim();
      var password = pw.input.value;
      var list = dirs.dirs();

      if (!username) { fail('请填写用户名'); nameInput.focus(); return; }
      if (!password) { fail('请填写密码'); pw.input.focus(); return; }
      if (!list.length) { fail('至少要指定一个允许访问的目录'); return; }

      var btn = modal.el.querySelector('.modal-foot .btn.primary');
      if (btn) { btn.disabled = true; btn.textContent = '创建中…'; }

      G.apiFetch(EP, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: username, password: password, allowed_dirs: list })
      }).then(function (res) {
        if (!res.ok) return errText(res).then(function (m) { throw new Error(m); });
        modal.close();
        G.toast('ok', '已创建 WebDAV 账号 ' + username);
        if (onDone) onDone();
      }).catch(function (e) {
        fail('创建失败：' + e.message);
        if (btn) { btn.disabled = false; btn.textContent = '创建'; }
      });
    }

    nameInput.focus();
  }

  // ---------------------------------------------------------------- 修改权限

  function openEditDialog(account, onDone) {
    var body = document.createElement('div');

    var dirField = document.createElement('div');
    dirField.className = 'key-field';
    var dirLabel = document.createElement('label');
    dirLabel.textContent = '允许访问的目录';
    dirField.appendChild(dirLabel);
    var dirs = dirEditor(account.allowed_dirs);
    dirField.appendChild(dirs.el);
    var dirHint = document.createElement('div');
    dirHint.className = 'key-hint';
    dirHint.textContent = '修改后立即生效，已在连接的客户端可能需要重新认证。';
    dirField.appendChild(dirHint);
    body.appendChild(dirField);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;
    body.appendChild(errBox);

    var modal = G.openModal({
      title: '修改访问权限 · ' + account.username,
      body: body,
      wide: true,
      footer: G.modalFoot([
        { label: '取消', onClick: function () { modal.close(); } },
        { label: '保存', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    function submit() {
      errBox.hidden = true;
      var list = dirs.dirs();
      if (!list.length) {
        errBox.hidden = false;
        errBox.textContent = '至少要指定一个允许访问的目录';
        return;
      }

      var btn = modal.el.querySelector('.modal-foot .btn.primary');
      if (btn) { btn.disabled = true; btn.textContent = '保存中…'; }

      G.apiFetch(EP + '/' + encodeURIComponent(account.username), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ allowed_dirs: list })
      }).then(function (res) {
        if (!res.ok) return errText(res).then(function (m) { throw new Error(m); });
        modal.close();
        G.toast('ok', '已更新 ' + account.username + ' 的访问权限');
        if (onDone) onDone();
      }).catch(function (e) {
        errBox.hidden = false;
        errBox.textContent = '更新失败：' + e.message;
        if (btn) { btn.disabled = false; btn.textContent = '保存'; }
      });
    }
  }

  // ---------------------------------------------------------------- 修改密码

  function openPasswordDialog(account, onDone) {
    var body = document.createElement('div');

    var pwField = document.createElement('div');
    pwField.className = 'key-field';
    var pwLabel = document.createElement('label');
    pwLabel.textContent = '新密码';
    pwField.appendChild(pwLabel);
    var pw = passwordRow('输入新密码');
    pwField.appendChild(pw.el);
    var pwHint = document.createElement('div');
    pwHint.className = 'key-hint';
    pwHint.textContent = '只影响 WebDAV 访问，不影响网页登录密码。';
    pwField.appendChild(pwHint);
    body.appendChild(pwField);

    var errBox = document.createElement('div');
    errBox.className = 'hint-box danger';
    errBox.hidden = true;
    body.appendChild(errBox);

    var modal = G.openModal({
      title: '重置密码 · ' + account.username,
      body: body,
      footer: G.modalFoot([
        { label: '取消', onClick: function () { modal.close(); } },
        { label: '保存', cls: 'primary', onClick: function () { submit(); } }
      ])
    });

    function submit() {
      errBox.hidden = true;
      var password = pw.input.value;
      if (!password) {
        errBox.hidden = false;
        errBox.textContent = '请输入新密码';
        pw.input.focus();
        return;
      }

      var btn = modal.el.querySelector('.modal-foot .btn.primary');
      if (btn) { btn.disabled = true; btn.textContent = '保存中…'; }

      G.apiFetch(EP + '/' + encodeURIComponent(account.username) + '/password', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ new_password: password })
      }).then(function (res) {
        if (!res.ok) return errText(res).then(function (m) { throw new Error(m); });
        modal.close();
        G.toast('ok', '已重置 ' + account.username + ' 的密码');
        if (onDone) onDone();
      }).catch(function (e) {
        errBox.hidden = false;
        errBox.textContent = '修改失败：' + e.message;
        if (btn) { btn.disabled = false; btn.textContent = '保存'; }
      });
    }

    pw.input.focus();
  }

  // ---------------------------------------------------------------- 主面板

  function webdavPanel() {
    var body = document.createElement('div');

    var intro = document.createElement('div');
    intro.className = 'hint-box';
    intro.style.marginBottom = '14px';
    intro.textContent = 'WebDAV 账号只用于 WebDAV 客户端（Windows 映射网络驱动器、' +
      'macOS 连接服务器等），每个账号只能访问指定的目录。';
    body.appendChild(intro);

    var listTitle = document.createElement('div');
    listTitle.className = 'key-list-title';
    body.appendChild(listTitle);

    var listBox = document.createElement('div');
    listBox.className = 'key-list';
    body.appendChild(listBox);

    var tipBox = document.createElement('div');
    body.appendChild(tipBox);

    function render(data) {
      if (!data) {
        listBox.textContent = '正在读取…';
        fetchAccounts().then(render).catch(function (e) {
          listBox.textContent = '加载失败：' + e.message;
        });
        return;
      }

      var accounts = data.accounts || [];
      listTitle.textContent = '账号（' + accounts.length + '）';

      listBox.textContent = '';
      if (!accounts.length) {
        var empty = document.createElement('div');
        empty.className = 'key-empty';
        empty.textContent = '还没有 WebDAV 账号。';
        listBox.appendChild(empty);
      }

      accounts.forEach(function (acc) {
        var item = document.createElement('div');
        item.className = 'key-item';

        var main = document.createElement('div');
        main.className = 'key-main';

        var head = document.createElement('div');
        head.className = 'key-name';
        head.textContent = acc.username;
        main.appendChild(head);

        var dirs = document.createElement('div');
        dirs.className = 'user-rules';
        (acc.allowed_dirs || []).forEach(function (d) {
          var chip = document.createElement('span');
          chip.className = 'rule-chip rw';
          chip.textContent = d;
          dirs.appendChild(chip);
        });
        main.appendChild(dirs);

        // 每个账号直接给出可用地址：只授权子目录时，连 /dav/ 根目录会被 403，
        // 这是最常见的「账号登录无法访问」原因。
        var urls = (acc.allowed_dirs || []).map(dirURL);
        if (urls.length) {
          var conn = document.createElement('div');
          conn.className = 'pw-row';
          conn.style.marginTop = '4px';

          var connText = document.createElement('div');
          connText.className = 'key-detail';
          connText.style.wordBreak = 'break-all';
          connText.textContent = '连接地址：' + urls.join('　·　');
          conn.appendChild(connText);

          conn.appendChild(G.btn('复制', 'btn-sm', function () {
            G.copyText(urls.join('\n')).then(function () {
              G.toast('ok', '已复制连接地址');
            }).catch(function (e) {
              G.toast('err', e.message);
            });
          }));
          main.appendChild(conn);
        }

        var meta = document.createElement('div');
        meta.className = 'key-detail';
        meta.textContent = '创建于 ' + G.fmtTime(acc.created_at) +
          (acc.updated_at && acc.updated_at !== acc.created_at
            ? '　·　更新于 ' + G.fmtTime(acc.updated_at) : '');
        main.appendChild(meta);
        item.appendChild(main);

        var actions = document.createElement('div');
        actions.className = 'key-actions';
        actions.appendChild(G.btn('修改权限', '', function () {
          openEditDialog(acc, refresh);
        }));
        actions.appendChild(G.btn('改密码', '', function () {
          openPasswordDialog(acc, refresh);
        }));
        actions.appendChild(G.btn('删除', 'danger', function () {
          doDelete(acc);
        }));
        item.appendChild(actions);

        listBox.appendChild(item);
      });

      // 连接说明：把「地址填什么」写清楚，避免用户到处找。
      tipBox.textContent = '';
      var tip = document.createElement('div');
      tip.className = 'hint-box';
      tip.style.marginTop = '12px';
      tip.textContent = '认证方式：用户名 + 密码（HTTP Basic Auth）。' +
        '客户端必须连接账号被授权的目录（见上方的连接地址）：' +
        '账号没有授权 "/" 时，连 ' + location.origin + G.raw('/dav/') +
        ' 会返回 403，这是权限限制，不是故障。';
      tipBox.appendChild(tip);
    }

    function doDelete(acc) {
      G.confirm({
        title: '删除 WebDAV 账号 ' + acc.username + '？',
        message: '该账号将立即无法连接，已经建立的客户端会话也会失效。',
        detail: '删除后无法恢复。',
        okLabel: '删除',
        danger: true
      }).then(function (ok) {
        if (!ok) return;
        return G.apiFetch(EP + '/' + encodeURIComponent(acc.username), { method: 'DELETE' })
          .then(function (res) {
            if (!res.ok) return errText(res).then(function (m) { throw new Error(m); });
            G.toast('ok', '已删除 WebDAV 账号 ' + acc.username);
            refresh();
          });
      }).catch(function (e) {
        G.toast('err', '删除失败：' + e.message);
      });
    }

    function refresh() {
      return render();
    }
    refresh();

    return {
      el: body,
      refresh: refresh,
      // 设置面板用 G.modalFoot 组装底部按钮，字段必须是 onClick；
      // 非「常规」页会自动补一个「关闭」，这里不要重复添加。
      buttons: [{
        label: '新建账号',
        cls: 'primary',
        onClick: function () { openCreateDialog(refresh); }
      }]
    };
  }

  G.webdavPanel = webdavPanel;
})();
