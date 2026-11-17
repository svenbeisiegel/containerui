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
