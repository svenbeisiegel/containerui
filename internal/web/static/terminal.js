// Browser terminal page (terminal.html): connects xterm.js to
// /containers/{id}/terminal/ws as soon as the page opens. Binary WebSocket
// messages carry raw terminal bytes; text messages carry JSON control
// messages (resize, exit, error).
(function () {
  "use strict";
  var box = document.querySelector("[data-terminal]");
  if (!box) return;
  var status = document.querySelector("[data-terminal-status]");
  var note = document.querySelector("[data-terminal-note]");
  var connectBtn = document.querySelector("[data-terminal-connect]");
  var disconnectBtn = document.querySelector("[data-terminal-disconnect]");
  var session = null; // the open connection, if any
  var lastTerm = null; // kept after disconnect so its output stays readable

  function setStatus(text, cls, message) {
    status.textContent = text;
    status.className = "badge" + (cls ? " " + cls : "");
    note.textContent = message || "";
    note.hidden = !message;
  }

  function setConnected(connected) {
    connectBtn.hidden = connected;
    disconnectBtn.hidden = !connected;
  }

  // fit resizes term to the cells that fit into the terminal box. xterm.js
  // sizes its screen element to exactly cols x rows cells, which gives the
  // cell size without reaching into its internals.
  function fit(term) {
    var el = term.element;
    if (!el || !el.parentElement) return;
    var screen = el.querySelector(".xterm-screen");
    var viewport = el.querySelector(".xterm-viewport");
    if (!screen) return;
    var r = screen.getBoundingClientRect();
    var cellW = r.width / term.cols, cellH = r.height / term.rows;
    if (!cellW || !cellH) return;
    // Content box of the parent (clientWidth includes its padding),
    // less the terminal's own padding and scrollbar.
    var parent = el.parentElement, ps = window.getComputedStyle(parent), es = window.getComputedStyle(el);
    var px = function (style, a, b) { return (parseFloat(style[a]) || 0) + (parseFloat(style[b]) || 0); };
    var scrollbar = viewport ? viewport.offsetWidth - viewport.clientWidth : 0;
    var w = parent.clientWidth - px(ps, "paddingLeft", "paddingRight") - px(es, "paddingLeft", "paddingRight") - scrollbar;
    var h = parent.clientHeight - px(ps, "paddingTop", "paddingBottom") - px(es, "paddingTop", "paddingBottom");
    var cols = Math.max(2, Math.floor(w / cellW));
    var rows = Math.max(1, Math.floor(h / cellH));
    if (cols !== term.cols || rows !== term.rows) term.resize(cols, rows);
  }

  function connect() {
    if (session || !window.Terminal) return;

    if (lastTerm) lastTerm.dispose();
    var term = new window.Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, "Cascadia Mono", Consolas, monospace',
      fontSize: 14,
      scrollback: 5000,
      theme: { background: "#071026", foreground: "#e6edf7", cursor: "#00a1e0", selectionBackground: "#35508a" }
    });
    lastTerm = term;
    term.open(box);
    fit(term);

    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var url = proto + "//" + location.host + box.getAttribute("data-url") +
      "?csrf=" + encodeURIComponent(box.getAttribute("data-csrf")) +
      "&cols=" + term.cols + "&rows=" + term.rows;
    var ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    var encoder = new TextEncoder();
    var ended = null; // message to show when the socket closes
    var failed = false; // ended is an error
    var opened = false;
    var s = { term: term, ws: ws };
    session = s;
    setConnected(true);
    setStatus("Connecting…");

    function sendResize() {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    }

    var observer = window.ResizeObserver ? new ResizeObserver(function () {
      try { fit(term); } catch (e) { /* terminal disposed */ }
    }) : null;
    if (observer) observer.observe(box);
    term.onResize(sendResize);

    term.onData(function (data) {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data));
    });

    ws.onopen = function () {
      opened = true;
      setStatus("Connected", "ok");
      sendResize();
      term.focus();
    };
    ws.onmessage = function (e) {
      if (typeof e.data !== "string") {
        term.write(new Uint8Array(e.data));
        return;
      }
      var m;
      try { m = JSON.parse(e.data); } catch (err) { return; }
      if (m.type === "exit") ended = "The shell exited with code " + m.code + ".";
      if (m.type === "error") { ended = m.message; failed = true; }
    };
    ws.onclose = function () {
      if (observer) observer.disconnect();
      term.options.cursorBlink = false;
      term.options.disableStdin = true;
      if (session === s) session = null;
      setConnected(false);
      if (ended) {
        setStatus(failed ? "Error" : "Ended", failed ? "bad" : "info", ended);
      } else if (!opened) {
        // The upgrade was refused (container stopped, no shell, too many
        // terminals, session ended, ...). The browser does not expose why.
        setStatus("Could not connect", "bad",
          "The container may have stopped, too many terminals may be open, or your session ended. Reload the page to see why.");
      } else {
        setStatus("Disconnected", "info");
      }
    };
  }

  function disconnect() {
    if (session) session.ws.close(1000, "closed by user");
  }

  connectBtn.addEventListener("click", connect);
  disconnectBtn.addEventListener("click", disconnect);
  // Close the shell when the page goes away.
  window.addEventListener("pagehide", disconnect);
  connect();
})();
