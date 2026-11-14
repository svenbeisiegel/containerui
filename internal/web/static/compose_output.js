// Compose output drawer (partials.html "compose_output"): open and close
// it, open it when a compose action starts a job, and keep the log scrolled
// to the end while it runs. Only the drawer's body is refreshed by htmx, so
// its open state survives the polling.
(function () {
  function drawer() { return document.getElementById("compose-output"); }

  function toggles() { return document.querySelectorAll("[data-output-toggle]"); }

  function log() {
    var d = drawer();
    return d && d.querySelector("pre.log");
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
    var d = drawer();
    if (e.key === "Escape" && d && !d.hidden) setOpen(false, d.contains(document.activeElement));
  });

  // A compose action started a job (HX-Trigger from the server).
  document.body.addEventListener("compose-job-started", function () { setOpen(true, false); });

  // Follow the log unless the user scrolled up to read.
  var follow = true;
  document.body.addEventListener("htmx:beforeSwap", function (e) {
    if (e.detail.target && e.detail.target.id === "compose-jobs") {
      var l = log();
      follow = !l || l.scrollHeight - l.scrollTop - l.clientHeight < 24;
    }
  });
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (follow && e.detail.target && e.detail.target.id === "compose-jobs") scrollToEnd();
  });

  scrollToEnd();
})();
