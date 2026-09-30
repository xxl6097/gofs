/* gofs 上传密钥管理。
 *
 * 上传密钥是给脚本 / 第三方用的最小权限凭证：只能上传、可限定目标目录、
 * 可设有效期、可随时撤销。明文只在创建时显示一次，服务端只保存摘要。
 *
 * 复用 app.js 暴露的 window.GOFS。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var KEYS_EP = G.raw('/__gofs__/keys');

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

  // copyText 优先用剪贴板 API；非安全上下文（纯 HTTP）下退回 execCommand。
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

  function serviceBase() {
    return location.origin + (G.data.path_prefix || '');
  }

  // ---------------------------------------------------------------- 主界面

  async function openKeys() {
    var body = document.createElement('div');

    // 顶部说明
    var intro = document.createElement('div');
    intro.className = 'hint-box';
    intro.style.marginBottom = '14px';
    intro.textContent = '上传密钥用于让脚本或第三方在不上交账号密码的前提下上传文件：' +
      '它只能上传，不能浏览、删除或修改其它内容，并且可以限定目录、设置有效期、随时撤销。';
    body.appendChild(intro);

    // 创建区
    var createBox = document.createElement('div');
    createBox.className = 'key-create';
    body.appendChild(createBox);

    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.placeholder = '用途备注，例如：备份脚本 / 给同事临时上传';
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
      '只允许写入该目录及其子目录；留 / 表示不限。'));
    createBox.appendChild(row('有效期', ttlSelect));

    var createBtn = G.btn('生成密钥', 'primary', function () { create(); });
    var createRow = document.createElement('div');
    createRow.style.marginTop = '4px';
    createRow.appendChild(createBtn);
    createBox.appendChild(createRow);

    // 列表区
    var listTitle = document.createElement('div');
    listTitle.className = 'key-list-title';
    body.appendChild(listTitle);

    var listBox = document.createElement('div');
    listBox.className = 'key-list';
    body.appendChild(listBox);

    var m = G.openModal({
      title: '上传密钥',
      body: body,
      wide: true,
      footer: G.modalFoot([{ label: '关闭', onClick: function () { m.close(); } }])
    });

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
      warn.textContent = '⚠ 密钥明文只显示这一次。关闭本窗口后无法再查看，请立即复制保存。';
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
        ['一键脚本上传（推荐）',
          'bash <(curl -sS "' + scriptURL + '") 文件或目录... [目标子目录]\n' +
          '# 目录会连层级一起上传；不带目标子目录就传到 ' + data.key.scope],
        ['请求头',
          "curl -T local.txt -H 'X-Gofs-Upload-Key: " + token + "' \"" + base + target + "\""],
        ['查询参数',
          'curl -T local.txt "' + base + target + '?key=' + token + '"'],
        ['保存脚本备用',
          'curl -sSLo gofs-up.sh "' + scriptURL + '" && chmod +x gofs-up.sh']
      ];
      samples.forEach(function (s) {
        var lb = document.createElement('div');
        lb.className = 'key-usage-label';
        lb.textContent = s[0];
        usage.appendChild(lb);
        var pre = document.createElement('pre');
        pre.className = 'ed-code key-cmd';
        pre.textContent = s[1];
        usage.appendChild(pre);
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
        item.className = 'key-item' + (k.status === 'expired' ? ' expired' : '');

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
            detail: '使用该密钥的脚本会立即收到 401，且无法恢复。',
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

    await load();
  }

  G.openKeys = openKeys;
})();
