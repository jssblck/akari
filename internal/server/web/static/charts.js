// akari charts: a small, dependency-free SVG time-series renderer. It hydrates
// any [data-chart] container from the inline JSON the server embeds, draws a
// hairline grid with mono tick labels on the dark ground, and tracks a lilac
// crosshair with a mono readout. No build step, no external library; the binary
// stays self-contained. See DESIGN.md (Charts) for the visual contract.
(function () {
  "use strict";
  var NS = "http://www.w3.org/2000/svg";
  var MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  function svgEl(name, attrs) {
    var e = document.createElementNS(NS, name);
    if (attrs) for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function fmtCost(v) {
    if (v === 0) return "$0";
    if (v < 0.01) return "$" + v.toFixed(4);
    if (v < 100) return "$" + v.toFixed(2);
    return "$" + Math.round(v).toLocaleString();
  }
  function fmtTok(v) {
    if (v >= 1e9) return (v / 1e9).toFixed(1) + "B";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
    if (v >= 1e3) return (v / 1e3).toFixed(1) + "k";
    return String(v);
  }
  function fmtDay(iso) {
    var p = iso.split("-");
    if (p.length !== 3) return iso;
    return MONTHS[parseInt(p[1], 10) - 1] + " " + parseInt(p[2], 10);
  }
  // niceMax rounds a max value up to a clean axis ceiling (1/2/5 x 10^n).
  function niceMax(v) {
    if (v <= 0) return 1;
    var exp = Math.floor(Math.log10(v));
    var base = Math.pow(10, exp);
    var f = v / base;
    var nice = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return nice * base;
  }

  function seriesFor(metric, data) {
    if (metric === "tokens") {
      return [
        { key: "input", label: "Input", color: "#c6a8f2", data: data.input || [] },
        { key: "output", label: "Output", color: "#88cfce", data: data.output || [] },
        { key: "cacheRead", label: "Cache read", color: "#f0bf92", data: data.cacheRead || [] },
        { key: "cacheWrite", label: "Cache write", color: "#ec98b0", data: data.cacheWrite || [] },
      ];
    }
    return [{ key: "cost", label: "Cost", color: "#c6a8f2", data: data.cost || [] }];
  }

  function render(container, data, metric) {
    // Reset, preserving the inline JSON script.
    Array.prototype.slice.call(container.querySelectorAll("svg,.chart-readout,.chart-empty")).forEach(function (n) { n.remove(); });
    container.classList.remove("cursor-on");

    var days = data.days || [];
    if (days.length === 0) {
      var empty = document.createElement("div");
      empty.className = "chart-empty";
      empty.textContent = "No dated usage to chart.";
      container.appendChild(empty);
      return;
    }

    var W = container.clientWidth || 600;
    var H = container.clientHeight || 240;
    var padL = 52, padR = 10, padT = 10, padB = 22;
    var plotW = Math.max(10, W - padL - padR);
    var plotH = Math.max(10, H - padT - padB);
    var series = seriesFor(metric, data);
    var fmtV = metric === "tokens" ? fmtTok : fmtCost;

    var maxV = 0;
    series.forEach(function (s) { s.data.forEach(function (v) { if (v > maxV) maxV = v; }); });
    var top = niceMax(maxV);

    var n = days.length;
    function xAt(i) { return n === 1 ? padL + plotW / 2 : padL + plotW * i / (n - 1); }
    function yAt(v) { return padT + plotH * (1 - v / top); }

    var svg = svgEl("svg", { viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img" });

    // Horizontal grid + y labels (5 lines).
    for (var g = 0; g <= 4; g++) {
      var gv = top * g / 4;
      var gy = yAt(gv);
      svg.appendChild(svgEl("line", { class: "grid-line", x1: padL, y1: gy, x2: W - padR, y2: gy }));
      var yl = svgEl("text", { class: "axis-label", x: padL - 6, y: gy + 3, "text-anchor": "end" });
      yl.textContent = fmtV(gv);
      svg.appendChild(yl);
    }
    // X labels: up to 5 evenly spaced dates.
    var ticks = Math.min(5, n);
    for (var t = 0; t < ticks; t++) {
      var idx = ticks === 1 ? 0 : Math.round((n - 1) * t / (ticks - 1));
      var xl = svgEl("text", { class: "axis-label", x: xAt(idx), y: H - 6, "text-anchor": t === 0 ? "start" : t === ticks - 1 ? "end" : "middle" });
      xl.textContent = fmtDay(days[idx]);
      svg.appendChild(xl);
    }

    // Series: area (single-series only) then line.
    series.forEach(function (s) {
      var line = "";
      var area = "";
      for (var i = 0; i < n; i++) {
        var x = xAt(i), y = yAt(s.data[i] || 0);
        line += (i === 0 ? "M" : "L") + x.toFixed(1) + "," + y.toFixed(1) + " ";
      }
      if (series.length === 1) {
        area = "M" + xAt(0).toFixed(1) + "," + yAt(0).toFixed(1) + " " +
          line.replace(/^M/, "L") + "L" + xAt(n - 1).toFixed(1) + "," + yAt(0).toFixed(1) + " Z";
        svg.appendChild(svgEl("path", { class: "series-area", d: area, fill: s.color }));
      }
      svg.appendChild(svgEl("path", { class: "series-line", d: line.trim(), stroke: s.color }));
    });

    // Crosshair + per-series cursor dots.
    var cross = svgEl("line", { class: "crosshair", x1: 0, y1: padT, x2: 0, y2: padT + plotH });
    svg.appendChild(cross);
    var dots = series.map(function (s) {
      var d = svgEl("circle", { class: "cursor-dot", r: 3, fill: s.color });
      svg.appendChild(d);
      return d;
    });
    container.appendChild(svg);

    var readout = document.createElement("div");
    readout.className = "chart-readout";
    container.appendChild(readout);

    function moveTo(clientX) {
      var rect = container.getBoundingClientRect();
      var px = clientX - rect.left;
      var i = n === 1 ? 0 : Math.round((px - padL) / plotW * (n - 1));
      i = Math.max(0, Math.min(n - 1, i));
      var cx = xAt(i);
      cross.setAttribute("x1", cx); cross.setAttribute("x2", cx);
      var html = '<span class="ro-key">' + fmtDay(days[i]) + "</span>";
      series.forEach(function (s, si) {
        var v = s.data[i] || 0;
        dots[si].setAttribute("cx", cx);
        dots[si].setAttribute("cy", yAt(v));
        html += "<br>" + (series.length > 1 ? s.label + " " : "") + fmtV(v);
      });
      readout.innerHTML = html;
      container.classList.add("cursor-on");
    }
    container.onmousemove = function (ev) { moveTo(ev.clientX); };
    container.onmouseleave = function () { container.classList.remove("cursor-on"); };
  }

  function initChart(container) {
    var raw = container.getAttribute("data-series");
    if (!raw) { var s = container.querySelector(".chart-data"); raw = s && s.textContent; }
    if (!raw) return;
    var data;
    try { data = JSON.parse(raw); } catch (e) { return; }
    var metric = "cost";
    var id = container.id;

    function draw() { render(container, data, metric); }
    draw();

    // Metric toggle buttons target this chart by id.
    Array.prototype.slice.call(document.querySelectorAll('.seg[data-chart-target="' + id + '"]')).forEach(function (btn) {
      btn.addEventListener("click", function () {
        metric = btn.getAttribute("data-metric");
        document.querySelectorAll('.seg[data-chart-target="' + id + '"]').forEach(function (b) { b.classList.toggle("active", b === btn); });
        draw();
      });
    });

    // Redraw on resize (debounced), so the line tracks container width.
    var raf = 0;
    window.addEventListener("resize", function () {
      if (raf) cancelAnimationFrame(raf);
      raf = requestAnimationFrame(draw);
    });
  }

  function init() {
    Array.prototype.slice.call(document.querySelectorAll("[data-chart]")).forEach(initChart);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
