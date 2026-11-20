// Small behaviours that would otherwise need inline handlers (blocked by the
// Content-Security-Policy).

// Clear forms marked data-reset-on-success after a successful htmx request.
document.addEventListener("htmx:afterRequest", function (e) {
  var form = e.detail.elt;
  if (e.detail.successful && form.matches && form.matches("form[data-reset-on-success]")) {
    form.reset();
  }
});

// Ask before submitting forms marked data-confirm="question" (plain form
// posts; htmx forms use hx-confirm).
document.addEventListener("submit", function (e) {
  var msg = e.target.getAttribute && e.target.getAttribute("data-confirm");
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Create form: "Create and attach" needs a TTY and STDIN. It submits into a
// new tab, which ends on the attach page, so this tab moves on to the list.
(function () {
  var btn = document.querySelector("[data-attach-create]");
  if (!btn || !btn.form) return;
  var form = btn.form;
  var tty = form.elements.namedItem("tty");
  var stdin = form.elements.namedItem("interactive");
  function sync() { btn.hidden = !(tty && tty.checked && stdin && stdin.checked); }
  form.addEventListener("change", sync);
  sync();
  form.addEventListener("submit", function (e) {
    if (e.submitter !== btn || e.defaultPrevented) return;
    setTimeout(function () { window.location.href = "/containers"; }, 300);
  });
})();

// Light/dark toggle. theme.js applied any stored choice already; a click
// flips the effective theme and remembers it.
(function () {
  var root = document.documentElement;
  var media = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;

  function current() {
    var t = root.getAttribute("data-theme");
    if (t === "light" || t === "dark") return t;
    return media && media.matches ? "dark" : "light";
  }

  function label() {
    var next = current() === "dark" ? "light" : "dark";
    var text = "Switch to " + next + " mode";
    document.querySelectorAll("[data-theme-toggle]").forEach(function (b) {
      b.setAttribute("aria-label", text);
      b.setAttribute("title", text);
    });
  }

  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-theme-toggle]");
    if (!btn) return;
    var next = current() === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("cui-theme", next); } catch (err) { /* not persisted */ }
    label();
  });

  if (media && media.addEventListener) media.addEventListener("change", label);
  label();
})();

// On the sign-in page, keep the container runtime warm while the user types:
// wslc stops its session after ~30s idle and restarting it takes seconds.
// Pings stop after 10 minutes so an abandoned tab doesn't keep it alive.
(function () {
  if (!document.querySelector("[data-keep-warm]")) return;
  var started = Date.now();
  var timer = setInterval(function () {
    if (Date.now() - started > 10 * 60 * 1000) { clearInterval(timer); return; }
    fetch("/login/warm", { credentials: "same-origin", cache: "no-store" }).catch(function () {});
  }, 15000);
})();

// Forms marked data-file-load: "Open file…" (data-file-open) or dropping a
// file onto data-file-drop loads its text into the data-file-target textarea.
(function () {
  var maxSize = 2 * 1024 * 1024;

  function load(form, file) {
    if (!file) return;
    if (file.size > maxSize) {
      window.alert(file.name + " is larger than 2 MiB.");
      return;
    }
    var target = form.querySelector("[data-file-target]");
    var reader = new FileReader();
    reader.onload = function () {
      target.value = String(reader.result).replace(/\r\n/g, "\n");
      target.focus();
    };
    reader.readAsText(file);
  }

  document.querySelectorAll("form[data-file-load]").forEach(function (form) {
    var input = form.querySelector("[data-file-input]");
    var open = form.querySelector("[data-file-open]");
    var drop = form.querySelector("[data-file-drop]");
    if (open && input) {
      open.addEventListener("click", function () { input.click(); });
      input.addEventListener("change", function () {
        load(form, input.files[0]);
        input.value = "";
      });
    }
    if (!drop) return;
    var depth = 0; // dragenter/dragleave also fire for child elements
    drop.addEventListener("dragenter", function (e) {
      e.preventDefault();
      depth++;
      drop.classList.add("dragging");
    });
    drop.addEventListener("dragover", function (e) {
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    });
    drop.addEventListener("dragleave", function () {
      if (--depth <= 0) {
        depth = 0;
        drop.classList.remove("dragging");
      }
    });
    drop.addEventListener("drop", function (e) {
      e.preventDefault();
      depth = 0;
      drop.classList.remove("dragging");
      if (e.dataTransfer.files.length > 0) load(form, e.dataTransfer.files[0]);
    });
  });
})();

// Menus. A header menu (<details class="menu">, e.g. "Prune volumes") drops
// down below its button. A row menu (<details class="row-menu">, the ⋮ of a
// table row) is placed next to its button with fixed positioning, so the
// table's scroll box does not clip it, and flips above it near the bottom
// of the window. Both close on an outside click or Escape; a row menu also
// closes when one of its actions is submitted, and stays open when htmx
// refreshes its table.
(function () {
  var OPEN = "details.menu[open], details.row-menu[open]";

  function closeMenus(except) {
    document.querySelectorAll(OPEN).forEach(function (m) {
      if (m !== except) m.open = false;
    });
  }
  window.cuiCloseMenus = closeMenus; // drawer.js: an action started a job

  function place(menu) {
    var panel = menu.querySelector(".row-menu-panel");
    var button = menu.querySelector("summary");
    if (!panel || !button) return;
    var r = button.getBoundingClientRect();
    var w = panel.offsetWidth, h = panel.offsetHeight;
    var vw = document.documentElement.clientWidth, vh = window.innerHeight;
    var left = Math.max(8, Math.min(r.right - w, vw - w - 8));
    var top = r.bottom + 4;
    if (top + h > vh - 8 && r.top - h - 4 >= 8) top = r.top - h - 4;
    panel.style.left = left + "px";
    panel.style.top = top + "px";
    menu.classList.add("placed");
  }

  function placeAll() {
    document.querySelectorAll("details.row-menu[open]").forEach(place);
  }

  // toggle does not bubble; listen while capturing.
  document.addEventListener("toggle", function (e) {
    var el = e.target;
    if (!el.matches) return;
    if (el.matches("details.menu, details.row-menu")) {
      if (el.open) {
        closeMenus(el);
        if (el.matches(".row-menu")) place(el);
      } else {
        el.classList.remove("placed");
      }
    } else if (el.matches("details.row-menu-sub")) {
      var menu = el.closest("details.row-menu");
      if (menu && menu.open) place(menu);
      var input = el.open && el.querySelector("input:not([type=hidden]), select");
      if (input) input.focus();
    }
  }, true);

  document.addEventListener("click", function (e) {
    if (e.target.closest) closeMenus(e.target.closest("details.menu, details.row-menu"));
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var menu = document.querySelector(OPEN);
    if (!menu) return;
    menu.open = false;
    var s = menu.querySelector("summary");
    if (s) s.focus();
    e.stopImmediatePropagation(); // the drawer stays open
  }, true);

  document.addEventListener("submit", function (e) {
    var menu = e.target.closest && e.target.closest("details.row-menu");
    if (menu) setTimeout(function () { menu.open = false; }, 0);
  });

  window.addEventListener("scroll", placeAll, true);
  window.addEventListener("resize", placeAll);

  // Polling replaces the whole table: reopen the row menu that was open,
  // with the Tag… or Update… form as it was.
  var saved = null;
  document.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail.target;
    var m = t && t.querySelector && t.querySelector("details.row-menu[open]");
    if (!m) return;
    var sub = m.querySelector("details.row-menu-sub");
    var input = m.querySelector("details.row-menu-sub :is(input:not([type=hidden]), select)");
    saved = {
      key: m.getAttribute("data-menu"),
      focus: m.contains(document.activeElement),
      sub: !!(sub && sub.open),
      value: input ? input.value : "",
      inInput: !!input && document.activeElement === input
    };
  });
  document.addEventListener("htmx:afterSwap", function () {
    var s = saved;
    saved = null;
    if (!s || !s.key) return;
    var m = document.querySelector('details.row-menu[data-menu="' + CSS.escape(s.key) + '"]');
    if (!m) return;
    m.open = true;
    place(m);
    var sub = m.querySelector("details.row-menu-sub");
    var input = m.querySelector("details.row-menu-sub :is(input:not([type=hidden]), select)");
    if (sub && s.sub) sub.open = true;
    if (input) input.value = s.value;
    if (s.inInput && input) input.focus();
    else if (s.focus) m.querySelector("summary").focus();
  });
})();

// Image picker: a small combobox over the datalist. The native datalist
// filters by the whole text, so "nginx:1.27" would hide "nginx:latest";
// this one ignores the tag, so every tag of the typed image stays listed.
(function () {
  document.querySelectorAll("[data-image-picker]").forEach(function (box, n) {
    var input = box.querySelector("input");
    var dl = box.querySelector("datalist");
    if (!input || !dl) return;
    var refs = Array.prototype.map.call(dl.options, function (o) { return o.value; });
    input.removeAttribute("list");

    var listId = "image-picker-list-" + n;
    var list = document.createElement("ul");
    list.id = listId;
    list.className = "combo-list";
    list.setAttribute("role", "listbox");
    list.hidden = true;
    box.appendChild(list);
    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-expanded", "false");
    input.setAttribute("aria-controls", listId);

    var active = -1;

    // Repository part of a reference: no digest, no tag after the last "/".
    function repo(s) {
      s = s.split("@")[0];
      var slash = s.lastIndexOf("/");
      var colon = s.lastIndexOf(":");
      if (colon > slash) s = s.slice(0, colon);
      return s.toLowerCase();
    }

    function items() { return list.querySelectorAll("[role=option]"); }

    function setActive(i) {
      var opts = items();
      if (active >= 0 && opts[active]) opts[active].removeAttribute("aria-selected");
      active = i;
      if (i >= 0 && opts[i]) {
        opts[i].setAttribute("aria-selected", "true");
        input.setAttribute("aria-activedescendant", opts[i].id);
        opts[i].scrollIntoView({ block: "nearest" });
      } else {
        input.removeAttribute("aria-activedescendant");
      }
    }

    function close() {
      list.hidden = true;
      input.setAttribute("aria-expanded", "false");
      setActive(-1);
    }

    function open() {
      var q = repo(input.value.trim());
      list.textContent = "";
      active = -1;
      input.removeAttribute("aria-activedescendant");
      refs.forEach(function (ref) {
        if (q && repo(ref).indexOf(q) < 0) return;
        var li = document.createElement("li");
        li.id = listId + "-" + list.children.length;
        li.setAttribute("role", "option");
        li.textContent = ref;
        list.appendChild(li);
      });
      var any = list.children.length > 0;
      list.hidden = !any;
      input.setAttribute("aria-expanded", any ? "true" : "false");
    }

    function pick(li) {
      input.value = li.textContent;
      input.dispatchEvent(new Event("change", { bubbles: true }));
      close();
    }

    input.addEventListener("focus", open);
    input.addEventListener("input", open);
    input.addEventListener("blur", close);
    input.addEventListener("keydown", function (e) {
      var opts = items();
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        if (list.hidden) { open(); opts = items(); }
        if (!opts.length) return;
        var i = e.key === "ArrowDown" ? active + 1 : active - 1;
        if (i >= opts.length) i = 0;
        if (i < 0) i = opts.length - 1;
        setActive(i);
      } else if (e.key === "Enter") {
        if (list.hidden) return;
        e.preventDefault();
        if (active >= 0 && opts[active]) pick(opts[active]);
        else close();
      } else if (e.key === "Escape") {
        if (list.hidden) return;
        e.preventDefault();
        close();
      }
    });
    // mousedown would blur the input (and close the list) before the click.
    list.addEventListener("mousedown", function (e) { e.preventDefault(); });
    list.addEventListener("click", function (e) {
      var li = e.target.closest("[role=option]");
      if (li) pick(li);
    });
  });
})();

// Volume list: the table's checkboxes belong to the "Backup" form. Keep the
// count, the submit button and the select-all box in step, also after htmx
// replaced the table.
(function () {
  var form = document.getElementById("volume-backup-form");
  if (!form) return;
  var submit = form.querySelector("[data-backup-submit]");
  var count = form.querySelector("[data-backup-count]");
  var inUse = form.querySelector("[data-backup-in-use]");
  var summary = document.querySelector("[data-backup-summary]");
  function boxes() {
    return Array.prototype.slice.call(document.querySelectorAll('input[name="volume"][form="volume-backup-form"]'));
  }
  function sync() {
    var all = boxes();
    var sel = all.filter(function (b) { return b.checked; });
    var busy = sel.some(function (b) { return b.hasAttribute("data-in-use"); });
    submit.disabled = sel.length === 0;
    submit.textContent = sel.length > 1 ? "Download backup of " + sel.length + " volumes" : "Download backup";
    count.textContent = sel.length === 0 ? "Select the volumes to back up in the table." :
      sel.length === 1 ? "Volume " + sel[0].value + " is selected." : sel.length + " volumes are selected; they go into one file.";
    inUse.hidden = !busy;
    if (summary) summary.textContent = sel.length ? "Backup (" + sel.length + ")" : "Backup";
    var head = document.querySelector("[data-select-all]");
    if (head) {
      head.checked = all.length > 0 && sel.length === all.length;
      head.indeterminate = sel.length > 0 && sel.length < all.length;
    }
  }
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.matches("[data-select-all]")) {
      boxes().forEach(function (b) { b.checked = t.checked; });
    }
    if (t.matches('[data-select-all], input[name="volume"][form="volume-backup-form"]')) sync();
  });
  document.body.addEventListener("htmx:afterSettle", sync);
  sync();
})();

// Volume restore: upload the backup file with a progress bar. The server
// answers JSON: where to go next, or what is wrong with the file.
(function () {
  var form = document.querySelector("form[data-volume-upload]");
  if (!form || !window.XMLHttpRequest || !window.FormData) return;
  var box = form.querySelector("[data-upload-progress]");
  var bar = box.querySelector("progress");
  var status = form.querySelector("[data-upload-status]");
  var errBox = form.querySelector("[data-upload-error]");
  var btn = form.querySelector('button[type="submit"]');
  function fail(msg) {
    errBox.textContent = msg;
    errBox.hidden = false;
    box.hidden = true;
    btn.disabled = false;
  }
  form.addEventListener("submit", function (e) {
    e.preventDefault();
    var xhr = new XMLHttpRequest();
    xhr.open("POST", form.action);
    xhr.setRequestHeader("X-CSRF-Token", form.elements.namedItem("csrf").value);
    xhr.setRequestHeader("Accept", "application/json");
    xhr.upload.addEventListener("progress", function (ev) {
      if (!ev.lengthComputable) return;
      bar.max = ev.total;
      bar.value = ev.loaded;
      status.textContent = Math.floor(ev.loaded * 100 / ev.total) + " % uploaded";
    });
    xhr.upload.addEventListener("load", function () {
      bar.removeAttribute("value");
      status.textContent = "Checking the backup file…";
    });
    xhr.addEventListener("load", function () {
      var res = null;
      try { res = JSON.parse(xhr.responseText); } catch (_) { /* not JSON */ }
      if (res && res.redirect) {
        window.location.href = res.redirect;
        return;
      }
      fail((res && res.error) || "The upload failed (HTTP " + xhr.status + ").");
    });
    xhr.addEventListener("error", function () { fail("The upload failed: the connection was lost."); });
    errBox.hidden = true;
    box.hidden = false;
    btn.disabled = true;
    bar.value = 0;
    status.textContent = "Uploading…";
    xhr.send(new FormData(form));
  });
})();
