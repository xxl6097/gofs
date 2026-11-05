/* gofs WebDAV 账号管理。
 *
 * 为 WebDAV 提供独立的账号管理界面，支持：
 *   - 创建 WebDAV 账号并指定可访问的目录
 *   - 更新账号的目录权限
 *   - 修改 WebDAV 账号密码
 *   - 删除 WebDAV 账号
 *
 * 与普通用户管理分离，WebDAV 账号仅用于 WebDAV 协议访问。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var EP = G.raw('/api/webdav/accounts');

  // ---------------------------------------------------------------- 工具函数

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

  function formatTimestamp(ts) {
    if (!ts) return '-';
    var d = new Date(ts * 1000);
    var pad = function (n) { return n < 10 ? '0' + n : n; };
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  async function fetchAccounts() {
    var res = await G.apiFetch(EP, { headers: { 'Accept': 'application/json' } });
    if (!res.ok) throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
    return res.json();
  }

  // ---------------------------------------------------------------- 目录选择器

  function dirSelector(initial) {
    var wrap = document.createElement('div');
    wrap.className = 'dir-list';

    function addDir(path) {
      var row = document.createElement('div');
      row.className = 'dir-row';
      row.style.cssText = 'display:flex;gap:8px;align-items:center;margin-bottom:8px;';

      var input = document.createElement('input');
      input.type = 'text';
      input.className = 'input';
      input.placeholder = '/docs';
      input.spellcheck = false;
      input.value = path || '';
      input.style.flex = '1';
      row.appendChild(input);

      var del = G.btn('移除', 'danger btn-sm', function () { row.remove(); });
      row.appendChild(del);

      wrap.appendChild(row);
      return input;
    }

    (initial || []).forEach(function (d) { addDir(d); });
    if (!initial || initial.length === 0) addDir('');

    var addBtn = G.btn('+ 添加目录', 'secondary btn-sm', function () {
      var inp = addDir('');
      inp.focus();
    });
    addBtn.style.marginTop = '8px';
    wrap.appendChild(addBtn);

    return {
      el: wrap,
      dirs: function () {
        var inputs = wrap.querySelectorAll('.dir-row input');
        var result = [];
        for (var i = 0; i < inputs.length; i++) {
          var v = inputs[i].value.trim();
          if (v) result.push(v);
        }
        return result;
      }
    };
  }

  // ---------------------------------------------------------------- 创建账号对话框

  function openCreateDialog(onSuccess) {
    var overlay = G.overlay();
    var dialog = G.dialog('创建 WebDAV 账号', overlay);

    var form = document.createElement('form');
    form.style.cssText = 'display:flex;flex-direction:column;gap:16px;padding:20px;';

    // 用户名
    var usernameField = document.createElement('div');
    usernameField.innerHTML = '<label style="display:block;margin-bottom:4px;font-weight:500;">用户名</label>';
    var usernameInput = document.createElement('input');
    usernameInput.type = 'text';
    usernameInput.className = 'input';
    usernameInput.placeholder = '输入用户名';
    usernameInput.required = true;
    usernameField.appendChild(usernameInput);
    form.appendChild(usernameField);

    // 密码
    var passwordField = document.createElement('div');
    passwordField.innerHTML = '<label style="display:block;margin-bottom:4px;font-weight:500;">密码</label>';
    var passwordRow = document.createElement('div');
    passwordRow.style.cssText = 'display:flex;gap:8px;';
    var passwordInput = document.createElement('input');
    passwordInput.type = 'text';
    passwordInput.className = 'input';
    passwordInput.placeholder = '输入密码';
    passwordInput.required = true;
    passwordInput.style.flex = '1';
    passwordRow.appendChild(passwordInput);
    var genBtn = G.btn('随机生成', 'secondary btn-sm', function () {
      passwordInput.value = randomPassword(12);
    });
    passwordRow.appendChild(genBtn);
    passwordField.appendChild(passwordRow);
    form.appendChild(passwordField);

    // 允许访问的目录
    var dirsField = document.createElement('div');
    dirsField.innerHTML = '<label style="display:block;margin-bottom:8px;font-weight:500;">允许访问的目录</label>';
    var selector = dirSelector(['/']);
    dirsField.appendChild(selector.el);
    form.appendChild(dirsField);

    var hint = document.createElement('div');
    hint.style.cssText = 'font-size:13px;color:#666;padding:8px;background:#f5f5f5;border-radius:4px;';
    hint.textContent = '提示：账号只能访问指定的目录及其子目录';
    form.appendChild(hint);

    var error = document.createElement('div');
    error.className = 'error';
    error.style.marginTop = '8px';
    form.appendChild(error);

    dialog.appendChild(form);

    // 底部按钮
    var footer = document.createElement('div');
    footer.style.cssText = 'display:flex;gap:8px;justify-content:flex-end;padding:16px 20px;border-top:1px solid #e0e0e0;';

    var cancelBtn = G.btn('取消', 'secondary', function () { overlay.remove(); });
    var createBtn = G.btn('创建', 'primary', async function () {
      error.textContent = '';
      var username = usernameInput.value.trim();
      var password = passwordInput.value.trim();
      var dirs = selector.dirs();

      if (!username) {
        error.textContent = '请输入用户名';
        usernameInput.focus();
        return;
      }
      if (!password) {
        error.textContent = '请输入密码';
        passwordInput.focus();
        return;
      }
      if (dirs.length === 0) {
        error.textContent = '至少需要指定一个允许访问的目录';
        return;
      }

      createBtn.disabled = true;
      createBtn.textContent = '创建中...';

      try {
        var res = await G.apiFetch(EP, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            username: username,
            password: password,
            allowed_dirs: dirs
          })
        });

        if (!res.ok) {
          var msg = await res.text();
          throw new Error(msg.trim() || ('HTTP ' + res.status));
        }

        G.toast('账号创建成功');
        overlay.remove();
        if (onSuccess) onSuccess();
      } catch (e) {
        error.textContent = '创建失败: ' + e.message;
        createBtn.disabled = false;
        createBtn.textContent = '创建';
      }
    });

    footer.appendChild(cancelBtn);
    footer.appendChild(createBtn);
    dialog.appendChild(footer);

    usernameInput.focus();
  }

  // ---------------------------------------------------------------- 修改目录对话框

  function openEditDialog(account, onSuccess) {
    var overlay = G.overlay();
    var dialog = G.dialog('修改访问权限 - ' + account.username, overlay);

    var form = document.createElement('form');
    form.style.cssText = 'display:flex;flex-direction:column;gap:16px;padding:20px;';

    var dirsField = document.createElement('div');
    dirsField.innerHTML = '<label style="display:block;margin-bottom:8px;font-weight:500;">允许访问的目录</label>';
    var selector = dirSelector(account.allowed_dirs);
    dirsField.appendChild(selector.el);
    form.appendChild(dirsField);

    var error = document.createElement('div');
    error.className = 'error';
    form.appendChild(error);

    dialog.appendChild(form);

    var footer = document.createElement('div');
    footer.style.cssText = 'display:flex;gap:8px;justify-content:flex-end;padding:16px 20px;border-top:1px solid #e0e0e0;';

    var cancelBtn = G.btn('取消', 'secondary', function () { overlay.remove(); });
    var saveBtn = G.btn('保存', 'primary', async function () {
      error.textContent = '';
      var dirs = selector.dirs();

      if (dirs.length === 0) {
        error.textContent = '至少需要指定一个允许访问的目录';
        return;
      }

      saveBtn.disabled = true;
      saveBtn.textContent = '保存中...';

      try {
        var res = await G.apiFetch(EP + '/' + account.username, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ allowed_dirs: dirs })
        });

        if (!res.ok) {
          var msg = await res.text();
          throw new Error(msg.trim() || ('HTTP ' + res.status));
        }

        G.toast('权限更新成功');
        overlay.remove();
        if (onSuccess) onSuccess();
      } catch (e) {
        error.textContent = '更新失败: ' + e.message;
        saveBtn.disabled = false;
        saveBtn.textContent = '保存';
      }
    });

    footer.appendChild(cancelBtn);
    footer.appendChild(saveBtn);
    dialog.appendChild(footer);
  }

  // ---------------------------------------------------------------- 修改密码对话框

  function openPasswordDialog(account, onSuccess) {
    var overlay = G.overlay();
    var dialog = G.dialog('修改密码 - ' + account.username, overlay);

    var form = document.createElement('form');
    form.style.cssText = 'display:flex;flex-direction:column;gap:16px;padding:20px;';

    var passwordField = document.createElement('div');
    passwordField.innerHTML = '<label style="display:block;margin-bottom:4px;font-weight:500;">新密码</label>';
    var passwordRow = document.createElement('div');
    passwordRow.style.cssText = 'display:flex;gap:8px;';
    var passwordInput = document.createElement('input');
    passwordInput.type = 'text';
    passwordInput.className = 'input';
    passwordInput.placeholder = '输入新密码';
    passwordInput.required = true;
    passwordInput.style.flex = '1';
    passwordRow.appendChild(passwordInput);
    var genBtn = G.btn('随机生成', 'secondary btn-sm', function () {
      passwordInput.value = randomPassword(12);
    });
    passwordRow.appendChild(genBtn);
    passwordField.appendChild(passwordRow);
    form.appendChild(passwordField);

    var error = document.createElement('div');
    error.className = 'error';
    form.appendChild(error);

    dialog.appendChild(form);

    var footer = document.createElement('div');
    footer.style.cssText = 'display:flex;gap:8px;justify-content:flex-end;padding:16px 20px;border-top:1px solid #e0e0e0;';

    var cancelBtn = G.btn('取消', 'secondary', function () { overlay.remove(); });
    var saveBtn = G.btn('保存', 'primary', async function () {
      error.textContent = '';
      var password = passwordInput.value.trim();

      if (!password) {
        error.textContent = '请输入新密码';
        passwordInput.focus();
        return;
      }

      saveBtn.disabled = true;
      saveBtn.textContent = '保存中...';

      try {
        var res = await G.apiFetch(EP + '/' + account.username + '/password', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ new_password: password })
        });

        if (!res.ok) {
          var msg = await res.text();
          throw new Error(msg.trim() || ('HTTP ' + res.status));
        }

        G.toast('密码修改成功');
        overlay.remove();
        if (onSuccess) onSuccess();
      } catch (e) {
        error.textContent = '修改失败: ' + e.message;
        saveBtn.disabled = false;
        saveBtn.textContent = '保存';
      }
    });

    footer.appendChild(cancelBtn);
    footer.appendChild(saveBtn);
    dialog.appendChild(footer);

    passwordInput.focus();
  }

  // ---------------------------------------------------------------- 主面板

  function webdavPanel() {
    var wrap = document.createElement('div');
    wrap.style.cssText = 'padding:20px;';

    var header = document.createElement('div');
    header.style.cssText = 'margin-bottom:16px;';

    var title = document.createElement('h3');
    title.textContent = 'WebDAV 账号管理';
    title.style.marginBottom = '8px';
    header.appendChild(title);

    var desc = document.createElement('p');
    desc.style.cssText = 'color:#666;font-size:14px;margin:0;';
    desc.textContent = 'WebDAV 账号用于通过 WebDAV 协议访问文件，每个账号可指定允许访问的目录。';
    header.appendChild(desc);

    wrap.appendChild(header);

    var list = document.createElement('div');
    list.style.cssText = 'border:1px solid #e0e0e0;border-radius:4px;';

    var loading = document.createElement('div');
    loading.textContent = '加载中...';
    loading.style.cssText = 'padding:40px;text-align:center;color:#999;';
    list.appendChild(loading);

    wrap.appendChild(list);

    async function refresh() {
      try {
        var data = await fetchAccounts();
        var accounts = data.accounts || [];

        list.innerHTML = '';

        if (accounts.length === 0) {
          var empty = document.createElement('div');
          empty.textContent = '暂无 WebDAV 账号';
          empty.style.cssText = 'padding:40px;text-align:center;color:#999;';
          list.appendChild(empty);
          return;
        }

        accounts.forEach(function (acc) {
          var row = document.createElement('div');
          row.style.cssText = 'padding:16px;border-bottom:1px solid #e0e0e0;display:flex;justify-content:space-between;align-items:center;';

          var info = document.createElement('div');
          info.style.flex = '1';

          var username = document.createElement('div');
          username.style.cssText = 'font-weight:500;margin-bottom:4px;';
          username.textContent = acc.username;
          info.appendChild(username);

          var dirs = document.createElement('div');
          dirs.style.cssText = 'font-size:13px;color:#666;margin-bottom:4px;';
          dirs.textContent = '允许访问: ' + (acc.allowed_dirs || []).join(', ');
          info.appendChild(dirs);

          var meta = document.createElement('div');
          meta.style.cssText = 'font-size:12px;color:#999;';
          meta.textContent = '创建时间: ' + formatTimestamp(acc.created_at);
          if (acc.updated_at && acc.updated_at !== acc.created_at) {
            meta.textContent += ' · 更新时间: ' + formatTimestamp(acc.updated_at);
          }
          info.appendChild(meta);

          row.appendChild(info);

          var actions = document.createElement('div');
          actions.style.cssText = 'display:flex;gap:8px;';

          var editBtn = G.btn('修改权限', 'secondary btn-sm', function () {
            openEditDialog(acc, refresh);
          });
          actions.appendChild(editBtn);

          var pwdBtn = G.btn('改密码', 'secondary btn-sm', function () {
            openPasswordDialog(acc, refresh);
          });
          actions.appendChild(pwdBtn);

          var delBtn = G.btn('删除', 'danger btn-sm', async function () {
            if (!confirm('确定要删除账号 ' + acc.username + ' 吗？')) return;
            try {
              var res = await G.apiFetch(EP + '/' + acc.username, { method: 'DELETE' });
              if (!res.ok) {
                var msg = await res.text();
                throw new Error(msg.trim() || ('HTTP ' + res.status));
              }
              G.toast('账号已删除');
              refresh();
            } catch (e) {
              alert('删除失败: ' + e.message);
            }
          });
          actions.appendChild(delBtn);

          row.appendChild(actions);
          list.appendChild(row);
        });
      } catch (e) {
        list.innerHTML = '';
        var error = document.createElement('div');
        error.textContent = '加载失败: ' + e.message;
        error.style.cssText = 'padding:40px;text-align:center;color:#d32f2f;';
        list.appendChild(error);
      }
    }

    refresh();

    return {
      el: wrap,
      refresh: refresh,
      footer: function () {
        var f = document.createElement('div');
        f.style.cssText = 'display:flex;gap:8px;justify-content:flex-end;';
        var closeBtn = G.btn('关闭', 'secondary', function () {
          // 由外层处理
        });
        var newBtn = G.btn('新建账号', 'primary', function () {
          openCreateDialog(refresh);
        });
        f.appendChild(closeBtn);
        f.appendChild(newBtn);
        return f;
      }
    };
  }

  // 暴露给设置面板使用
  G.webdavPanel = webdavPanel;

})();
