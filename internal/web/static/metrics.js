// Dashboard metrics: crosshair and tooltip on the server-rendered line
// charts. Each chart carries its sample times and formatted values in
// data-chart (see charts.go). Event delegation keeps it working after htmx
// replaces the dashboard every 5 s.
(function () {
  var cache = new WeakMap();

  function data(chart) {
    var d = cache.get(chart);
    if (d === undefined) {
      try { d = JSON.parse(chart.getAttribute("data-chart") || "null"); } catch (e) { d = null; }
      cache.set(chart, d);
    }
    return d;
  }

  function nearest(xs, x) {
    var best = -1, dist = Infinity;
    for (var i = 0; i < xs.length; i++) {
      var dx = Math.abs(xs[i] - x);
      if (dx < dist) { dist = dx; best = i; }
    }
    return best;
  }

  function row(cls, name, value) {
    var r = document.createElement("div");
    r.className = "r";
    var sw = document.createElement("span");
    sw.className = "swatch " + cls;
    var b = document.createElement("b");
    b.textContent = value;
    r.appendChild(sw);
    r.appendChild(document.createTextNode(name));
    r.appendChild(b);
    return r;
  }

  function hide(chart) {
    chart.classList.remove("hover");
    var tip = chart.querySelector(".chart-tip");
    if (tip) tip.hidden = true;
  }

  function show(plot, ev) {
    var chart = plot.closest(".chart");
    var d = chart && data(chart);
    if (!d || !d.x || !d.x.length) return;
    var box = plot.getBoundingClientRect();
    if (box.width <= 0) return;
    var x = (ev.clientX - box.left) / box.width * d.w;
    var i = nearest(d.x, x);
    if (i < 0) return;

    var line = chart.querySelector(".crosshair");
    line.setAttribute("x1", d.x[i]);
    line.setAttribute("x2", d.x[i]);
    chart.classList.add("hover");

    var tip = chart.querySelector(".chart-tip");
    tip.textContent = "";
    var t = document.createElement("div");
    t.className = "t";
    t.textContent = d.t[i];
    tip.appendChild(t);
    var any = false;
    (d.s || []).forEach(function (s) {
      if (s.v[i]) { tip.appendChild(row(s.c, s.n, s.v[i])); any = true; }
    });
    if (!any) { hide(chart); return; }
    tip.hidden = false;
    // keep the tooltip inside the plot, on the side away from the cursor
    var px = d.x[i] / d.w * box.width;
    var w = tip.offsetWidth;
    var left = px + 12;
    if (left + w > box.width) left = Math.max(0, px - 12 - w);
    tip.style.left = left + "px";
  }

  document.addEventListener("pointermove", function (ev) {
    var plot = ev.target.closest && ev.target.closest(".chart-plot");
    document.querySelectorAll(".chart.hover").forEach(function (c) {
      if (!plot || c !== plot.closest(".chart")) hide(c);
    });
    if (plot) show(plot, ev);
  });
  document.addEventListener("pointerleave", function () {
    document.querySelectorAll(".chart.hover").forEach(hide);
  });
})();
