/* 轻量 Markdown 渲染器。
 *
 * 为什么不引 marked/markdown-it：整个前端要 embed 进单二进制，
 * 多一个 npm 依赖就多一条构建链。这里只实现 agent 输出真正用到的子集：
 * 标题、段落、列表、代码块、行内代码、粗斜体、删除线、链接、引用、表格、分隔线。
 *
 * 安全：所有文本先 escape，再拼标签；不接受原始 HTML（模型输出里的 <script> 只会变成文本）。
 */
(function () {
  'use strict';

  function escapeHtml(s) {
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // 行内元素。先摘出行内代码，避免其中的 * _ [ ] 被误当语法。
  function inline(text) {
    const codes = [];
    let s = String(text).replace(/`([^`]+)`/g, (_, c) => {
      codes.push(c);
      return '\u0000' + (codes.length - 1) + '\u0000';
    });
    s = escapeHtml(s);
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    s = s.replace(/~~([^~]+)~~/g, '<del>$1</del>');
    s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    s = s.replace(/\u0000(\d+)\u0000/g, (_, i) => '<code>' + escapeHtml(codes[Number(i)]) + '</code>');
    return s;
  }

  function isTableSep(line) {
    return /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/.test(line);
  }

  function splitRow(line) {
    return line
      .trim()
      .replace(/^\|/, '')
      .replace(/\|$/, '')
      .split('|')
      .map((c) => c.trim());
  }

  function render(src) {
    const lines = String(src || '').replace(/\r\n/g, '\n').split('\n');
    const out = [];
    let i = 0;

    while (i < lines.length) {
      const line = lines[i];

      // 代码块
      const fence = line.match(/^\s*```(\S*)\s*$/);
      if (fence) {
        const lang = fence[1] || '';
        const body = [];
        i++;
        while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) {
          body.push(lines[i]);
          i++;
        }
        i++; // 跳过结束围栏
        const code = body.join('\n');
        out.push(
          '<div class="codeBlock"><div class="codeHead"><span>' +
            escapeHtml(lang || 'text') +
            '</span><button class="copyBtn" data-copy="1">复制</button></div><pre>' +
            escapeHtml(code) +
            '</pre></div>'
        );
        continue;
      }

      // 空行
      if (/^\s*$/.test(line)) {
        i++;
        continue;
      }

      // 分隔线
      if (/^\s*([-*_])\s*(\1\s*){2,}$/.test(line)) {
        out.push('<hr>');
        i++;
        continue;
      }

      // 标题
      const h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) {
        const level = Math.min(h[1].length, 4);
        out.push('<h' + level + '>' + inline(h[2]) + '</h' + level + '>');
        i++;
        continue;
      }

      // 表格
      if (/\|/.test(line) && i + 1 < lines.length && isTableSep(lines[i + 1])) {
        const head = splitRow(line);
        i += 2;
        const rows = [];
        while (i < lines.length && /\|/.test(lines[i]) && !/^\s*$/.test(lines[i])) {
          rows.push(splitRow(lines[i]));
          i++;
        }
        let t = '<table><thead><tr>';
        head.forEach((c) => (t += '<th>' + inline(c) + '</th>'));
        t += '</tr></thead><tbody>';
        rows.forEach((r) => {
          t += '<tr>';
          for (let k = 0; k < head.length; k++) t += '<td>' + inline(r[k] || '') + '</td>';
          t += '</tr>';
        });
        t += '</tbody></table>';
        out.push(t);
        continue;
      }

      // 引用
      if (/^\s*>/.test(line)) {
        const body = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) {
          body.push(lines[i].replace(/^\s*>\s?/, ''));
          i++;
        }
        out.push('<blockquote>' + inline(body.join('\n')).replace(/\n/g, '<br>') + '</blockquote>');
        continue;
      }

      // 列表（含嵌套：子项以缩进 ≥2 空格开头）
      const listM = line.match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
      if (listM) {
        const ordered = /\d/.test(listM[2]);
        const baseIndent = listM[1].length;
        const items = [];
        while (i < lines.length) {
          const m = lines[i].match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
          if (!m) break;
          const indent = m[1].length;
          if (indent < baseIndent) break;
          if (indent >= baseIndent + 2) {
            // 嵌套项：并进上一项
            if (items.length) items[items.length - 1].sub.push(m[3]);
          } else {
            items.push({ text: m[3], sub: [] });
          }
          i++;
        }
        const tag = ordered ? 'ol' : 'ul';
        let t = '<' + tag + '>';
        items.forEach((it) => {
          t += '<li>' + inline(it.text);
          if (it.sub.length) {
            t += '<' + tag + '>' + it.sub.map((s) => '<li>' + inline(s) + '</li>').join('') + '</' + tag + '>';
          }
          t += '</li>';
        });
        t += '</' + tag + '>';
        out.push(t);
        continue;
      }

      // 段落：合并连续非空行
      const para = [line];
      i++;
      while (
        i < lines.length &&
        !/^\s*$/.test(lines[i]) &&
        !/^\s*```/.test(lines[i]) &&
        !/^#{1,6}\s/.test(lines[i]) &&
        !/^\s*>/.test(lines[i]) &&
        !/^(\s*)([-*+]|\d+[.)])\s+/.test(lines[i]) &&
        !/^\s*([-*_])\s*(\1\s*){2,}$/.test(lines[i]) &&
        !(/\|/.test(lines[i]) && i + 1 < lines.length && isTableSep(lines[i + 1]))
      ) {
        para.push(lines[i]);
        i++;
      }
      out.push('<p>' + inline(para.join('\n')).replace(/\n/g, '<br>') + '</p>');
    }

    return out.join('\n');
  }

  window.renderMarkdown = render;
  window.escapeHtml = escapeHtml;
})();
