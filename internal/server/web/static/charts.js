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

  // ============================ Calendar heatmap ============================
  // The overview's activity grid: one cell per day over a trailing year, GitHub
  // contribution-graph style. It hydrates from the same daily JSON as the line
  // chart (sparse: only days with usage), fills the gaps client-side, and scales
  // cell intensity by the selected metric (token volume or dollars). Hover lifts
  // a tooltip with the day's total and its token breakdown.
  var DAY_MS = 86400000;
  var WEEKS = 53; // trailing columns, ~1 year ending on the current week

  // utcDay returns the UTC-midnight timestamp for a y/m/d triple. Server day
  // buckets are UTC-truncated, so the grid keys days in UTC to line up with them.
  function utcDay(y, m, d) { return Date.UTC(y, m, d); }
  function keyOf(ts) {
    var d = new Date(ts);
    var m = d.getUTCMonth() + 1, day = d.getUTCDate();
    return d.getUTCFullYear() + "-" + (m < 10 ? "0" + m : m) + "-" + (day < 10 ? "0" + day : day);
  }
  function fmtFullDay(ts) {
    var d = new Date(ts);
    return MONTHS[d.getUTCMonth()] + " " + d.getUTCDate() + ", " + d.getUTCFullYear();
  }

  // valueFor picks the scalar a day contributes to intensity for the metric:
  // total tokens across all four classes, or dollars.
  function valueFor(rec, metric) {
    if (!rec) return 0;
    if (metric === "cost") return rec.cost || 0;
    return (rec.input || 0) + (rec.output || 0) + (rec.cacheRead || 0) + (rec.cacheWrite || 0);
  }
  // levelFor maps a value to one of five steps (0 empty, 1-4 ramp). A sqrt scale
  // lifts the long tail of small days off the floor so they remain legible.
  function levelFor(v, max) {
    if (v <= 0 || max <= 0) return 0;
    var f = Math.sqrt(v / max);
    return f > 0.75 ? 4 : f > 0.5 ? 3 : f > 0.25 ? 2 : 1;
  }

  function buildIndex(data) {
    var days = data.days || [];
    var idx = {};
    for (var i = 0; i < days.length; i++) {
      idx[days[i]] = {
        cost: (data.cost || [])[i] || 0,
        input: (data.input || [])[i] || 0,
        output: (data.output || [])[i] || 0,
        cacheRead: (data.cacheRead || [])[i] || 0,
        cacheWrite: (data.cacheWrite || [])[i] || 0,
      };
    }
    return idx;
  }

  function renderHeatmap(container, index, metric) {
    Array.prototype.slice.call(container.querySelectorAll("svg,.heatmap-tooltip,.chart-empty")).forEach(function (n) { n.remove(); });

    if (!Object.keys(index).length) {
      var empty = document.createElement("div");
      empty.className = "chart-empty";
      empty.textContent = "No dated usage to chart.";
      container.appendChild(empty);
      return;
    }

    // The grid ends on the current week and spans WEEKS columns back to a Sunday.
    var now = new Date();
    var end = utcDay(now.getFullYear(), now.getMonth(), now.getDate());
    var endDow = new Date(end).getUTCDay();
    var start = end - endDow * DAY_MS - (WEEKS - 1) * 7 * DAY_MS;

    // Peak metric value across the visible window sets the ramp ceiling.
    var max = 0;
    for (var c = 0; c < WEEKS; c++) {
      for (var r = 0; r < 7; r++) {
        var v = valueFor(index[keyOf(start + (c * 7 + r) * DAY_MS)], metric);
        if (v > max) max = v;
      }
    }

    var W = container.clientWidth || 720;
    var gap = 3, labelH = 16, padT = 2;
    var cell = Math.max(7, Math.floor((W - (WEEKS - 1) * gap) / WEEKS));
    var gridW = WEEKS * cell + (WEEKS - 1) * gap;
    var gridH = 7 * cell + 6 * gap;
    var H = padT + gridH + labelH;

    var svg = svgEl("svg", { viewBox: "0 0 " + gridW + " " + H, width: gridW, height: H, role: "img", "aria-label": "Daily activity" });

    var tooltip = document.createElement("div");
    tooltip.className = "heatmap-tooltip";
    tooltip.setAttribute("aria-hidden", "true");

    function show(ts, rec, cx, cy) {
      var total = (rec ? (rec.input || 0) + (rec.output || 0) + (rec.cacheRead || 0) + (rec.cacheWrite || 0) : 0);
      var html = '<div class="tt-day">' + fmtFullDay(ts) + "</div>";
      if (!rec || total === 0) {
        html += '<div class="tt-empty">No activity</div>';
      } else {
        html += '<div class="tt-total">' + fmtTok(total) + " tokens</div>";
        html += '<dl class="tt-grid">' +
          '<dt>In</dt><dd>' + fmtTok(rec.input || 0) + "</dd>" +
          '<dt>Out</dt><dd>' + fmtTok(rec.output || 0) + "</dd>" +
          '<dt>Cache read</dt><dd>' + fmtTok(rec.cacheRead || 0) + "</dd>" +
          '<dt>Cache write</dt><dd>' + fmtTok(rec.cacheWrite || 0) + "</dd>" +
          "</dl>";
        html += '<div class="tt-cost">' + fmtCost(rec.cost || 0) + "</div>";
      }
      tooltip.innerHTML = html;
      tooltip.classList.add("on");
      // Center above the cell, clamped to the container; flip below near the top.
      var tw = tooltip.offsetWidth, th = tooltip.offsetHeight;
      var left = cx + cell / 2 - tw / 2;
      left = Math.max(0, Math.min(left, container.clientWidth - tw));
      var top = cy - th - 8;
      if (top < 0) top = cy + cell + 8;
      tooltip.style.left = left + "px";
      tooltip.style.top = top + "px";
    }
    function hide() { tooltip.classList.remove("on"); }

    for (var col = 0; col < WEEKS; col++) {
      for (var row = 0; row < 7; row++) {
        var ts = start + (col * 7 + row) * DAY_MS;
        if (ts > end) continue; // future days in the current week stay blank
        var rec = index[keyOf(ts)];
        var lvl = levelFor(valueFor(rec, metric), max);
        var x = col * (cell + gap), y = padT + row * (cell + gap);
        var rect = svgEl("rect", {
          class: "hm-cell lvl-" + lvl, x: x, y: y, width: cell, height: cell,
          rx: 2, ry: 2,
        });
        (function (ts, rec, x, y) {
          rect.addEventListener("mouseenter", function () { show(ts, rec, x, y); });
        })(ts, rec, x, y);
        svg.appendChild(rect);
      }
    }

    // Month labels along the bottom, placed at the column where a month begins.
    var lastMonth = -1, lastLabelCol = -2;
    for (var mc = 0; mc < WEEKS; mc++) {
      var weekStart = start + mc * 7 * DAY_MS;
      var mo = new Date(weekStart).getUTCMonth();
      if (mo !== lastMonth && mc - lastLabelCol >= 3) {
        var tx = svgEl("text", { class: "hm-month", x: mc * (cell + gap), y: H - 4 });
        tx.textContent = MONTHS[mo];
        svg.appendChild(tx);
        lastLabelCol = mc;
      }
      lastMonth = mo;
    }

    svg.addEventListener("mouseleave", hide);
    container.appendChild(svg);
    container.appendChild(tooltip);
  }

  function initHeatmap(container) {
    var raw = container.getAttribute("data-series");
    if (!raw) return;
    var data;
    try { data = JSON.parse(raw); } catch (e) { return; }
    var index = buildIndex(data);
    var metric = "tokens";
    var id = container.id;

    function draw() { renderHeatmap(container, index, metric); }
    draw();

    Array.prototype.slice.call(document.querySelectorAll('.seg[data-heatmap-target="' + id + '"]')).forEach(function (btn) {
      btn.addEventListener("click", function () {
        metric = btn.getAttribute("data-metric");
        document.querySelectorAll('.seg[data-heatmap-target="' + id + '"]').forEach(function (b) { b.classList.toggle("active", b === btn); });
        draw();
      });
    });

    var raf = 0;
    window.addEventListener("resize", function () {
      if (raf) cancelAnimationFrame(raf);
      raf = requestAnimationFrame(draw);
    });
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
    Array.prototype.slice.call(document.querySelectorAll("[data-heatmap]")).forEach(initHeatmap);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
