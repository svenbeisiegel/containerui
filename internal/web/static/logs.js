// Live container log (logs_watch page). Server-Sent Events from the stream
// URL are appended to the <pre> as plain text. The browser's automatic
// reconnect would replay the initial lines, so a lost or ended stream is
// closed and can be restarted with the Reconnect button instead.
(function () {
  "use strict";
  var MAX_LINES = 20000; // older lines are dropped beyond this

  var out = document.getElementById("log");
  if (!out) return;
  var url = out.getAttribute("data-stream");
  var status = document.querySelector("[data-log-status]");
  var count = document.querySelector("[data-log-count]");
  var autoscroll = document.querySelector("[data-log-autoscroll]");
  var reconnect = document.querySelector("[data-log-reconnect]");
  var note = document.querySelector("[data-log-note]");
  var lines = 0;
  var source = null;

  function setStatus(text, cls, message) {
    status.textContent = text;
    status.className = "badge" + (cls ? " " + cls : "");
    note.textContent = message || "";
    note.hidden = !message;
  }

  function atBottom() {
    var doc = document.documentElement;
    return window.innerHeight + window.scrollY >= doc.scrollHeight - 40;
  }

  function scrollToEnd() {
    window.scrollTo(0, document.documentElement.scrollHeight);
  }

  function append(text) {
    var stick = autoscroll.checked && atBottom();
    var node = document.createTextNode(text + "\n");
    node.lineCount = text.split("\n").length;
    out.appendChild(node);
    lines += node.lineCount;
    while (lines > MAX_LINES && out.firstChild !== node) {
      lines -= out.firstChild.lineCount || 0;
      out.removeChild(out.firstChild);
    }
    count.textContent = lines;
    if (stick) scrollToEnd();
  }

  function stop(text, cls, message) {
    if (source) source.close();
    source = null;
    setStatus(text, cls, message);
    reconnect.hidden = false;
  }

  function connect() {
    out.textContent = "";
    lines = 0;
    count.textContent = "0";
    reconnect.hidden = true;
    setStatus("Connecting…");
    if (!window.EventSource) {
      stop("Unsupported", "bad", "This browser cannot receive live logs.");
      return;
    }
    source = new EventSource(url);
    source.onopen = function () { setStatus("Live", "ok"); };
    source.onmessage = function (e) { append(e.data); };
    source.addEventListener("end", function (e) { stop("Ended", "info", e.data); });
    source.onerror = function () {
      if (source) stop("Disconnected", "bad", "The connection was lost. If your session ended, sign in again, then reconnect.");
    };
  }

  autoscroll.addEventListener("change", function () {
    if (autoscroll.checked) scrollToEnd();
  });
  reconnect.addEventListener("click", connect);
  connect();
})();
