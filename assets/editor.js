/* gofs 在线文本编辑器。
 *
 * 支持 txt / markdown / html / json / xml / yaml 以及各类源码文件的读取与保存，
 * 并提供 markdown（自研渲染）、html（沙箱 iframe）、json / xml 预览。
 * JSON / XML / YAML / TOML / INI / conf / .config / .env 还能直接格式化源码；
 * 全部用浏览器内建能力或手写的小解析器实现，不引入任何第三方库。 */
(function () {
  'use strict';

  var G = window.GOFS;
  var TEXT_EP = G.raw('/fs/text');

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

  // ---------------------------------------------------------------- 格式化

  // 这些格式化器不引入任何第三方库：JSON/XML 用浏览器内建能力，
  // YAML/TOML/INI/dotenv 是保守的行级整理；复杂结构宁可报错也不改坏内容。

  function formatJSONText(src) {
    try {
      return JSON.stringify(JSON.parse(src), null, 2);
    } catch (e) {
      throw new Error('JSON 不合法：' + e.message);
    }
  }

  function xmlEscapeText(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  function xmlEscapeAttr(s) {
    return xmlEscapeText(s).replace(/"/g, '&quot;');
  }

  function xmlSerializeNode(node, depth) {
    var pad = new Array(depth + 1).join('  ');
    var type = node.nodeType;
    if (type === 1) {
      // xml:space="preserve" 的子树不能靠加空白来排版。
      if (node.getAttribute && node.getAttribute('xml:space') === 'preserve') {
        return pad + new XMLSerializer().serializeToString(node);
      }
      var attrs = '';
      for (var i = 0; i < node.attributes.length; i++) {
        var a = node.attributes[i];
        attrs += ' ' + a.name + '="' + xmlEscapeAttr(a.value) + '"';
      }
      var kids = [];
      for (var k = 0; k < node.childNodes.length; k++) {
        var ch = node.childNodes[k];
        if (ch.nodeType === 3 && !ch.nodeValue.trim()) continue;
        kids.push(ch);
      }
      if (!kids.length) return pad + '<' + node.nodeName + attrs + '/>';
      var hasText = false;
      for (var t = 0; t < kids.length; t++) {
        if (kids[t].nodeType === 3 || kids[t].nodeType === 4) { hasText = true; break; }
      }
      // 混合内容（文本 + 子元素）保留原始内部形态，避免额外空白改变语义。
      if (hasText) return pad + new XMLSerializer().serializeToString(node);
      var lines = [pad + '<' + node.nodeName + attrs + '>'];
      kids.forEach(function (child) { lines.push(xmlSerializeNode(child, depth + 1)); });
      lines.push(pad + '</' + node.nodeName + '>');
      return lines.join('\n');
    }
    if (type === 3) return pad + xmlEscapeText(node.nodeValue);
    if (type === 4) return pad + '<![CDATA[' + node.nodeValue + ']]>';
    if (type === 7) return pad + '<?' + node.target + (node.data ? ' ' + node.data : '') + '?>';
    if (type === 8) return pad + '<!--' + node.nodeValue + '-->';
    return pad + new XMLSerializer().serializeToString(node);
  }

  function formatXMLText(src) {
    if (typeof DOMParser === 'undefined' || typeof XMLSerializer === 'undefined') {
      throw new Error('浏览器不支持 XML 格式化');
    }
    var doc = new DOMParser().parseFromString(src, 'application/xml');
    var err = doc.getElementsByTagName('parsererror')[0];
    if (err) {
      var msg = String(err.textContent || '').replace(/\s+/g, ' ').trim();
      throw new Error('XML 不合法' + (msg ? '：' + msg.slice(0, 120) : ''));
    }
    var lines = [];
    // 原文没有 XML 声明就不要凭空加一条，只格式化已有声明。
    if (/^\s*<\?xml\b/.test(src)) {
      var decl = '<?xml version="' + (doc.xmlVersion || '1.0') + '"';
      if (doc.xmlEncoding) decl += ' encoding="' + doc.xmlEncoding + '"';
      if (doc.xmlStandalone) decl += ' standalone="yes"';
      lines.push(decl + '?>');
    }
    if (doc.doctype) {
      try { lines.push(new XMLSerializer().serializeToString(doc.doctype)); } catch (e) { /* 忽略 */ }
    }
    for (var i = 0; i < doc.childNodes.length; i++) {
      var ch = doc.childNodes[i];
      if (ch.nodeType === 1 || ch.nodeType === 8 ||
          (ch.nodeType === 7 && ch.target !== 'xml')) {
        lines.push(xmlSerializeNode(ch, 0));
      }
    }
    return finishFormatted(lines, src);
  }

  // findUnquoted 找分隔符，跳过单/双引号里的内容（TOML/INI 的 key 里可能有 =）。
  function findUnquoted(s, needle) {
    var quote = null;
    for (var i = 0; i < s.length; i++) {
      var ch = s.charAt(i);
      if (quote) {
        if (quote === '"' && ch === '\\') { i++; continue; }
        if (ch === quote) quote = null;
      } else if (ch === '"' || ch === "'") {
        quote = ch;
      } else if (ch === needle) {
        return i;
      }
    }
    return -1;
  }

  function countOccurrences(s, sub) {
    var n = 0, at = 0;
    while ((at = s.indexOf(sub, at)) !== -1) { n++; at += sub.length; }
    return n;
  }

  function finishFormatted(lines, src) {
    while (lines.length && !String(lines[lines.length - 1]).trim()) lines.pop();
    var text = lines.join('\n');
    if (/\n$/.test(src) && text) text += '\n';
    return text;
  }

  // YAML：只整理「结构清晰」的块式配置。块标量、跨行引号、多行标量
  // 或 Tab 缩进一律拒绝，避免把内容改坏。
  function yamlBlockHeader(line) {
    var t = line.replace(/\s+$/, '').trim();
    if (!t || t.charAt(0) === '#') return false;
    // key: |  /  - >-  /  key: |2+  /  key: | # 注释
    return /[:\-]\s*[|>][0-9+\-]*\s*(#.*)?$/.test(t);
  }

  function yamlQuotesBalanced(lines, inBlock) {
    var quote = null;
    for (var i = 0; i < lines.length; i++) {
      if (inBlock[i]) continue;
      var line = lines[i];
      for (var j = 0; j < line.length; j++) {
        var ch = line.charAt(j);
        if (quote) {
          if (quote === '"' && ch === '\\') { j++; continue; }
          if (ch === quote) quote = null;
        } else {
          if (ch === '#') break;
          if (ch === '"' || ch === "'") quote = ch;
        }
      }
      if (quote) return false;
    }
    return true;
  }

  function formatYAMLText(src) {
    if (!src.trim()) return '';
    var lines = src.replace(/\r\n?/g, '\n').split('\n');
    var inBlock = new Array(lines.length);
    var blockHeaderIndent = -1;

    // 先标出块标量的内容区：块标量里的行不参与结构校验、缩进重排，
    // 后面只整体平移，保持 | / > 里的空白语义。
    for (var i = 0; i < lines.length; i++) {
      var raw0 = lines[i];
      var t0 = raw0.replace(/\s+$/, '').trim();
      var indent0 = raw0.length - raw0.replace(/^ +/, '').length;
      if (blockHeaderIndent >= 0) {
        if (!t0 || indent0 > blockHeaderIndent) {
          inBlock[i] = true;
          continue;
        }
        blockHeaderIndent = -1;
      }
      if (yamlBlockHeader(raw0)) blockHeaderIndent = indent0;
    }

    for (i = 0; i < lines.length; i++) {
      if (inBlock[i]) continue;
      var line = lines[i];
      if (/^[ ]*\t/.test(line)) {
        throw new Error('YAML 使用 Tab 缩进，未自动格式化');
      }
      var t = line.replace(/\s+$/, '').trim();
      if (!t) continue;
      if (t.charAt(0) !== '#' && t !== '---' && t !== '...' &&
          !/^-(\s|$)/.test(t) && t.indexOf(':') === -1 &&
          ']}[{'.indexOf(t.charAt(0)) === -1) {
        throw new Error('YAML 含多行标量或复杂结构，未自动格式化');
      }
    }
    if (!yamlQuotesBalanced(lines, inBlock)) {
      throw new Error('YAML 含跨行引号，未自动格式化');
    }

    var stack = [0];
    var prevIndent = 0;
    var out = [];
    var block = null;
    for (i = 0; i < lines.length; i++) {
      var raw = lines[i];
      if (inBlock[i]) {
        if (!raw) { out.push(''); continue; }
        var blockIndent = raw.length - raw.replace(/^ +/, '').length;
        if (block && blockIndent > block.headerIndent) {
          // 相对块头的缩进保持不变；块内行尾空格也原样保留。
          var lead = block.newIndent + (blockIndent - block.headerIndent);
          out.push(new Array(lead + 1).join(' ') + raw.slice(blockIndent));
        } else {
          out.push('');
        }
        continue;
      }
      block = null;
      var content = raw.replace(/^ +/, '');
      if (!content) { out.push(''); continue; }
      var indent = raw.length - content.length;
      if (indent > prevIndent) {
        stack.push(indent);
      } else if (indent < prevIndent) {
        while (stack.length > 1 && stack[stack.length - 1] > indent) stack.pop();
        if (indent > stack[stack.length - 1]) stack.push(indent);
      }
      var newIndent = (stack.length - 1) * 2;
      var isHeader = yamlBlockHeader(raw);
      // 块头行尾的空格/注释没有语义，其余行原样保留内容。
      var text = isHeader ? content.replace(/\s+$/, '') : content;
      out.push(new Array(newIndent + 1).join(' ') + text);
      if (isHeader) {
        block = { headerIndent: indent, newIndent: newIndent };
      }
      prevIndent = indent;
    }
    // 不用 finishFormatted：它会吃掉尾部空行，而 |+ / >+ 里这些空行是内容。
    return out.join('\n');
  }

  function formatTOMLText(src) {
    var lines = src.replace(/\r\n?/g, '\n').split('\n');
    var out = [];
    var multi = null;
    for (var i = 0; i < lines.length; i++) {
      var raw = lines[i];
      if (multi) {
        out.push(raw);
        if (raw.indexOf(multi) !== -1) multi = null;
        continue;
      }
      var delim = raw.indexOf('"""') !== -1 ? '"""'
        : (raw.indexOf("'''") !== -1 ? "'''" : null);
      if (delim) {
        out.push(raw);
        if (countOccurrences(raw, delim) % 2 === 1) multi = delim;
        continue;
      }
      var line = raw.replace(/\s+$/, '');
      var t = line.trim();
      if (!t) { out.push(''); continue; }
      if (t.charAt(0) === '#' || /^\[\[?.*\]\]?$/.test(t)) {
        out.push(t);
        continue;
      }
      var eq = findUnquoted(line, '=');
      if (eq < 0) {
        out.push(line);
        continue;
      }
      out.push(line.slice(0, eq).trim() + ' = ' + line.slice(eq + 1).trim());
    }
    return finishFormatted(out, src);
  }

  function formatIniText(src) {
    var lines = src.replace(/\r\n?/g, '\n').split('\n');
    var out = [];
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].replace(/\s+$/, '');
      var t = line.trim();
      if (!t) { out.push(''); continue; }
      if (/^[;#]/.test(t) || /^\[.*\]$/.test(t)) {
        out.push(t);
        continue;
      }
      // 缩进行视为续行，保留原样，避免改坏多行值。
      if (/^\s/.test(line)) { out.push(line); continue; }
      var eq = findUnquoted(line, '=');
      if (eq < 0) { out.push(line); continue; }
      out.push(line.slice(0, eq).trim() + ' = ' + line.slice(eq + 1).trim());
    }
    return finishFormatted(out, src);
  }

  function formatDotenvText(src) {
    var lines = src.replace(/\r\n?/g, '\n').split('\n');
    var out = [];
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].replace(/\s+$/, '');
      var t = line.trim();
      if (!t) { out.push(''); continue; }
      if (t.charAt(0) === '#') { out.push(t); continue; }
      var prefix = '';
      if (/^export\s+/.test(t)) {
        prefix = 'export ';
        t = t.replace(/^export\s+/, '');
      }
      var eq = findUnquoted(t, '=');
      if (eq < 0) { out.push(line); continue; }
      out.push(prefix + t.slice(0, eq).trim() + '=' + t.slice(eq + 1).trim());
    }
    return finishFormatted(out, src);
  }

  var FORMATTERS = {
    json: { name: 'JSON', run: formatJSONText },
    xml: { name: 'XML', run: formatXMLText },
    yaml: { name: 'YAML', run: formatYAMLText },
    toml: { name: 'TOML', run: formatTOMLText },
    ini: { name: 'INI / conf', run: formatIniText },
    dotenv: { name: '环境变量', run: formatDotenvText }
  };

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
    // 点预览区也能拿到焦点，方向键 / PageDown 才能滚动长文档。
    // -1 表示不进 Tab 序列，弹窗的焦点陷阱不会因此多出一站。
    previewPane.tabIndex = -1;
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

    // 标题旁的未保存圆点：保存按钮在右上角，视线离开编辑区后
    // 也能一眼看出这份文件还没落盘。
    var dirtyDot = null;
    if (!readOnly) {
      var headTitle = m.el.querySelector('.modal-head > div');
      if (headTitle) {
        dirtyDot = document.createElement('span');
        dirtyDot.className = 'ed-dirty-dot';
        dirtyDot.textContent = '●';
        dirtyDot.title = '有未保存的修改';
        dirtyDot.setAttribute('aria-label', '有未保存的修改');
        dirtyDot.hidden = true;
        headTitle.appendChild(dirtyDot);
      }
    }

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
        ((dirty || doc.created) ? '　·　● 有未保存的修改' : '　·　已同步') +
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
      // 没有预览就谈不上「仅预览」：关掉预览时顺手退回编辑态。
      if (!on) setPreviewOnly(false);
      previewPane.hidden = !on;
      stage.classList.toggle('split', on);
      // 「仅预览」只在「全屏 + 预览开着」时有意义，别留一颗点不动的按钮。
      if (fsOnlyBtn) fsOnlyBtn.hidden = !(on && fullscreen);
      if (pvBtn) {
        // 按钮本身要能看出「预览开着」。否则用户只能从多出来的半边
        // 内容推断状态，再点一次也不知道会发生什么。
        pvBtn.classList.toggle('active', on);
        pvBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
        pvBtn.textContent = on ? '关闭预览' : pvLabel;
        pvBtn.title = on
          ? '关闭预览（不影响原文）'
          : PREVIEWABLE[previewMode] + '（只读，不影响原文）';
      }
      if (on) refreshPreview();
    }

    // ---- 全屏编辑与预览 ----
    //
    // 用 CSS 把**弹窗本身**撑到整个窗口，而不是浏览器原生 Fullscreen：
    // 原生全屏会把弹窗标题栏和这颗「退出全屏」按钮一起藏起来，
    // 用户只能靠猜 Esc；而且 `srcdoc` 沙箱 iframe 在部分浏览器里
    // 拿不到全屏权限。现在标题栏、工具栏都还在，退出入口一直可见。
    //
    // 全屏默认是编辑态：左边编辑区保留，右边预览随手改随手变
    // （预览刷新本来就跟在 input 事件后面）。需要安静读长文时，
    // 再切到「仅预览」把编辑区收起，预览占满整行。
    var fullscreen = false;
    var previewOnly = false;

    // setPreviewOnly 只负责切「收起编辑区」这一个状态，
    // 焦点由调用方决定：点按钮进来 / 切回编辑区落点不一样。
    function setPreviewOnly(on) {
      on = !!on && fullscreen && previewOn;
      if (on === previewOnly) return;
      previewOnly = on;
      body.classList.toggle('preview-only', on);
      if (fsOnlyBtn) {
        fsOnlyBtn.textContent = on ? '显示编辑' : '仅预览';
        fsOnlyBtn.title = on
          ? '显示编辑区，边改边看'
          : '隐藏编辑区，只留预览（Esc 退出全屏）';
        fsOnlyBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
      }
    }

    function setFullscreen(on) {
      if (on === fullscreen) return;
      // 还没开预览就点全屏：直接替用户打开，一步到位。
      if (on && !previewOn) setPreview(true);
      fullscreen = on;
      var mask = m.el.parentNode;
      body.classList.toggle('preview-fs', on);
      m.el.classList.toggle('preview-fs', on);
      if (mask) mask.classList.toggle('preview-fs', on);
      if (fsBtn) {
        fsBtn.textContent = on ? '退出全屏' : '全屏';
        fsBtn.classList.toggle('active', on);
        fsBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
        fsBtn.title = on ? '退出全屏（Esc）' : '全屏编辑与预览（Esc 退出）';
      }
      // 「仅预览」只在这时候有意义：每次进全屏都从边改边看开始。
      setPreviewOnly(false);
      if (fsOnlyBtn) fsOnlyBtn.hidden = !on;
      if (on) {
        // 桌面端直接把焦点放进编辑区，进来就能改。触屏不抢焦点，
        // 否则一进全屏就弹出软键盘，把预览挤得只剩一条。
        if (!window.matchMedia || window.matchMedia('(hover: hover)').matches) ta.focus();
      } else if (fsBtn) {
        fsBtn.focus();
      }
    }

    m.el.addEventListener('keydown', function (e) {
      // Esc 只退出全屏、不关弹窗。弹窗自己的 Esc 监听挂在 document 上（冒泡），
      // 这里在弹窗元素上先接住并 stopPropagation，事件就到不了它。
      if (e.key === 'Escape' && fullscreen) {
        e.preventDefault();
        e.stopPropagation();
        setFullscreen(false);
        return;
      }
      // 预览区是脚本聚焦的（tabIndex = -1），不在弹窗焦点循环的候选里。
      // 不接管的话，从它按 Tab 会直接跳出弹窗、跑到浏览器地址栏。
      if (e.key === 'Tab' && fullscreen && document.activeElement === previewPane) {
        e.preventDefault();
        var first = null;
        Array.prototype.some.call(tools.querySelectorAll('button'), function (b) {
          if (!b.disabled && !b.hidden) { first = b; return true; }
          return false;
        });
        (first || m.el).focus();
      }
    });

    // ---- 工具栏 ----

    function makeTool(label, title, onClick, cls) {
      var b = G.btn(label, cls || '', onClick);
      b.title = title || label;
      b.classList.add('btn-sm');
      return b;
    }

    var saveBtn = makeTool('保存', '保存到服务器（Ctrl/⌘ + S）', function () { save(); }, 'primary');
    var pvBtn = null;
    var fsBtn = null;
    var fsOnlyBtn = null;
    var pvLabel = '';
    if (readOnly) {
      saveBtn.disabled = true;
      saveBtn.title = '没有写入权限';
    }
    tools.appendChild(saveBtn);

    // syncSaveState 让「保存」成为真正的状态按钮：
    // clean 时禁用并显示「已保存」，有改动时才恢复主色与可点状态。
    // 之前它永远是蓝色可点的，点下去只弹一句「没有需要保存的修改」。
    function syncSaveState() {
      if (readOnly) {
        saveBtn.disabled = true;
        saveBtn.textContent = '保存';
        saveBtn.title = '没有写入权限';
        if (dirtyDot) dirtyDot.hidden = true;
        return;
      }
      if (saving) return;          // 保存中由 save() 自己改文案
      var clean = !dirty && !doc.created;
      saveBtn.disabled = clean;
      saveBtn.classList.toggle('primary', !clean);
      saveBtn.textContent = clean ? '已保存' : '保存';
      saveBtn.title = clean ? '没有未保存的修改'
        : '保存到服务器（Ctrl/⌘ + S）';
      if (dirtyDot) dirtyDot.hidden = clean;
    }

    previewMode = PREVIEWABLE[doc.language] ? doc.language : null;
    if (previewMode) {
      pvLabel = { markdown: '预览', html: '预览', json: '预览', xml: '校验' }[previewMode];
      pvBtn = makeTool(pvLabel, PREVIEWABLE[previewMode] + '（只读，不影响原文）', function () {
        setPreview(!previewOn);
      });
      pvBtn.setAttribute('aria-pressed', 'false');
      tools.appendChild(pvBtn);

      // 全屏按钮只在有预览的语言里出现（Markdown / HTML / JSON / XML）。
      // 没开预览时点它，会先把预览打开再进全屏。
      fsBtn = makeTool('全屏', '全屏编辑与预览（Esc 退出）', function () {
        setFullscreen(!fullscreen);
      });
      fsBtn.setAttribute('aria-pressed', 'false');
      tools.appendChild(fsBtn);

      // 「仅预览」只出现在全屏期间：把编辑区收起来，长文读起来更安静。
      // 放在这里而不是另外开一颗入口按钮，是为了全屏只有一个进入点。
      fsOnlyBtn = makeTool('仅预览', '隐藏编辑区，只留预览（Esc 退出全屏）', function () {
        setPreviewOnly(!previewOnly);
        if (previewOnly) {
          previewPane.focus();
        } else {
          ta.focus();
        }
      });
      fsOnlyBtn.setAttribute('aria-pressed', 'false');
      fsOnlyBtn.hidden = true;
      tools.appendChild(fsOnlyBtn);
    }

    // 格式化会直接改写 textarea 内容；只读文件不显示入口。
    var fmt = FORMATTERS[doc.language];
    if (fmt && !readOnly) {
      tools.appendChild(makeTool('格式化', '格式化 ' + fmt.name + '（会修改原文）', function () {
        try {
          var out = fmt.run(ta.value);
          if (out === ta.value) {
            G.toast('info', '格式没有变化');
            return;
          }
          ta.value = out;
          onEdit();
          ta.focus();
          G.toast('ok', '已格式化 ' + fmt.name);
        } catch (e) {
          G.toast('err', '格式化失败：' + e.message);
        }
      }));
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
      // 改回原文时 dirty 应当消失，而不是一旦动过就永远亮着。
      dirty = ta.value !== original || !!doc.created;
      renderGutter();
      updateStats();
      refreshPreview();
      syncSaveState();
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

      // 先拍下要保存的内容：保存过程中用户仍可能继续编辑，
      // 成功后不能拿「那时的最新内容」当已保存基准，否则这段新改动会被误判为已同步。
      var savedContent = ta.value;
      var payload = {
        path: doc.path,
        content: savedContent,
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
        // 以「实际发出去的内容」为新基准。保存期间如果又敲了字，
        // dirty 仍然是 true，不能被这次成功回包抹掉。
        original = savedContent;
        doc.created = false;
        dirty = ta.value !== original;
        updateStats();
        syncSaveState();
        G.toast('ok', '已保存 ' + doc.name);
        G.refresh();
      } catch (e) {
        G.toast('err', '保存失败：' + e.message);
      } finally {
        saving = false;
        syncSaveState();
      }
    }

    // ---- 初始化 ----
    renderGutter();
    updateStats();
    setPreview(false);
    syncSaveState();
    ta.focus();
    ta.setSelectionRange(0, 0);
  }

  G.openEditor = openEditor;
  G.renderMarkdown = renderMarkdown;
})();
