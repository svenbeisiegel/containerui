// Small behaviours that would otherwise need inline handlers (blocked by the
// Content-Security-Policy).

// Clear forms marked data-reset-on-success after a successful htmx request.
document.addEventListener("htmx:afterRequest", function (e) {
  var form = e.detail.elt;
  if (e.detail.successful && form.matches && form.matches("form[data-reset-on-success]")) {
    form.reset();
  }
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
