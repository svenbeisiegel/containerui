// Text editor for a file in a volume (volume_edit.html), with Ace. The file
// is read as bytes and saved in the encoding it came in: UTF-8, or UTF-16
// when it began with the mark that says so, with a byte order mark where it
// had one, and with the line endings it had (Ace's newLineMode "auto").
// What is not valid text, or larger than the server sends, is shown read
// only. Saving sends the mtime the file was read with; the server refuses
// it when the file changed since, and the editor asks before overwriting.
(function () {
  var box = document.querySelector("[data-file-editor]");
  if (!box) return;
  var saveBtn = document.querySelector("[data-edit-save]");
  var dirtyDot = document.querySelector("[data-edit-dirty]");
  var noticeBox = document.querySelector("[data-edit-notice]");
  var errorBox = document.querySelector("[data-edit-error]");
  var closeLink = document.querySelector("[data-edit-close]");
  var field = function (name) { return document.querySelector("[data-edit-" + name + "]"); };

  // The modes shipped in /static/vendor/ace; anything else is plain text.
  var modes = ["apache_conf", "batchfile", "c_cpp", "css", "csv", "diff", "dockerfile", "gitignore", "golang", "html", "ini", "java",
    "javascript", "json", "json5", "lua", "makefile", "markdown", "nginx", "perl", "plain_text", "powershell", "properties", "python",
    "ruby", "sh", "sql", "text", "toml", "tsv", "typescript", "xml", "yaml"];

  var editor = null;
  var editable = false;
  var decoded = null;
  var mtime = "";
  var saving = false;

  function notice(msg) {
    noticeBox.textContent = msg;
    noticeBox.hidden = !msg;
  }
  function fail(msg) {
    errorBox.textContent = msg;
    errorBox.hidden = !msg;
  }
  function humanSize(n) {
    n = Number(n);
    if (n < 1024) return n + " B";
    var units = "KMGTPE", i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + " " + units[i] + "iB";
  }
  function setMeta(owner, mode, size) {
    if (owner) field("owner").textContent = owner;
    if (mode) field("mode").textContent = mode;
    if (size !== null && size !== undefined && size !== "") field("size").textContent = humanSize(size);
  }

  // decode reads UTF-8 unless the file says it is UTF-16, and remembers
  // which it was and whether it began with a byte order mark. strict
  // refuses what is not valid in the encoding (and NUL bytes, which text
  // does not have), which would otherwise be saved back replaced.
  function decode(bytes, strict) {
    var head = new Uint8Array(bytes, 0, Math.min(3, bytes.byteLength));
    var encoding = "utf-8", bom = false;
    if (head[0] === 0xff && head[1] === 0xfe) {
      encoding = "utf-16le"; bom = true;
    } else if (head[0] === 0xfe && head[1] === 0xff) {
      encoding = "utf-16be"; bom = true;
    } else if (head[0] === 0xef && head[1] === 0xbb && head[2] === 0xbf) {
      bom = true;
    }
    if (strict && encoding === "utf-8" && new Uint8Array(bytes).indexOf(0) >= 0) return null;
    try {
      // The decoder drops the byte order mark itself.
      return { text: new TextDecoder(encoding, { fatal: strict }).decode(bytes), encoding: encoding, bom: bom };
    } catch (e) {
      return null;
    }
  }

  // encode is the text as bytes in the encoding it was read in.
  function encode(text, encoding, bom) {
    if (encoding === "utf-8") {
      var body = new TextEncoder().encode(text);
      if (!bom) return body;
      var out = new Uint8Array(body.length + 3);
      out.set([0xef, 0xbb, 0xbf]);
      out.set(body, 3);
      return out;
    }
    var little = encoding === "utf-16le";
    var buf = new Uint8Array(2 + text.length * 2);
    var view = new DataView(buf.buffer);
    view.setUint16(0, 0xfeff, little);
    for (var i = 0; i < text.length; i++) view.setUint16(2 + 2 * i, text.charCodeAt(i), little);
    return buf;
  }

  function dirty() {
    return editable && editor && !editor.session.getUndoManager().isClean();
  }
  function changed() {
    var d = dirty();
    dirtyDot.hidden = !d;
    saveBtn.disabled = !d || saving;
  }

  function isDark() {
    var t = document.documentElement.getAttribute("data-theme");
    if (t === "dark") return true;
    if (t === "light") return false;
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  }
  function applyTheme() {
    if (editor) editor.setTheme(isDark() ? "ace/theme/tomorrow_night" : "ace/theme/tomorrow");
  }

  function save(force) {
    if (!dirty() || saving) return;
    saving = true;
    changed();
    fail("");
    var undo = editor.session.getUndoManager();
    // What is typed while the save runs stays unsaved: mark clean the
    // revision that was sent.
    var revision = undo.startNewGroup();
    var bytes = encode(editor.session.getValue(), decoded.encoding, decoded.bom);
    var headers = { "Content-Type": "application/octet-stream", "X-CSRF-Token": box.getAttribute("data-csrf") };
    if (!force && mtime) headers["X-Expect-Mtime"] = mtime;
    fetch(box.getAttribute("data-save"), { method: "POST", headers: headers, body: bytes, credentials: "same-origin" })
      .then(function (res) {
        return res.json().catch(function () { return { error: "The server answered " + res.status + "." }; }).then(function (data) {
          return { ok: res.ok, status: res.status, data: data };
        });
      })
      .then(function (r) {
        saving = false;
        if (r.ok) {
          mtime = String(r.data.mtime);
          setMeta(r.data.owner, r.data.mode, r.data.size);
          undo.markClean(revision);
          changed();
          return;
        }
        changed();
        if (r.data.changed) {
          if (window.confirm("The file was changed by someone else since it was opened. Overwrite their changes?")) {
            save(true);
          } else {
            fail("Not saved: the file was changed by someone else. Copy your changes, then reload to see theirs.");
          }
          return;
        }
        fail("Not saved: " + (r.data.error || "error " + r.status));
      })
      .catch(function () {
        saving = false;
        changed();
        fail("Not saved: the server could not be reached.");
      });
  }

  function start(bytes, res) {
    mtime = res.headers.get("X-File-Mtime") || "";
    setMeta(res.headers.get("X-File-Owner"), res.headers.get("X-File-Mode"), res.headers.get("X-File-Size"));
    var truncated = res.headers.get("X-File-Truncated") === "1";
    if (truncated) {
      notice("This file is larger than 10 MiB, so it cannot be edited here; only its beginning is shown. Download it to see all of it.");
    } else {
      decoded = decode(bytes, true);
      if (decoded) {
        editable = true;
      } else {
        notice("This file is not text in UTF-8 or UTF-16, so it cannot be edited here.");
      }
    }
    if (!decoded) decoded = decode(bytes, false);
    field("encoding").textContent = decoded.encoding.toUpperCase() + (decoded.bom ? " with BOM" : "") + (editable ? "" : " (read only)");

    var ace = window.ace;
    ace.config.set("basePath", "/static/vendor/ace/");
    box.classList.toggle("read-only", !editable);
    editor = ace.edit(box, {
      readOnly: !editable,
      useWorker: false,
      showPrintMargin: false,
      highlightActiveLine: editable,
      highlightGutterLine: editable,
      newLineMode: "auto",
      fontSize: 13,
      fontFamily: "ui-monospace, \"Cascadia Mono\", Consolas, Menlo, monospace",
      scrollPastEnd: 0.5
    });
    var session = editor.session;
    session.setValue(decoded.text);
    session.getUndoManager().reset();
    session.getUndoManager().markClean();
    editor.moveCursorTo(0, 0);
    editor.renderer.scrollToRow(0);
    ace.config.loadModule("ace/ext/modelist", function (modelist) {
      var mode = modelist.getModeForPath(box.getAttribute("data-name")).mode;
      if (modes.indexOf(mode.replace("ace/mode/", "")) < 0) mode = "ace/mode/text";
      session.setMode(mode);
    });
    applyTheme();
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);
    new MutationObserver(applyTheme).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    if (editable) {
      editor.commands.addCommand({ name: "save", bindKey: { win: "Ctrl-S", mac: "Command-S" }, exec: function () { save(false); } });
      editor.on("input", changed);
    }
    changed();
    editor.focus();
  }

  saveBtn.addEventListener("click", function () { save(false); });
  // Ctrl+S outside the editor saves too, instead of saving the page.
  document.addEventListener("keydown", function (e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === "s" || e.key === "S")) {
      e.preventDefault();
      save(false);
    }
  });
  window.addEventListener("beforeunload", function (e) {
    if (dirty()) {
      e.preventDefault();
      e.returnValue = "";
    }
  });
  closeLink.addEventListener("click", function (e) {
    if (dirty() && !window.confirm("Discard the unsaved changes?")) e.preventDefault();
    else editable = false; // no second question from beforeunload
  });

  if (!window.ace) {
    fail("The editor could not be loaded.");
    return;
  }
  fetch(box.getAttribute("data-raw"), { credentials: "same-origin" })
    .then(function (res) {
      if (!res.ok) {
        return res.text().then(function (t) { throw new Error(t.trim() || "error " + res.status); });
      }
      return res.arrayBuffer().then(function (bytes) { start(bytes, res); });
    })
    .catch(function (err) {
      fail("The file could not be opened: " + err.message);
    });
})();
