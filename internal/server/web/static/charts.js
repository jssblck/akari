// akari charts: a small, dependency-free SVG renderer for the usage activity
// grid. It hydrates any [data-heatmap] container from the inline daily JSON the
// server embeds, draws a GitHub-style calendar of daily intensity on the dark
// ground, and lifts a mono tooltip on hover. No build step, no external library;
// the binary stays self-contained. See DESIGN.md (Charts) for the visual contract.
(function () {
  "use strict";
  var NS = "http://www.w3.org/2000/svg";
  var MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  function svgEl(name, attrs) {
    var e = document.createElementNS(NS, name);
    if (attrs) for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  // fmtCost mirrors web.FmtCost so the tooltip's dollar figure reads identically
  // to every server-rendered cost on the page, at any magnitude.
  function fmtCost(v) {
    if (v === 0) return "$0";
    if (v < 0.01) return "$" + v.toFixed(4);
    return "$" + v.toFixed(2);
  }
  function fmtTok(v) {
    if (v >= 1e9) return (v / 1e9).toFixed(1) + "B";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
    if (v >= 1e3) return (v / 1e3).toFixed(1) + "k";
    return String(v);
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
    // Floor the cell at a size a finger can still tell apart. When the floor
    // binds (a phone can't fit a year of readable cells), the grid keeps its
    // natural width and the container pans instead (.hm-scroll, overview.css),
    // parked on the most recent weeks after render.
    var cell = Math.max(9, Math.floor((W - (WEEKS - 1) * gap) / WEEKS));
    var gridW = WEEKS * cell + (WEEKS - 1) * gap;
    var scrolls = gridW > W;
    container.classList.toggle("hm-scroll", scrolls);
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
      // In a panning grid the tooltip positions against the scrolled content,
      // so clamp to the content width, not the (narrower) scrollport.
      left = Math.max(0, Math.min(left, (scrolls ? container.scrollWidth : container.clientWidth) - tw));
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
    // Park the scroll at the trailing edge, where the most recent weeks are.
    if (scrolls) container.scrollLeft = container.scrollWidth;
  }

  function initHeatmap(container) {
    if (container._hydrated) return; // re-scans (e.g. after an htmx swap) skip live grids
    container._hydrated = true;
    var raw = container.getAttribute("data-series");
    if (!raw) return;
    var data;
    try { data = JSON.parse(raw); } catch (e) { return; }
    var index = buildIndex(data);
    var metric = "tokens";
    var id = container.id;

    function draw() { renderHeatmap(container, index, metric); }
    // Stash the redraw on the element so the single module-level resize handler
    // (registered once, below) can find every live grid by querying the DOM,
    // rather than each initHeatmap adding its own window listener. The usage panel
    // is swapped whole on a range/user change, so a per-call listener would stack
    // one leaked closure (pinning a detached SVG) on every swap; keeping the
    // handler global and resolving live grids by query avoids that, matching the
    // single-handler convention initOutlineSpy uses in app.js.
    container._redraw = draw;
    draw();

    Array.prototype.slice.call(document.querySelectorAll('.seg[data-heatmap-target="' + id + '"]')).forEach(function (btn) {
      btn.addEventListener("click", function () {
        metric = btn.getAttribute("data-metric");
        document.querySelectorAll('.seg[data-heatmap-target="' + id + '"]').forEach(function (b) {
          b.classList.toggle("active", b === btn);
          b.setAttribute("aria-pressed", b === btn ? "true" : "false");
        });
        draw();
      });
    });
  }

  // One resize handler for the whole document: on resize it redraws each grid
  // currently in the DOM via the _redraw stashed on it. A grid detached by an
  // htmx swap is no longer found, so its closure is not retained and never
  // redrawn; the fresh grid that replaced it carries its own _redraw.
  var resizeRaf = 0;
  window.addEventListener("resize", function () {
    if (resizeRaf) cancelAnimationFrame(resizeRaf);
    resizeRaf = requestAnimationFrame(function () {
      Array.prototype.slice.call(document.querySelectorAll("[data-heatmap]")).forEach(function (el) {
        if (typeof el._redraw === "function") el._redraw();
      });
    });
  });

  function init() {
    Array.prototype.slice.call(document.querySelectorAll("[data-heatmap]")).forEach(initHeatmap);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
  // A control swap brings a fresh, unhydrated heatmap: the overview's range/user
  // controls swap #usage; the project page swaps the larger #project-view (panel
  // plus session table) so both re-scope to one filter at once. Hydrate the grids
  // under whichever of those swapped, and only those: gating on the swapped target's
  // id by an O(1) check keeps live transcript appends (which swap #session-body on
  // every SSE update) from scanning a growing document. The _hydrated guard skips
  // any grid that survived the swap.
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = (e.detail && e.detail.target) || e.target;
    if (!t || (t.id !== "usage" && t.id !== "project-view")) return;
    // Read the live node by id, not the event's target: an outerHTML swap reports
    // the detached old node, whose subtree is no longer in the document.
    var root = document.getElementById(t.id);
    if (!root) return;
    Array.prototype.slice.call(root.querySelectorAll("[data-heatmap]")).forEach(initHeatmap);
  });
})();
