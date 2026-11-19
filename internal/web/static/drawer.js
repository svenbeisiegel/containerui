// Output drawer on the right edge: the compose output (partials.html
// "compose_output") and the activity of the Images, Volumes and Networks
// tabs ("activity_drawer"). A page has at most one. Opens and closes it,
// opens it when an action starts a job or records an entry, and keeps the
// log scrolled to the end while it runs. Only the drawer's body is
// refreshed by htmx, so its open state survives the polling.
// Opening it closes any open menu (app.js).
(function () {
  function drawer() { return document.querySelector("aside.drawer"); }

  function toggles() { return document.querySelectorAll("[data-output-toggle]"); }

  function isBody(el) { return el && el.classList && el.classList.contains("drawer-body"); }

  // The log to follow: the compose output, or the newest open entry.
  function log() {
    var d = drawer();
    return d && d.querySelector("details[open] pre.log, pre.log");
  }

  function scrollToEnd() {
    var l = log();
    if (l) l.scrollTop = l.scrollHeight;
  }

  function setOpen(open, focus) {
    var d = drawer();
    if (!d) return;
    d.hidden = !open;
    toggles().forEach(function (t) { t.setAttribute("aria-expanded", open ? "true" : "false"); });
    if (open) {
      scrollToEnd();
      if (focus) {
        var close = d.querySelector("[data-output-close]");
        if (close) close.focus();
      }
    } else if (focus && toggles().length) {
      toggles()[0].focus();
    }
  }

  document.addEventListener("click", function (e) {
    if (!e.target.closest) return;
    if (e.target.closest("[data-output-toggle]")) {
      var d = drawer();
      setOpen(d && d.hidden, true);
    } else if (e.target.closest("[data-output-close]")) {
      setOpen(false, true);
    }
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var d = drawer();
    if (d && !d.hidden) setOpen(false, d.contains(document.activeElement));
  });

  // An action started a job or recorded an entry (HX-Trigger from the server).
  ["compose-job-started", "activity-changed"].forEach(function (name) {
    document.body.addEventListener(name, function () {
      if (window.cuiCloseMenus) window.cuiCloseMenus(null);
      setOpen(true, false);
    });
  });

  // Follow the log unless the user scrolled up to read.
  var follow = true;
  document.body.addEventListener("htmx:beforeSwap", function (e) {
    if (isBody(e.detail.target)) {
      var l = log();
      follow = !l || l.scrollHeight - l.scrollTop - l.clientHeight < 24;
    }
  });
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (follow && isBody(e.detail.target)) scrollToEnd();
  });

  scrollToEnd();
})();
