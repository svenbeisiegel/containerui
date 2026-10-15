// Applies the stored light/dark choice before the first paint. Loaded
// synchronously in <head>; without a stored choice the CSS follows the
// system preference (prefers-color-scheme).
(function () {
  try {
    var t = localStorage.getItem("cui-theme");
    if (t === "light" || t === "dark") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    // Storage unavailable (private mode, blocked): follow the system.
  }
})();
