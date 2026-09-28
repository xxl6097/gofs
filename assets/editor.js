/* gofs 在线文本编辑器。
 *
 * 支持 txt / markdown / html / json / xml / yaml 以及各类源码文件的读取与保存，
 * 并提供 markdown（自研渲染）、html（沙箱 iframe）、json（格式化 + 校验）、
 * xml（格式校验）四种预览。
 *
 * 复用 app.js 暴露的 window.GOFS，不引入任何第三方库。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var TEXT_EP = G.raw('/__gofs__/text');

  // 超过这个行数就不渲染行号，避免大文件把 DOM 撑爆。
  var MAX_GUTTER_LINES = 5000;

  // ---------------------------------------------------------------- 基础工具

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // safeURL 过滤掉 javascript: 之类的可执行协议，避免预览时被注入。
  function safeURL(u) {
    var s = String(u).trim();
    if (/^\s*(javascript|data|vbscript):/i.test(s)) return '';
    return s;
  }

  // ---------------------------------------------------- markdown 渲染（精简版）

  function renderInline(s) {
    // 入参已经是转义后的文本。
    // 行内代码先摘出来，免得其中的 * _ ` 被后续规则误伤。
    var codes = [];
    s = s.replace(/`([^`]+)`/g, function (_, code) {
      codes.push(code);
      return '\u0000C' + (codes.length - 1) + '\u0000';
    });

    s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, function (_, alt, src) {
      var u = safeURL(src);
      if (!u) return alt;
      return '<img src="' + u + '" alt="' + alt + '" loading="lazy">';
    });
    s = s.replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, function (_, text, href) {
      var u = safeURL(href);
      if (!u) return text;
      return '<a href="' + u + '" target="_blank" rel="noopener noreferrer">' + text + '</a>';
    });

    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/__([^_]+)__/g, '<strong>$1</strong>');
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    s = s.replace(/(^|[^_\w])_([^_\n]+)_/g, '$1<em>$2</em>');
    s = s.replace(/~~([^~]+)~~/g, '<del>$1</del>');

    s = s.replace(/\u0000C(\d+)\u0000/g, function (_, i) {
      return '<code>' + codes[+i] + '</code>';
    });
    return s;
  }

  function splitRow(line) {
    return line.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map(function (c) {
      return c.trim();
    });
  }

  function renderMarkdown(src) {
    var lines = String(src).replace(/\r\n?/g, '\n').split('\n');
    var out = [];
    var para = [];
    var currentList = null;
    var i = 0;

    function flushPara() {
      if (para.length) {
        out.push('<p>' + renderInline(escapeHTML(para.join('\n')).replace(/\n/g, '<br>')) + '</p>');
        para.length = 0;
      }
    }
    function closeList() {
      if (currentList) {
        out.push('</' + currentList + '>');
        currentList = null;
      }
    }

    while (i < lines.length) {
      var line = lines[i];

      // 围栏代码块
      var fence = line.match(/^\s*(```|~~~)\s*(\S*)/);
      if (fence) {
        flushPara();
        closeList();
        var marker = fence[1];
        var lang = fence[2] || '';
        var buf = [];
        i++;
        while (i < lines.length && lines[i].indexOf(marker) !== 0 &&
               !new RegExp('^\\s*' + marker).test(lines[i])) {
          buf.push(lines[i]);
          i++;
        }
        i++; // 跳过结束围栏
        out.push('<pre class="md-code"><code' + (lang ? ' data-lang="' + escapeHTML(lang) + '"' : '') +
          '>' + escapeHTML(buf.join('\n')) + '</code></pre>');
        continue;
      }

      // 分隔线
      if (/^\s*(?:[-*_]\s*){3,}$/.test(line)) {
        flushPara();
        closeList();
        out.push('<hr>');
        i++;
        continue;
      }

      // 标题
      var h = line.match(/^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/);
      if (h) {
        flushPara();
        closeList();
        var lvl = h[1].length;
        out.push('<h' + lvl + '>' + renderInline(escapeHTML(h[2])) + '</h' + lvl + '>');
        i++;
        continue;
      }

      // 引用
      if (/^\s*>/.test(line)) {
        flushPara();
        closeList();
        var quote = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) {
          quote.push(lines[i].replace(/^\s*>\s?/, ''));
          i++;
        }
        out.push('<blockquote>' + renderMarkdown(quote.join('\n')) + '</blockquote>');
        continue;
      }

      // 表格：当前行含 |，下一行是分隔行
      if (line.indexOf('|') >= 0 && i + 1 < lines.length &&
          /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[i + 1]) && /-/.test(lines[i + 1])) {
        flushPara();
        closeList();
        var head = splitRow(line);
        i += 2;
        var rows = [];
        while (i < lines.length && lines[i].indexOf('|') >= 0 && lines[i].trim() !== '') {
          rows.push(splitRow(lines[i]));
          i++;
        }
        var t = '<table class="md-table"><thead><tr>';
        head.forEach(function (c) { t += '<th>' + renderInline(escapeHTML(c)) + '</th>'; });
        t += '</tr></thead><tbody>';
        rows.forEach(function (r) {
          t += '<tr>';
          for (var c = 0; c < head.length; c++) {
            t += '<td>' + renderInline(escapeHTML(r[c] || '')) + '</td>';
          }
          t += '</tr>';
        });
        out.push(t + '</tbody></table>');
        continue;
      }

      // 列表
      var ul = line.match(/^\s*[-*+]\s+(.*)$/);
      var ol = line.match(/^\s*\d+[.)]\s+(.*)$/);
      if (ul || ol) {
        flushPara();
        var tag = ul ? 'ul' : 'ol';
        if (currentList !== tag) {
          closeList();
          out.push('<' + tag + '>');
          currentList = tag;
        }
        var text = ul ? ul[1] : ol[1];
        var task = text.match(/^\[([ xX])\]\s+(.*)$/);
        if (task) {
          out.push('<li class="md-task"><span class="md-check">' +
            (task[1].toLowerCase() === 'x' ? '☑' : '☐') + '</span>' +
            renderInline(escapeHTML(task[2])) + '</li>');
        } else {
          out.push('<li>' + renderInline(escapeHTML(text)) + '</li>');
        }
        i++;
        continue;
      }

      // 空行
      if (line.trim() === '') {
        flushPara();
        closeList();
        i++;
        continue;
      }

      para.push(line);
      i++;
    }
    flushPara();
    closeList();
    return out.join('\n');
  }

  // ---------------------------------------------------------------- JSON / XML

  // highlightJSON 给格式化后的 JSON 上色。输入必须是**已转义**的文本。
  function highlightJSON(escaped) {
    return escaped.replace(
      /("(?:\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(?:true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+-]?\d+)?)/g,
      function (m) {
        var cls = 'j-num';
        if (m.charAt(0) === '"') cls = /:\s*$/.test(m) ? 'j-key' : 'j-str';
        else if (/true|false/.test(m)) cls = 'j-bool';
        else if (/null/.test(m)) cls = 'j-null';
        return '<span class="' + cls + '">' + m + '</span>';
      }
    );
  }

  function previewJSON(src) {
    var box = document.createElement('div');
    var text;
    try {
      var parsed = JSON.parse(src);
      text = JSON.stringify(parsed, null, 2);
    } catch (e) {
      box.className = 'ed-error';
      box.textContent = 'JSON 解析失败：' + e.message;
      return box;
    }
    var pre = document.createElement('pre');
    pre.className = 'ed-code';
    pre.innerHTML = highlightJSON(escapeHTML(text));
    box.appendChild(pre);
    return box;
  }

  function previewXML(src) {
    var box = document.createElement('div');
    var doc = new DOMParser().parseFromString(src, 'application/xml');
    var err = doc.querySelector('parsererror');
    if (err) {
      box.className = 'ed-error';
      box.textContent = 'XML 解析失败：' + (err.textContent || '').trim().split('\n')[0];
      return box;
    }
    var note = document.createElement('div');
    note.className = 'ed-note ok';
    note.textContent = 'XML 结构合法（未做缩进重排，保持原文）';
    box.appendChild(note);
    var pre = document.createElement('pre');
    pre.className = 'ed-code';
    pre.textContent = src;
    box.appendChild(pre);
    return box;
  }

  function previewHTML(src) {
    var wrap = document.createElement('div');
    wrap.className = 'ed-html-wrap';
    var frame = document.createElement('iframe');
    // sandbox 留空表示最严格：禁止脚本、禁止表单、禁止同源访问。
    // 这是必须的——否则预览自己上传的 HTML 会让其中的脚本
    // 在本站同源下运行，拿到凭据并调用全部接口。
    frame.setAttribute('sandbox', '');
    frame.className = 'ed-html-frame';
    frame.srcdoc = src;
    wrap.appendChild(frame);
    return wrap;
  }

  function previewMarkdown(src) {
    var box = document.createElement('div');
    box.className = 'ed-markdown';
    box.innerHTML = renderMarkdown(src);
    // 预览里的链接统一在新标签打开，且不允许 opener 反向访问。
    box.querySelectorAll('a').forEach(function (a) {
      a.setAttribute('target', '_blank');
      a.setAttribute('rel', 'noopener noreferrer');
    });
    return box;
  }

  // ---------------------------------------------------------------- 编辑器

  // PREVIEWABLE 决定哪些语言给出预览入口。
  var PREVIEWABLE = {
    markdown: 'Markdown 渲染',
    html: 'HTML 渲染',
    json: 'JSON 格式化',
    xml: 'XML 校验',
    csv: ''
  };

  function languageLabel(lang) {
    return ({
      markdown: 'Markdown', html: 'HTML', json: 'JSON', xml: 'XML', yaml: 'YAML',
      toml: 'TOML', ini: 'INI', plaintext: '纯文本', csv: 'CSV', sql: 'SQL',
      shell: 'Shell', python: 'Python', go: 'Go', javascript: 'JavaScript',
      typescript: 'TypeScript', java: 'Java', c: 'C', cpp: 'C++', rust: 'Rust',
      css: 'CSS', scss: 'SCSS', dockerfile: 'Dockerfile', makefile: 'Makefile',
      ruby: 'Ruby', php: 'PHP', powershell: 'PowerShell', bat: 'Batch',
      properties: 'Properties', dotenv: '环境变量', hcl: 'HCL', kotlin: 'Kotlin',
      swift: 'Swift', csharp: 'C#', perl: 'Perl', lua: 'Lua', r: 'R',
      graphql: 'GraphQL', protobuf: 'Protocol Buffers', diff: 'Diff',
      gitignore: 'Ignore', latex: 'LaTeX', rst: 'reStructuredText'
    })[lang] || (lang ? lang : '纯文本');
  }

  // openEditor 打开编辑器。entry 是目录列表里的条目。
  async function openEditor(entry) {
    if (!entry || entry.is_dir) return;

    // ---- 先把内容拉下来，避免弹出一个空壳再闪烁填充 ----
    var doc;
    try {
      var res = await G.apiFetch(TEXT_EP + '?path=' + encodeURIComponent(entry.path));
      if (!res.ok) {
        throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
      }
      doc = await res.json();
    } catch (e) {
      G.toast('err', '打开失败：' + e.message);
      return;
    }

    var original = doc.content || '';
    // baseHash 作为乐观锁凭据：服务端只保存摘要，比对内容而非时间戳，
    // 这样同一毫秒内的两次保存也不会互相误判。
    var baseHash = doc.hash || '';
    var newline = doc.newline || '\n';
    var dirty = false;
    var readOnly = !!doc.read_only || !G.can('write');
    var previewOn = false;
    var previewMode = null;

    // ---- DOM ----
    var body = document.createElement('div');
    body.className = 'ed-root';

    var bar = document.createElement('div');
    bar.className = 'ed-bar';
    var meta = document.createElement('div');
    meta.className = 'ed-meta';
    var langTag = document.createElement('span');
    langTag.className = 'ed-lang';
    langTag.textContent = languageLabel(doc.language);
    meta.appendChild(langTag);
    var stat = document.createElement('span');
    stat.className = 'ed-stat';
    meta.appendChild(stat);
    bar.appendChild(meta);

    var tools = document.createElement('div');
    tools.className = 'ed-tools';
    bar.appendChild(tools);
    body.appendChild(bar);

    var stage = document.createElement('div');
    stage.className = 'ed-stage';
    var pane = document.createElement('div');
    pane.className = 'ed-pane';
    var gutter = document.createElement('div');
    gutter.className = 'ed-gutter';
    var gutterInner = document.createElement('div');
    gutterInner.className = 'ed-gutter-inner';
    gutter.appendChild(gutterInner);
    var ta = document.createElement('textarea');
    ta.className = 'ed-text';
    ta.spellcheck = false;
    ta.wrap = 'off';
    ta.value = original;
    if (readOnly) ta.readOnly = true;
    pane.appendChild(gutter);
    pane.appendChild(ta);
    stage.appendChild(pane);

    var previewPane = document.createElement('div');
    previewPane.className = 'ed-preview';
    previewPane.hidden = true;
    stage.appendChild(previewPane);
    body.appendChild(stage);

    var foot = document.createElement('div');
    foot.className = 'ed-foot';
    body.appendChild(foot);

    var m = G.openModal({
      title: doc.name || entry.name,
      body: body,
      full: true,
      // 用 beforeClose 钩子统一拦住 ESC、点遮罩和右上角按钮三条关闭路径，
      // 避免其中任何一条绕过未保存确认。
      beforeClose: function () {
        if (!dirty) return true;
        G.confirm({
          title: '放弃未保存的修改？',
          message: doc.name + ' 还有未保存的改动。',
          detail: '点击「放弃修改」将直接关闭，改动不会写入服务器。',
          okLabel: '放弃修改',
          danger: true
        }).then(function (yes) {
          if (yes) {
            dirty = false;
            m.close();
          }
        });
        return false;
      }
    });

    // ---- 统计与行号 ----

    function updateStats() {
      var text = ta.value;
      var lines = text.split('\n').length;
      var chars = text.length;
      var pos = caretPos();
      stat.textContent = lines + ' 行 · ' + chars + ' 字符 · 第 ' + pos.line + ' 行';
      foot.textContent = '换行 ' + (newline === '\r\n' ? 'CRLF' : 'LF') +
        '　·　' + (doc.bom ? '带 BOM' : '无 BOM') +
        '　·　' + G.fmtSize(new Blob([text]).size) +
        (dirty ? '　·　● 有未保存的修改' : '　·　已同步') +
        (readOnly ? '　·　只读' : '');
    }

    function caretPos() {
      var upto = ta.value.slice(0, ta.selectionStart);
      var idx = upto.lastIndexOf('\n');
      return { line: upto.split('\n').length, col: upto.length - idx };
    }

    function renderGutter() {
      var count = ta.value.split('\n').length;
      if (count > MAX_GUTTER_LINES) {
        gutterInner.textContent = '';
        gutter.classList.add('off');
        return;
      }
      gutter.classList.remove('off');
      var out = [];
      for (var i = 1; i <= count; i++) out.push(i);
      gutterInner.textContent = out.join('\n');
    }

    function syncScroll() {
      gutterInner.style.transform = 'translateY(' + (-ta.scrollTop) + 'px)';
    }

    ta.addEventListener('scroll', syncScroll);

    // ---- 预览 ----

    function refreshPreview() {
      if (!previewOn) return;
      previewPane.textContent = '';
      var src = ta.value;
      var node;
      switch (previewMode) {
        case 'markdown': node = previewMarkdown(src); break;
        case 'html': node = previewHTML(src); break;
        case 'json': node = previewJSON(src); break;
        case 'xml': node = previewXML(src); break;
        default: node = document.createElement('div');
      }
      previewPane.appendChild(node);
    }

    function setPreview(on) {
      previewOn = on;
      previewPane.hidden = !on;
      stage.classList.toggle('split', on);
      if (on) refreshPreview();
    }

    // ---- 工具栏 ----

    function makeTool(label, title, onClick, cls) {
      var b = G.btn(label, cls || '', onClick);
      b.title = title || label;
      b.classList.add('btn-sm');
      return b;
    }

    var saveBtn = makeTool('保存', '保存到服务器（Ctrl/⌘ + S）', function () { save(); }, 'primary');
    if (readOnly) {
      saveBtn.disabled = true;
      saveBtn.title = '没有写入权限';
    }
    tools.appendChild(saveBtn);

    previewMode = PREVIEWABLE[doc.language] ? doc.language : null;
    if (previewMode) {
      var pvLabel = { markdown: '预览', html: '预览', json: '格式化', xml: '校验' }[previewMode];
      var pvBtn = makeTool(pvLabel, PREVIEWABLE[previewMode] + '（只读，不影响原文）', function () {
        setPreview(!previewOn);
      });
      tools.appendChild(pvBtn);
    }

    if (doc.language === 'json') {
      tools.appendChild(makeTool('压缩', '压缩为单行 JSON', function () {
        try {
          ta.value = JSON.stringify(JSON.parse(ta.value));
          onEdit();
          G.toast('ok', '已压缩');
        } catch (e) {
          G.toast('err', 'JSON 不合法：' + e.message);
        }
      }));
    }

    tools.appendChild(makeTool('退出', '关闭编辑器', function () { m.close(); }));

    // ---- 编辑行为 ----

    function onEdit() {
      dirty = true;
      renderGutter();
      updateStats();
      refreshPreview();
    }

    ta.addEventListener('input', onEdit);
    ta.addEventListener('keyup', updateStats);
    ta.addEventListener('click', updateStats);

    ta.addEventListener('keydown', function (e) {
      // Ctrl/⌘ + S 保存
      if ((e.metaKey || e.ctrlKey) && (e.key === 's' || e.key === 'S')) {
        e.preventDefault();
        if (!readOnly) save();
        return;
      }
      if (readOnly) return;

      // Tab 插入缩进而不是跳走
      if (e.key === 'Tab') {
        e.preventDefault();
        var start = ta.selectionStart;
        var end = ta.selectionEnd;
        var val = ta.value;
        if (start !== end && val.slice(start, end).indexOf('\n') >= 0) {
          // 多行选区整体缩进 / 反缩进
          var lineStart = val.lastIndexOf('\n', start - 1) + 1;
          var block = val.slice(lineStart, end);
          var shifted = e.shiftKey
            ? block.replace(/^ {1,2}/gm, '')
            : block.replace(/^/gm, '  ');
          ta.value = val.slice(0, lineStart) + shifted + val.slice(end);
          ta.selectionStart = lineStart;
          ta.selectionEnd = lineStart + shifted.length;
        } else if (e.shiftKey) {
          // 单光标反缩进
          var ls = val.lastIndexOf('\n', start - 1) + 1;
          if (val.slice(ls, ls + 2) === '  ') {
            ta.value = val.slice(0, ls) + val.slice(ls + 2);
            ta.setSelectionRange(start - 2, start - 2);
          }
        } else {
          ta.setRangeText('  ', start, end, 'end');
        }
        onEdit();
        return;
      }

      // 回车自动延续缩进
      if (e.key === 'Enter') {
        var upto = ta.value.slice(0, ta.selectionStart);
        var ls = upto.lastIndexOf('\n') + 1;
        var indent = (upto.slice(ls).match(/^[ \t]*/) || [''])[0];
        var before = upto.replace(/^.*$/s, '');
        if (indent) {
          e.preventDefault();
          var insert = '\n' + indent;
          // 行尾是 { [ ( 时再多缩进一级
          if (/[{[(:]\s*$/.test(ta.value.slice(ls, ta.selectionStart))) {
            insert += '  ';
          }
          ta.setRangeText(insert, ta.selectionStart, ta.selectionEnd, 'end');
          onEdit();
        }
      }
    });

    // ---- 保存 ----

    var saving = false;
    async function save() {
      if (saving || readOnly) return;
      if (!dirty && !doc.created) {
        G.toast('info', '没有需要保存的修改');
        return;
      }
      saving = true;
      saveBtn.disabled = true;
      saveBtn.textContent = '保存中…';

      var payload = {
        path: doc.path,
        content: ta.value,
        base_hash: baseHash,
        newline: newline
      };

      async function put(force) {
        payload.force = !!force;
        return G.apiFetch(TEXT_EP, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
      }

      try {
        var res = await put(false);

        if (res.status === 409) {
          var info = {};
          try { info = await res.json(); } catch (e) { info = {}; }
          var keep = await G.confirm({
            title: '文件已被外部修改',
            message: '这个文件在你编辑期间被其他人（或其它程序）改动过。',
            detail: '服务器上的版本：' + (info.current_mtime || '未知') +
              '，' + G.fmtSize(info.current_size || 0) +
              '\n点击「强制覆盖」会丢弃对方的改动，点击「取消」则保留对方版本。',
            okLabel: '强制覆盖',
            danger: true
          });
          if (!keep) {
            G.toast('warn', '已取消保存，建议关闭后重新打开文件');
            return;
          }
          res = await put(true);
        }

        if (!res.ok) {
          throw new Error((await res.text()).trim() || ('HTTP ' + res.status));
        }

        var out = await res.json();
        baseHash = out.hash || baseHash;
        dirty = false;
        updateStats();
        G.toast('ok', '已保存 ' + doc.name);
        G.refresh();
      } catch (e) {
        G.toast('err', '保存失败：' + e.message);
      } finally {
        saving = false;
        saveBtn.disabled = false;
        saveBtn.textContent = '保存';
      }
    }

    // ---- 初始化 ----
    renderGutter();
    updateStats();
    setPreview(false);
    ta.focus();
    ta.setSelectionRange(0, 0);
  }

  G.openEditor = openEditor;
  G.renderMarkdown = renderMarkdown;
})();
