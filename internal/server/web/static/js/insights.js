/* ============================================================
   akari insights: chart engine, ported near-verbatim from the
   concept mock. Vanilla JS, no libraries, no build step.

   Data contract: window.AK_DATA is parsed from the JSON the server
   embeds in #insights-data (see runInsights() at the bottom). Every
   time-series field shares one bucket grid: AK_DATA.nBuckets entries,
   index 0 = oldest, AK_DATA.bucketUnit is "day" or "week", and
   AK_DATA.bucketLabels[i] is the axis/tooltip label for bucket i.
   The mock's seeded-data generator and its separate 26-week/90-day
   grids are gone; see the port brief for the full field list.
   ============================================================ */
(function () {
  'use strict';

  const NS = 'http://www.w3.org/2000/svg';
  function svgEl(tag, attrs) {
    const el = document.createElementNS(NS, tag);
    if (attrs) for (const k in attrs) el.setAttribute(k, attrs[k]);
    return el;
  }
  function svgRoot(viewBoxW, viewBoxH, extraClass) {
    const svg = svgEl('svg', {
      viewBox: '0 0 ' + viewBoxW + ' ' + viewBoxH,
      class: 'chart-svg' + (extraClass ? ' ' + extraClass : ''),
      preserveAspectRatio: 'none',
    });
    return svg;
  }

  // clipGroup returns a <g> whose children are clipped to the plot rectangle
  // [x, y, w, h]. Value-driven marks (scatter dots, line and area paths, bars)
  // go in this group, so a datum beyond the axis domain paints up to the plot
  // edge and no further, never bleeding into the axis margins. Axis chrome
  // (ticks, gridlines, callout labels, annotations) is appended to the svg
  // directly, not this group, so labels that live in the margins stay visible.
  // Each call mints a unique clipPath id, since one page holds many charts.
  let clipSeq = 0;
  function clipGroup(svg, x, y, w, h) {
    const id = 'ak-clip-' + (++clipSeq);
    const cp = svgEl('clipPath', { id: id });
    cp.appendChild(svgEl('rect', { x: x, y: y, width: Math.max(0, w), height: Math.max(0, h) }));
    svg.appendChild(cp);
    const g = svgEl('g', { 'clip-path': 'url(#' + id + ')' });
    svg.appendChild(g);
    return g;
  }

  /* linear scale factory */
  function scaleLinear(domain, range) {
    const [d0, d1] = domain, [r0, r1] = range;
    return function (v) {
      if (d1 === d0) return r0;
      return r0 + ((v - d0) / (d1 - d0)) * (r1 - r0);
    };
  }
  function scaleLog(domain, range) {
    const [d0, d1] = domain, [r0, r1] = range;
    const ld0 = Math.log10(Math.max(d0, 1e-6)), ld1 = Math.log10(d1);
    return function (v) {
      const lv = Math.log10(Math.max(v, 1e-6));
      return r0 + ((lv - ld0) / (ld1 - ld0)) * (r1 - r0);
    };
  }

  function pathLine(points) {
    // points: [[x,y], ...]
    if (!points.length) return '';
    return points.map((p, i) => (i === 0 ? 'M' : 'L') + p[0].toFixed(2) + ',' + p[1].toFixed(2)).join(' ');
  }
  function pathArea(points, baseline) {
    if (!points.length) return '';
    const top = pathLine(points);
    const last = points[points.length - 1];
    const first = points[0];
    return top + ' L' + last[0].toFixed(2) + ',' + baseline.toFixed(2) +
      ' L' + first[0].toFixed(2) + ',' + baseline.toFixed(2) + ' Z';
  }
  function pathBand(topPoints, bottomPoints) {
    // shaded band between two series (bottom drawn reversed to close the shape)
    const top = pathLine(topPoints);
    const rev = bottomPoints.slice().reverse();
    const bottom = rev.map((p, i) => 'L' + p[0].toFixed(2) + ',' + p[1].toFixed(2)).join(' ');
    return top + ' ' + bottom + ' Z';
  }
  function pathStep(points, baseline) {
    // step-area: horizontal-then-vertical steps, good for daily counts
    if (!points.length) return '';
    let d = 'M' + points[0][0].toFixed(2) + ',' + baseline.toFixed(2);
    d += ' L' + points[0][0].toFixed(2) + ',' + points[0][1].toFixed(2);
    for (let i = 1; i < points.length; i++) {
      const prev = points[i - 1], cur = points[i];
      const midX = (prev[0] + cur[0]) / 2;
      d += ' L' + midX.toFixed(2) + ',' + prev[1].toFixed(2);
      d += ' L' + midX.toFixed(2) + ',' + cur[1].toFixed(2);
      d += ' L' + cur[0].toFixed(2) + ',' + cur[1].toFixed(2);
    }
    const last = points[points.length - 1];
    d += ' L' + last[0].toFixed(2) + ',' + baseline.toFixed(2) + ' Z';
    return d;
  }

  function axisTicksY(svg, values, xLeft, xRight, yScale, fmt) {
    values.forEach((v) => {
      const y = yScale(v);
      svg.appendChild(svgEl('line', {
        x1: xLeft, x2: xRight, y1: y, y2: y, class: 'gridline',
      }));
      const t = svgEl('text', { x: xLeft - 6, y: y + 3, class: 'axis-tick-text', 'text-anchor': 'end' });
      t.textContent = fmt ? fmt(v) : String(v);
      svg.appendChild(t);
    });
  }
  function axisBaseline(svg, x1, x2, y) {
    svg.appendChild(svgEl('line', { x1, x2, y1: y, y2: y, class: 'axis-line' }));
  }

  // bucketAxis draws the shared x-axis (first/middle/last tick) for every
  // time-series chart, reading its label from AK_DATA.bucketLabels: the
  // server already resolved day-vs-week framing and real calendar dates for
  // the selected range, so the client only ever indexes into that array.
  function bucketAxis(svg, w, h, pB, pL, pR, mini) {
    const D = window.AK_DATA;
    const y = h - pB + (mini ? 14 : 17);
    const n = D.nBuckets;
    const marks = [0, Math.floor((n - 1) / 2), n - 1];
    const xScale = scaleLinear([0, n - 1], [pL, w - pR]);
    marks.forEach((i) => {
      const x = xScale(i);
      const t = svgEl('text', { x, y, class: 'axis-tick-text', 'text-anchor': i === 0 ? 'start' : (i === n - 1 ? 'end' : 'middle') });
      t.textContent = D.bucketLabels[i];
      if (!mini) svg.appendChild(t);
    });
  }

  function fmtInt(n) { return Math.round(n).toLocaleString('en-US'); }
  function fmtK(n) {
    if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1).replace(/\.0$/, '') + 'k';
    return String(Math.round(n));
  }
  function fmtPct(n, d) { return n.toFixed(d == null ? 0 : d) + '%'; }
  function fmtS(n) { return Math.round(n) + 's'; }

  /* ---------- tooltip ---------- */
  function showTooltip(x, y, html) {
    const tooltipEl = document.getElementById('tooltip');
    if (!tooltipEl) return;
    tooltipEl.innerHTML = html;
    tooltipEl.style.display = 'block';
    const pad = 14;
    let left = x + pad, top = y + pad;
    const rect = tooltipEl.getBoundingClientRect();
    if (left + rect.width > window.innerWidth - 8) left = x - rect.width - pad;
    if (top + rect.height > window.innerHeight - 8) top = y - rect.height - pad;
    tooltipEl.style.left = left + 'px';
    tooltipEl.style.top = top + 'px';
  }
  function hideTooltip() {
    const tooltipEl = document.getElementById('tooltip');
    if (tooltipEl) tooltipEl.style.display = 'none';
  }

  /* ---------- shared weekly/bucket hover crosshair ----------
     shared by every time-series chart: a vertical crosshair that snaps to
     the nearest bucket index and hands that index to htmlFn to build the
     tooltip body. */
  function attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, htmlFn) {
    const D = window.AK_DATA;
    const cross = svgEl('line', { x1: 0, x2: 0, y1: pT, y2: h - pB, stroke: 'var(--border-strong)', 'stroke-width': 1, style: 'display:none' });
    svg.appendChild(cross);
    svg.addEventListener('mousemove', (e) => {
      const rect = svg.getBoundingClientRect();
      const px = ((e.clientX - rect.left) / rect.width) * w;
      const i = Math.round(scaleLinear([pL, w - pR], [0, D.nBuckets - 1])(px));
      const clampI = Math.max(0, Math.min(D.nBuckets - 1, i));
      const x = xScale(clampI);
      cross.setAttribute('x1', x); cross.setAttribute('x2', x); cross.style.display = 'block';
      showTooltip(e.clientX, e.clientY, htmlFn(clampI));
    });
    svg.addEventListener('mouseleave', () => { cross.style.display = 'none'; hideTooltip(); });
  }

  /* ---------- right-edge label collision resolution ----------
     shared by every chart that stacks value labels on the right edge
     (Fleet mix, Throughput, Failures, Hygiene, Subagents): sort by y,
     then push any pair closer than minGap apart symmetrically, and
     clamp the whole resolved set back inside [top, bottom]. Items
     are plain objects with a numeric y; this mutates and returns y
     only, so callers keep their own label/color/text alongside it. */
  function resolveLabelCollisions(items, minGap, top, bottom) {
    const sorted = items.slice().sort((a, b) => a.y - b.y);
    for (let pass = 0; pass < sorted.length; pass++) {
      let moved = false;
      for (let i = 1; i < sorted.length; i++) {
        const gap = sorted[i].y - sorted[i - 1].y;
        if (gap < minGap) {
          const push = (minGap - gap) / 2;
          sorted[i - 1].y -= push;
          sorted[i].y += push;
          moved = true;
        }
      }
      if (!moved) break;
    }
    if (sorted.length) {
      if (sorted[0].y < top) {
        const d = top - sorted[0].y;
        sorted.forEach((s) => { s.y += d; });
      }
      if (sorted[sorted.length - 1].y > bottom) {
        const d = sorted[sorted.length - 1].y - bottom;
        sorted.forEach((s) => { s.y -= d; });
      }
    }
    return sorted;
  }

  /* expose a tiny namespace for the rest of the scripts */
  window.AK = {
    svgEl, svgRoot, clipGroup, scaleLinear, scaleLog,
    pathLine, pathArea, pathBand, pathStep,
    axisTicksY, axisBaseline, bucketAxis, attachHoverBucket,
    fmtInt, fmtK, fmtPct, fmtS,
    showTooltip, hideTooltip,
    resolveLabelCollisions,
  };
})();


/* ============================================================
   Fleet mix: stacked area of token share by model, one bucket
   per column.
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;

  function chartFleetMix() {
    const D = window.AK_DATA;
    const w = 1000, h = 380, pL = 34, pR = 130, pT = 14, pB = 24;
    const svg = A.svgRoot(w, h);
    const M = D.fleetMix;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 100], [h - pB, pT]);

    A.axisTicksY(svg, [0, 25, 50, 75, 100], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    // model-arrival marker: only drawn when the server flags an arrival
    // bucket for the window in view (a model that predates the window has
    // no marker to draw).
    if (typeof M.arrivalWeek === 'number' && M.arrivalWeek >= 0) {
      const arrivalX = xScale(M.arrivalWeek);
      svg.appendChild(A.svgEl('line', { x1: arrivalX, x2: arrivalX, y1: pT, y2: h - pB, stroke: 'var(--faint)', 'stroke-width': 1, 'stroke-dasharray': '2,3' }));
    }

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    let cum = new Array(D.nBuckets).fill(0);
    M.order.forEach((key) => {
      const bottom = cum.slice();
      const top = cum.map((c, i) => c + M.rows[i][key]);
      const bottomPts = bottom.map((v, i) => [xScale(i), yScale(v)]);
      const topPts = top.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathBand(topPts, bottomPts), fill: M.colors[key], opacity: '0.85' }));
      cum = top;
    });

    A.axisBaseline(svg, pL, w - pR, h - pB);

    // right-edge share labels for the current bucket
    const last = M.rows[D.nBuckets - 1];
    let acc = 0;
    const pendingLabels = [];
    M.order.forEach((key) => {
      const mid = acc + last[key] / 2;
      acc += last[key];
      if (last[key] < 3) return;
      pendingLabels.push({ y: yScale(100 - mid), color: M.colors[key], text: M.labels[key] + ' ' + last[key].toFixed(0) + '%' });
    });
    A.resolveLabelCollisions(pendingLabels, 14, pT, h - pB).forEach((lbl) => {
      const t = A.svgEl('text', { x: w - pR + 8, y: lbl.y + 3, class: 'axis-tick-text', fill: lbl.color });
      t.setAttribute('font-family', 'var(--mono)');
      t.setAttribute('font-size', '11');
      t.textContent = lbl.text;
      svg.appendChild(t);
    });

    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      M.order.forEach((key) => {
        html += '<div class="tt-row" style="color:' + M.colors[key] + '">' + M.labels[key] + ' <b>' + M.rows[i][key].toFixed(1) + '%</b></div>';
      });
      return html;
    });

    return svg;
  }

  function renderFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('fleetmix-figures');
    if (!el) return;
    el.innerHTML = '';
    const M = D.fleetMix;
    const last = M.rows[D.nBuckets - 1];
    const busiest = M.order.reduce((a, b) => (last[b] > last[a] ? b : a));
    const figs = [
      { v: String(M.order.length), k: 'models in window' },
      { v: M.labels[busiest], k: 'busiest model · ' + last[busiest].toFixed(0) + '% share' },
    ];
    if (M.newestArrivalLabel) {
      figs.push({ v: M.newestArrivalLabel, k: 'newest arrival' + (M.newestArrivalDate ? ' · first seen ' + M.newestArrivalDate : '') });
    }
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function renderLegend() {
    const D = window.AK_DATA;
    const el = document.getElementById('fleetmix-legend');
    if (!el) return;
    el.innerHTML = '';
    D.fleetMix.order.forEach((key) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + D.fleetMix.colors[key] + '"></span>' + D.fleetMix.labels[key];
      el.appendChild(li);
    });
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function renderFleetMix() {
    renderFigures();
    mount('chart-fleetmix-full', chartFleetMix());
    renderLegend();
  }

  window.AK_FLEETMIX = { renderFleetMix };
})();


/* ============================================================
   Session gallery: scatter of sessions, duration x cost,
   colored by archetype, with annotated outliers.
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;

  const ARCH_LABEL = { quick: 'Quick', standard: 'Standard', deep: 'Deep', marathon: 'Marathon', automation: 'Automation' };

  function fmtDuration(s) {
    if (s < 90) return Math.round(s) + 's';
    if (s < 3600) return (s / 60).toFixed(0) + 'm';
    return (s / 3600).toFixed(1) + 'h';
  }
  // A trailing '+' marks a lower-bound cost, matching the server's FmtCost: the figure folded a
  // token-bearing usage event the pricing table could not price, so the true cost is higher.
  function fmtCost(v, incomplete) { return '$' + v.toFixed(2) + (incomplete ? '+' : ''); }

  function chartGallery() {
    const D = window.AK_DATA;
    const w = 1000, h = 380, pL = 46, pR = 24, pT = 16, pB = 30;
    const svg = A.svgRoot(w, h);
    const G = D.sessionGallery;
    // Fit the log axes to the data so the outliers (the priciest session, the
    // longest run) sit inside the plot instead of painting past the axis. The
    // gallery exists to stop outliers hiding, so clipping them away would defeat
    // it; the fixed defaults still hold when the data stays inside them.
    const durs = G.points.map((p) => p.durationS);
    const costs = G.points.map((p) => p.costUsd);
    (G.annotations || []).forEach((a) => { durs.push(a.durationS); costs.push(a.costUsd); });
    const xLo = durs.length ? Math.max(1, Math.min(30, Math.min(...durs))) : 30;
    const xHi = durs.length ? Math.max(86400, Math.max(...durs) * 1.05) : 86400;
    const yLo = costs.length ? Math.max(0.001, Math.min(0.01, Math.min(...costs))) : 0.01;
    const yHi = costs.length ? Math.max(60, Math.max(...costs) * 1.08) : 60;
    const xScale = A.scaleLog([xLo, xHi], [pL, w - pR]);
    const yScale = A.scaleLog([yLo, yHi], [h - pB, pT]);

    A.axisTicksY(svg, [0.01, 0.1, 1, 10, 60], pL, w - pR, yScale, (v) => (v < 1 ? '$' + v.toFixed(2) : '$' + v));
    [30, 300, 3600, 43200, 86400].forEach((v) => {
      const x = xScale(v);
      svg.appendChild(A.svgEl('line', { x1: x, x2: x, y1: pT, y2: h - pB, class: 'gridline' }));
      const t = A.svgEl('text', { x, y: h - pB + 15, class: 'axis-tick-text', 'text-anchor': 'middle' });
      t.textContent = fmtDuration(v);
      svg.appendChild(t);
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);

    const dotsLayer = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    G.points.forEach((p) => {
      // Fitting covers the high-end outliers, but a near-zero duration or cost
      // underflows the log axis and would fly off the left/bottom. Pin those to
      // the plot edge so every session stays visible instead of clipped away.
      const cx = Math.max(pL, Math.min(w - pR, xScale(p.durationS)));
      const cy = Math.max(pT, Math.min(h - pB, yScale(p.costUsd)));
      const dot = A.svgEl('circle', {
        cx, cy, r: 3.4, fill: G.archColor[p.arch], opacity: '0.7', class: 'scatter-dot',
      });
      dotsLayer.appendChild(dot);
      dot.addEventListener('mousemove', (e) => {
        const html = '<div class="tt-title">' + ARCH_LABEL[p.arch] + '</div>' +
          '<div class="tt-row">duration <b>' + fmtDuration(p.durationS) + '</b></div>' +
          '<div class="tt-row">cost <b>' + fmtCost(p.costUsd, p.costIncomplete) + '</b></div>' +
          '<div class="tt-row">grade <b>' + p.grade + '</b></div>' +
          '<div class="tt-row">outcome <b>' + p.outcome + '</b></div>';
        A.showTooltip(e.clientX, e.clientY, html);
      });
      dot.addEventListener('mouseleave', A.hideTooltip);
    });

    // annotated outliers: only drawn when the server hand-selects sessions
    // worth calling out; an empty/missing list means no callouts.
    (G.annotations || []).forEach((ann) => {
      annotate(svg, xScale(ann.durationS), yScale(ann.costUsd), ann.label, ann.corner);
    });

    return svg;

    function annotate(svg, x, y, label, corner) {
      const dx = corner === 'top-right' ? 70 : -70;
      const dy = corner === 'top-right' ? -34 : 34;
      const lx = x + dx, ly = y + dy;
      svg.appendChild(A.svgEl('line', { x1: x, y1: y, x2: lx, y2: ly, stroke: 'var(--subtext)', 'stroke-width': 1 }));
      const t = A.svgEl('text', {
        x: corner === 'top-right' ? lx - 4 : lx + 4, y: ly, class: 'callout-label',
        'text-anchor': corner === 'top-right' ? 'end' : 'start',
      });
      t.textContent = label;
      svg.appendChild(t);
    }
  }

  function renderFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('gallery-figures');
    if (!el) return;
    el.innerHTML = '';
    const G = D.sessionGallery;
    const figs = [
      { v: fmtDuration(G.medianDurationS), k: 'median duration' },
      { v: fmtCost(G.medianCostUsd, G.costIncomplete), k: 'median cost' },
      { v: fmtCost(G.priciest.costUsd, G.costIncomplete), k: 'priciest session · ' + fmtDuration(G.priciest.durationS) },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  // The scatter caps at the most recent maxGalleryPoints, but the figures above read the full
  // cohort. When the window holds more sessions than the scatter shows, say so, so a reader does
  // not take the dots for the whole population or hunt for a priciest session that was sampled out.
  function renderSampleNote() {
    const D = window.AK_DATA;
    const el = document.getElementById('gallery-sample');
    if (!el) return;
    const G = D.sessionGallery;
    el.textContent = G.shown < G.total
      ? 'Scatter shows the ' + A.fmtInt(G.shown) + ' most recent of ' + A.fmtInt(G.total) + ' sessions; the figures cover all ' + A.fmtInt(G.total) + '.'
      : '';
  }

  function renderLegend() {
    const D = window.AK_DATA;
    const el = document.getElementById('gallery-legend');
    if (!el) return;
    el.innerHTML = '';
    Object.keys(ARCH_LABEL).forEach((key) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + D.sessionGallery.archColor[key] + '"></span>' + ARCH_LABEL[key];
      el.appendChild(li);
    });
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function renderGallery() {
    renderFigures();
    mount('chart-gallery-full', chartGallery());
    renderSampleNote();
    renderLegend();
  }

  window.AK_GALLERY = { renderGallery };
})();


/* ============================================================
   Velocity: agent hours, response time, throughput, rhythm
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;
  const W = 1000, H = 380, padL = 40, padR = 14, padT = 14, padB = 26;
  const MW = 420, MH = 210, mpadL = 28, mpadR = 8, mpadT = 8, mpadB = 18;

  function clampX(x, w, pR, labelW) { return Math.min(x, w - pR - labelW); }

  function chartActiveHours(mini) {
    const D = window.AK_DATA;
    const w = mini ? MW : W, h = mini ? MH : H;
    const pL = mini ? mpadL : padL, pR = mini ? mpadR : padR, pT = mini ? mpadT : padT, pB = mini ? mpadB : padB;
    const svg = A.svgRoot(w, h);
    const H_ = D.activeHours;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const maxV = Math.max(...H_.wallSpan) * 1.1;
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, Math.round(maxV)] : [0, 15, 30, 45], pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const wallPts = H_.wallSpan.map((v, i) => [xScale(i), yScale(v)]);

    const bw = (w - pL - pR) / D.nBuckets;
    H_.active.forEach((v, i) => {
      const x = xScale(i) - bw * 0.32;
      const y = yScale(v);
      clip.appendChild(A.svgEl('rect', { x, y, width: bw * 0.64, height: (h - pB) - y, fill: 'var(--viz-8)', opacity: '0.78' }));
    });
    clip.appendChild(A.svgEl('path', { d: A.pathLine(wallPts), fill: 'none', stroke: 'var(--muted)', 'stroke-width': mini ? 1 : 1.4, 'stroke-dasharray': '3,3' }));

    if (!mini && D.activeHours.maxLabel != null && D.activeHours.maxIdx != null) {
      const mx = xScale(H_.maxIdx), my = yScale(H_.active[H_.maxIdx]);
      svg.appendChild(A.svgEl('circle', { cx: mx, cy: my, r: 3.2, fill: 'var(--viz-8)', stroke: 'var(--bg)', 'stroke-width': 1.5 }));
      const t = A.svgEl('text', { x: clampX(mx + 8, w, pR, 150), y: my - 10, class: 'callout-label' });
      t.textContent = D.activeHours.maxLabel;
      svg.appendChild(t);
    }

    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--viz-8)">active h <b>' + H_.active[i].toFixed(1) + '</b></div>' +
        '<div class="tt-row" style="color:var(--muted)">wall-clock span h <b>' + H_.wallSpan[i].toFixed(1) + '</b></div>';
    });
    return svg;
  }

  function chartResponseTime(mini) {
    const D = window.AK_DATA;
    const w = mini ? MW : W, h = mini ? MH : H;
    const pL = mini ? mpadL : padL, pR = mini ? mpadR : (padR + 70), pT = mini ? mpadT : padT, pB = mini ? mpadB : padB;
    const svg = A.svgRoot(w, h);
    const RT = D.responseTime;
    // Fit the ceiling to the tallest series drawn (p99 on the full chart, the
    // p50/p90 band on the mini) so a latency spike stays inside the plot instead
    // of painting over the top; the original fixed ceilings hold as the floor.
    const drawn = mini ? RT.p50.concat(RT.p90) : RT.p50.concat(RT.p90, RT.p99);
    const dataMax = drawn.length ? Math.max(...drawn) : 0;
    const yMax = Math.max(mini ? 40 : 120, Math.ceil(dataMax * 1.1 / 10) * 10);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, yMax], [h - pB, pT]);

    const yticks = mini ? [0, yMax] : [0, yMax * 0.25, yMax * 0.5, yMax * 0.75, yMax].map((v) => Math.round(v));
    A.axisTicksY(svg, yticks, pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const p50Pts = RT.p50.map((v, i) => [xScale(i), yScale(v)]);
    const p90Pts = RT.p90.map((v, i) => [xScale(i), yScale(v)]);

    const band = A.svgEl('path', { d: A.pathBand(p90Pts, p50Pts), fill: 'var(--viz-2)', opacity: '0.16' });
    clip.appendChild(band);
    clip.appendChild(A.svgEl('path', { d: A.pathLine(p50Pts), fill: 'none', stroke: 'var(--viz-2)', 'stroke-width': mini ? 1.4 : 2 }));
    if (!mini) {
      clip.appendChild(A.svgEl('path', { d: A.pathLine(p90Pts), fill: 'none', stroke: 'var(--viz-2)', 'stroke-width': 1, opacity: '0.5', 'stroke-dasharray': '2,3' }));

      const p99Pts = RT.p99.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathLine(p99Pts), fill: 'none', stroke: 'var(--warn)', 'stroke-width': 1.4, 'stroke-dasharray': '4,3' }));
      const lastP99 = p99Pts[p99Pts.length - 1];
      const t99 = A.svgEl('text', { x: w - pR + 6, y: lastP99[1] + 3, class: 'callout-label', fill: 'var(--warn)' });
      t99.textContent = 'p99 ' + A.fmtS(RT.p99[RT.p99.length - 1]);
      svg.appendChild(t99);
    }
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--viz-2)">p50 <b>' + A.fmtS(D.responseTime.p50[i]) + '</b></div>' +
        '<div class="tt-row" style="color:var(--muted)">p90 <b>' + A.fmtS(D.responseTime.p90[i]) + '</b></div>' +
        '<div class="tt-row" style="color:var(--warn)">p99 <b>' + A.fmtS(D.responseTime.p99[i]) + '</b></div>';
    });
    return svg;
  }

  function chartThroughput(mini) {
    // Cadence, not raw token throughput: output-tokens-per-second would need the
    // model's generation duration, which the projection does not record, so this
    // draws the two densities we can actually derive, messages and tool calls per
    // active minute, over the shared bucket grid.
    const D = window.AK_DATA;
    const w = mini ? MW : W, h = mini ? MH : H;
    const pL = mini ? mpadL : padL, pR = mini ? mpadR : (padR + 90), pT = mini ? mpadT : padT, pB = mini ? mpadB : padB;
    const svg = A.svgRoot(w, h);
    const T = D.throughput;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const allVals = T.msgsPerMin.concat(T.toolsPerMin);
    const maxV = Math.max(1, Math.max(...allVals, 0)) * 1.15;
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, +maxV.toFixed(1)] : [0, +(maxV / 2).toFixed(1), +maxV.toFixed(1)], pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const series = [
      { key: 'msgsPerMin', label: 'msgs/min', color: 'var(--viz-2)', width: mini ? 1.4 : 2 },
      { key: 'toolsPerMin', label: 'tools/min', color: 'var(--viz-5)', width: mini ? 1.2 : 1.7 },
    ];
    const pendingLabels = [];
    series.forEach((s) => {
      const pts = T[s.key].map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: s.color, 'stroke-width': s.width }));
      if (!mini && pts.length) {
        const last = pts[pts.length - 1];
        pendingLabels.push({ y: last[1], color: s.color, text: s.label + ' ' + T[s.key][T[s.key].length - 1].toFixed(1) });
      }
    });
    if (!mini) {
      A.resolveLabelCollisions(pendingLabels, 14, pT, h - pB).forEach((lbl) => {
        const t = A.svgEl('text', { x: w - pR + 6, y: lbl.y + 3, class: 'callout-label', fill: lbl.color });
        t.textContent = lbl.text;
        svg.appendChild(t);
      });
    }
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      series.forEach((s) => {
        html += '<div class="tt-row" style="color:' + s.color + '">' + s.label + ' <b>' + T[s.key][i].toFixed(1) + '</b></div>';
      });
      return html;
    });
    return svg;
  }

  function chartPunchcard(mini) {
    const D = window.AK_DATA;
    const w = mini ? MW : 1000, h = mini ? MH : 320;
    const cols = 24, rows = 7;
    const pL = mini ? 22 : 44, pR = mini ? 4 : 14, pT = mini ? 4 : 10, pB = mini ? 14 : 26;
    const gap = mini ? 1 : 2;
    const cellW = (w - pL - pR) / cols;
    const cellH = (h - pT - pB) / rows;
    // uniform heatmap cells: identical fixed size (short side of the grid
    // step, minus the gap), intensity is background brightness only, never
    // cell area, so the reading is a conventional heatmap ramp.
    const cellSize = Math.min(cellW, cellH) - gap;
    const svg = A.svgRoot(w, h);
    const DOW = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

    let maxVol = 0;
    D.punchcard.forEach((row) => row.forEach((c) => { if (c.volume > maxVol) maxVol = c.volume; }));

    D.punchcard.forEach((row, r) => {
      if (!mini) {
        const rowLabel = A.svgEl('text', {
          x: pL - 8, y: pT + r * cellH + cellH / 2 + 3, class: 'punchcard-row-label', 'text-anchor': 'end',
        });
        rowLabel.textContent = DOW[r];
        svg.appendChild(rowLabel);
      }

      row.forEach((cell, c) => {
        const t = cell.volume / maxVol;
        const cx = pL + c * cellW + cellW / 2;
        const cy = pT + r * cellH + cellH / 2;
        const rect = A.svgEl('rect', {
          x: cx - cellSize / 2, y: cy - cellSize / 2, width: cellSize, height: cellSize,
          rx: mini ? 1 : 2, fill: mixColor(t), class: 'scatter-dot',
        });
        svg.appendChild(rect);
        if (!mini) {
          rect.addEventListener('mousemove', (e) => {
            const html = '<div class="tt-title">' + DOW[r] + ' ' + String(c).padStart(2, '0') + ':00</div>' +
              '<div class="tt-row">volume <b>' + A.fmtInt(cell.volume) + '</b></div>';
            A.showTooltip(e.clientX, e.clientY, html);
          });
          rect.addEventListener('mouseleave', A.hideTooltip);
        }
      });
    });

    if (!mini) {
      [0, 6, 12, 18, 23].forEach((c) => {
        const x = pL + c * cellW + cellW / 2;
        const t = A.svgEl('text', { x, y: h - pB + 16, class: 'axis-tick-text', 'text-anchor': 'middle' });
        t.textContent = String(c).padStart(2, '0') + ':00';
        svg.appendChild(t);
      });
    }

    return svg;

    function mixColor(t) {
      // surface-2 (#242228) at zero, lilac (#c6a8f2) at max; brightness is
      // the only channel that encodes intensity on this heatmap.
      const stops = [[0x24, 0x22, 0x28], [0xc6, 0xa8, 0xf2]];
      const c = stops[0].map((v, i) => Math.round(v + (stops[1][i] - v) * t));
      return 'rgb(' + c.join(',') + ')';
    }
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function miniMultiple(id, title, valueText, chartFn) {
    const el = document.getElementById(id);
    if (!el) return;
    el.innerHTML = '';
    const head = document.createElement('div');
    head.className = 'chart-caption';
    head.innerHTML = '<span class="chart-title">' + title + '</span><span class="chart-value mono">' + valueText + '</span>';
    el.appendChild(head);
    el.appendChild(chartFn(true));
  }

  function renderActiveHoursFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('activehours-figures');
    if (!el) return;
    el.innerHTML = '';
    const avgActive = D.activeHours.active.reduce((a, b) => a + b, 0) / D.activeHours.active.length;
    const figs = [
      { v: avgActive.toFixed(1) + 'h', k: 'avg active h/day' },
      { v: D.concurrency.peakConcurrent, k: 'peak concurrent sessions' },
      { v: D.concurrency.avgConcurrent.toFixed(1), k: 'avg concurrent sessions' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function renderThroughputFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('throughput-figures');
    if (!el) return;
    el.innerHTML = '';
    const T = D.throughput;
    // The canonical whole-window rates (total over total active minutes), not the mean of the
    // per-bucket rates, which drifts when buckets hold unequal active time.
    const figs = [
      { v: (T.msgsPerMinAvg || 0).toFixed(1), k: 'avg msgs/active min' },
      { v: (T.toolsPerMinAvg || 0).toFixed(1), k: 'avg tools/active min' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function renderVelocity() {
    const D = window.AK_DATA;
    renderActiveHoursFigures();
    mount('chart-activehours-full', chartActiveHours(false));
    mount('chart-response-full', chartResponseTime(false));
    renderThroughputFigures();
    mount('chart-throughput-full', chartThroughput(false));
    mount('chart-punchcard', chartPunchcard(false));

    const lastActive = D.activeHours.active[D.activeHours.active.length - 1];
    miniMultiple('mini-activehours', 'Agent hours', lastActive.toFixed(1) + ' h today', chartActiveHours);
    const lastP50 = D.responseTime.p50[D.responseTime.p50.length - 1];
    miniMultiple('mini-response', 'Response time', A.fmtS(lastP50) + ' p50', chartResponseTime);
    const msgsSeries = D.throughput.msgsPerMin;
    const lastMsgs = msgsSeries.length ? msgsSeries[msgsSeries.length - 1] : 0;
    miniMultiple('mini-throughput', 'Throughput', lastMsgs.toFixed(1) + ' msgs/min', chartThroughput);
    miniMultiple('mini-rhythm', 'Rhythm', D.punchcardPeakLabel || '', chartPunchcard);
  }

  window.AK_VELOCITY = { renderVelocity };
})();


/* ============================================================
   Tools: reliability, mix, failures, churn (trend + treemap)
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;
  const W = 1000, H = 380, mW = 420, mH = 210;

  // Tool categories are the parser's fixed vocabulary (internal/parser.toolCategory):
  // bash / edit / read / search / write, with the unclassified tail as other. The
  // reliability scatter reads these locally; the mix legend reads the identical map the
  // server embeds in toolMix.colors/labels, so the two frames stay in step.
  const CAT_COLOR = {
    bash: 'var(--viz-1)', edit: 'var(--viz-4)', read: 'var(--viz-2)',
    search: 'var(--viz-6)', write: 'var(--viz-3)', other: 'var(--viz-8)',
  };
  const CAT_LABEL = {
    bash: 'Shell', edit: 'Edit', read: 'Read', search: 'Search', write: 'Write', other: 'Other',
  };

  function chartReliability(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : 1000, h = mini ? mH : 380;
    const pL = mini ? 8 : 46, pR = mini ? 8 : 18, pT = mini ? 8 : 14, pB = mini ? 16 : 32;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLog([10, 100000], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 12], [h - pB, pT]);
    const maxSessions = Math.max(...D.allTools.map((t) => t.sessions));
    const rScale = (s) => (mini ? 1.4 : 2.6) + Math.sqrt(s / maxSessions) * (mini ? 9 : 20);

    if (!mini) {
      A.axisTicksY(svg, [0, 2, 4, 6, 8, 10, 12], pL, w - pR, yScale, (v) => v + '%');
      [10, 100, 1000, 10000, 100000].forEach((v) => {
        const x = xScale(v);
        svg.appendChild(A.svgEl('line', { x1: x, x2: x, y1: pT, y2: h - pB, class: 'gridline' }));
        const t = A.svgEl('text', { x, y: h - pB + 17, class: 'axis-tick-text', 'text-anchor': 'middle' });
        t.textContent = A.fmtK(v);
        svg.appendChild(t);
      });

      const refY = yScale(1);
      svg.appendChild(A.svgEl('line', { x1: pL, x2: w - pR, y1: refY, y2: refY, stroke: 'var(--faint)', 'stroke-width': 1, 'stroke-dasharray': '2,3' }));
      const refT = A.svgEl('text', { x: w - pR, y: refY - 5, class: 'axis-tick-text', 'text-anchor': 'end', fill: 'var(--faint)' });
      refT.textContent = '1% fleet rate';
      svg.appendChild(refT);
      A.axisBaseline(svg, pL, w - pR, h - pB);
    }

    const dotsLayer = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);

    const labeled = new Set(['PowerShell', 'shell_command', 'Edit', 'Bash']);

    D.allTools.forEach((tool) => {
      const cx = xScale(Math.max(tool.calls, 10));
      const cy = yScale(Math.min(tool.err, 12));
      const r = rScale(tool.sessions);
      const dot = A.svgEl('circle', {
        cx, cy, r, fill: CAT_COLOR[tool.cat], opacity: '0.75',
        stroke: 'var(--bg)', 'stroke-width': 1, class: 'scatter-dot',
      });
      dotsLayer.appendChild(dot);
      if (!mini) {
        dot.addEventListener('mousemove', (e) => {
          const html = '<div class="tt-title">' + tool.name + '</div>' +
            '<div class="tt-row">calls <b>' + A.fmtInt(tool.calls) + '</b></div>' +
            '<div class="tt-row">err <b>' + tool.err.toFixed(1) + '%</b></div>' +
            '<div class="tt-row">sessions <b>' + A.fmtInt(tool.sessions) + '</b></div>';
          A.showTooltip(e.clientX, e.clientY, html);
        });
        dot.addEventListener('mouseleave', A.hideTooltip);
      }

      if (!mini && labeled.has(tool.name)) {
        const t = A.svgEl('text', { x: cx + r + 6, y: cy + 4, class: 'scatter-label' });
        t.setAttribute('font-size', '11');
        t.textContent = tool.name;
        svg.appendChild(t);
      }
    });

    return svg;
  }

  function renderReliabilityLegend() {
    const el = document.getElementById('reliability-legend');
    if (!el) return;
    el.innerHTML = '';
    Object.keys(CAT_LABEL).forEach((cat) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + CAT_COLOR[cat] + '"></span>' + CAT_LABEL[cat];
      el.appendChild(li);
    });
  }

  function chartToolMix(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : W, h = mini ? mH : H;
    const pL = mini ? 26 : 40, pR = mini ? 8 : 16, pT = mini ? 8 : 14, pB = mini ? 16 : 26;
    const svg = A.svgRoot(w, h);
    const M = D.toolMix;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 100], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, 100] : [0, 25, 50, 75, 100], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    let cum = new Array(D.nBuckets).fill(0);
    M.order.forEach((key) => {
      const bottom = cum.slice();
      const top = cum.map((c, i) => c + M.rows[i][key]);
      const bottomPts = bottom.map((v, i) => [xScale(i), yScale(v)]);
      const topPts = top.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathBand(topPts, bottomPts), fill: M.colors[key], opacity: '0.82' }));
      cum = top;
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      M.order.forEach((key) => { html += '<div class="tt-row" style="color:' + M.colors[key] + '">' + M.labels[key] + ' <b>' + M.rows[i][key].toFixed(1) + '%</b></div>'; });
      return html;
    });

    return svg;
  }

  function renderToolMixLegend() {
    const D = window.AK_DATA;
    const el = document.getElementById('toolmix-legend');
    if (!el) return;
    el.innerHTML = '';
    D.toolMix.order.forEach((cat) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + D.toolMix.colors[cat] + '"></span>' + D.toolMix.labels[cat];
      el.appendChild(li);
    });
  }

  function chartFailures(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : W, h = mini ? mH : H;
    const pL = mini ? 26 : 40, pR = mini ? 8 : 96, pT = mini ? 8 : 14, pB = mini ? 16 : 26;
    const svg = A.svgRoot(w, h);
    const F = D.toolFailures;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    // The worst offenders are chosen server-side and vary by fleet, so the y axis
    // scales to the data rather than a fixed 15%: a clean fleet reads on a tight
    // axis instead of hugging the floor of a tall one.
    const worst = F.worst || [];
    const allVals = worst.reduce((acc, s) => acc.concat(s.rate), F.fleet.slice());
    const maxV = Math.max(5, Math.max(...allVals, 0) * 1.15);
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, Math.round(maxV)] : [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    // Named worst tools get warm hues; the fleet average is the heavy neutral line.
    const palette = ['var(--err)', 'var(--viz-4)', 'var(--viz-1)', 'var(--viz-6)'];
    const series = worst.map((s, i) => ({ rate: s.rate, label: s.name, color: palette[i % palette.length], width: mini ? 1 : 1.4 }));
    series.push({ rate: F.fleet, label: 'fleet', color: 'var(--text)', width: mini ? 1.4 : 2 });
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const pendingLabels = [];
    series.forEach((s) => {
      const pts = s.rate.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: s.color, 'stroke-width': s.width }));
      if (!mini && pts.length) {
        const last = pts[pts.length - 1];
        pendingLabels.push({ y: last[1], color: s.color, text: s.label + ' ' + s.rate[s.rate.length - 1].toFixed(1) + '%' });
      }
    });
    if (!mini) {
      A.resolveLabelCollisions(pendingLabels, 14, pT, h - pB).forEach((lbl) => {
        const t = A.svgEl('text', { x: w - pR + 6, y: lbl.y + 3, class: 'callout-label', fill: lbl.color });
        t.textContent = lbl.text;
        svg.appendChild(t);
      });
    }
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      series.forEach((s) => { html += '<div class="tt-row" style="color:' + s.color + '">' + s.label + ' <b>' + s.rate[i].toFixed(1) + '%</b></div>'; });
      return html;
    });

    return svg;
  }

  function chartChurnTrend(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : W, h = mini ? mH : 260;
    const pL = mini ? 26 : 40, pR = mini ? 8 : 16, pT = mini ? 8 : 14, pB = mini ? 16 : 26;
    const svg = A.svgRoot(w, h);
    const T = D.churnTrend;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    // Both series share this axis, so the ceiling covers whichever runs taller
    // (the hot-file count can top the re-edit count), keeping both lines in the plot.
    const maxV = Math.max(1, Math.max(...T.reedits, ...T.hotFiles)) * 1.15;
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, Math.round(maxV)] : [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const rePts = T.reedits.map((v, i) => [xScale(i), yScale(v)]);
    const hotPts = T.hotFiles.map((v, i) => [xScale(i), yScale(v)]);
    clip.appendChild(A.svgEl('path', { d: A.pathArea(rePts, yScale(0)), fill: 'var(--viz-3)', opacity: '0.16' }));
    clip.appendChild(A.svgEl('path', { d: A.pathLine(rePts), fill: 'none', stroke: 'var(--viz-3)', 'stroke-width': mini ? 1.4 : 2 }));
    clip.appendChild(A.svgEl('path', { d: A.pathLine(hotPts), fill: 'none', stroke: 'var(--muted)', 'stroke-width': mini ? 1 : 1.3, 'stroke-dasharray': '3,3' }));
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--viz-3)">re-edits <b>' + T.reedits[i] + '</b></div>' +
        '<div class="tt-row" style="color:var(--muted)">hot files <b>' + T.hotFiles[i] + '</b></div>';
    });

    return svg;
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function miniMultiple(id, title, valueText, chartFn) {
    const el = document.getElementById(id);
    if (!el) return;
    el.innerHTML = '';
    const head = document.createElement('div');
    head.className = 'chart-caption';
    head.innerHTML = '<span class="chart-title">' + title + '</span><span class="chart-value mono">' + valueText + '</span>';
    el.appendChild(head);
    el.appendChild(chartFn(true));
  }

  function renderChurnFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('churn-figures');
    if (!el) return;
    el.innerHTML = '';
    const busiest = D.churn.slice().sort((a, b) => b.edits - a.edits)[0];
    const figs = [
      { v: A.fmtInt(D.churnTrend.totalReedits), k: 'total re-edits' },
      { v: A.fmtInt(D.churnTrend.totalHotFiles), k: 'hot files' },
      { v: busiest.path.split('/').pop(), k: 'busiest file · ' + busiest.edits + ' edits' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
    // The totals count every hot file in the window, but the treemap caps at the busiest
    // maxChurnTreeFiles. When files were clipped, note the tail so the headline count does not
    // read as more files than the tree draws.
    const clipEl = document.getElementById('churn-clip');
    if (clipEl) {
      const c = D.churnTrend.clipped || 0;
      clipEl.textContent = c > 0
        ? '+' + A.fmtInt(c) + ' more hot file' + (c === 1 ? '' : 's') + ' beyond the treemap cap, counted in the totals above.'
        : '';
    }
  }

  function renderTools() {
    const D = window.AK_DATA;
    mount('chart-reliability', chartReliability());
    renderReliabilityLegend();
    mount('chart-toolmix-full', chartToolMix(false));
    renderToolMixLegend();
    mount('chart-failures-full', chartFailures(false));
    renderChurnFigures();
    mount('chart-churntrend-full', chartChurnTrend(false));
    window.AK_CHURN.renderChurn();

    miniMultiple('mini-reliability', 'Reliability', D.allTools.length + ' tools', chartReliability);
    miniMultiple('mini-toolmix', 'Mix', D.toolMix.miniLabel || '', chartToolMix);
    const lastFleet = D.toolFailures.fleet[D.toolFailures.fleet.length - 1];
    miniMultiple('mini-failures', 'Failures', lastFleet.toFixed(1) + '% fleet', chartFailures);
    miniMultiple('mini-churntrend', 'Churn', A.fmtInt(D.churnTrend.reedits[D.churnTrend.reedits.length - 1]) + ' re-edits/wk', chartChurnTrend);
  }

  window.AK_TOOLS = { renderTools };
})();


/* ============================================================
   Health: grades, outcomes, hygiene, context
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;
  const W = 1000, H = 380, padL = 40, padR = 46, padT = 14, padB = 26;

  function chartGrades() {
    const D = window.AK_DATA;
    const w = W, h = H, pL = padL, pR = padR, pT = padT, pB = padB;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 100], [h - pB, pT]);
    const gpaScale = A.scaleLinear([0, 4], [h - pB, pT]);

    A.axisTicksY(svg, [0, 25, 50, 75, 100], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    const order = ['A', 'B', 'C', 'D', 'F', 'U'];
    const colors = { A: 'var(--viz-5)', B: 'var(--viz-2)', C: 'var(--viz-7)', D: 'var(--viz-3)', F: 'var(--viz-4)', U: 'var(--faint)' };
    const labels = { A: 'A', B: 'B', C: 'C', D: 'D', F: 'F', U: 'unscored' };

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    let cum = new Array(D.nBuckets).fill(0);
    order.forEach((key) => {
      const bottom = cum.slice();
      const top = cum.map((c, i) => c + D.grades[i][key]);
      const bottomPts = bottom.map((v, i) => [xScale(i), yScale(v)]);
      const topPts = top.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathBand(topPts, bottomPts), fill: colors[key], opacity: key === 'U' ? '0.5' : '0.82' }));
      cum = top;
    });

    const gpaPts = D.grades.map((row, i) => [xScale(i), gpaScale(row.gpa)]);
    clip.appendChild(A.svgEl('path', { d: A.pathLine(gpaPts), fill: 'none', stroke: 'var(--text)', 'stroke-width': 2 }));
    [0, 1, 2, 3, 4].forEach((v) => {
      const y = gpaScale(v);
      const t = A.svgEl('text', { x: w - pR + 6, y: y + 3, class: 'axis-tick-text', 'text-anchor': 'start' });
      t.textContent = v.toFixed(0);
      svg.appendChild(t);
    });
    const gpaNow = D.grades[D.grades.length - 1].gpa;
    const lastPt = gpaPts[gpaPts.length - 1];
    const gpaLabel = A.svgEl('text', { x: w - pR + 6, y: lastPt[1] - 6, class: 'callout-label', fill: 'var(--text)' });
    gpaLabel.textContent = 'GPA ' + gpaNow.toFixed(2);
    svg.appendChild(gpaLabel);

    A.axisBaseline(svg, pL, w - pR, h - pB);

    const wrap = document.createElement('div');
    wrap.appendChild(svg);
    const legend = document.createElement('ul');
    legend.className = 'legend';
    legend.style.marginTop = '10px';
    order.forEach((key) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + colors[key] + '"></span>' + labels[key];
      legend.appendChild(li);
    });
    wrap.appendChild(legend);

    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      const row = D.grades[i];
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      order.forEach((key) => { html += '<div class="tt-row" style="color:' + colors[key] + '">' + labels[key] + ' <b>' + row[key].toFixed(1) + '%</b></div>'; });
      html += '<div class="tt-row">GPA <b>' + row.gpa.toFixed(2) + '</b></div>';
      return html;
    });

    return wrap;
  }

  function chartOutcomes() {
    const D = window.AK_DATA;
    const w = W, h = H, pL = padL, pR = 16, pT = padT, pB = padB;
    const barH = h * 0.22;
    const lineH = h - barH - 10;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([60, 100], [lineH, pT]);

    A.axisTicksY(svg, [60, 70, 80, 90, 100], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    const compPts = D.outcomes.map((r, i) => [xScale(i), yScale(r.completedRate)]);
    const abandPts = D.outcomes.map((r, i) => [xScale(i), yScale(clampToDomain(r.abandonedRate))]);
    function clampToDomain(v) { return Math.max(60, v + 60); }

    // The line axis zooms to 60-100%, so a bucket below 60% completion would draw
    // under the baseline and into the bars below. Clip to the line region alone.
    const lineClip = A.clipGroup(svg, pL, pT, w - pL - pR, lineH - pT);
    lineClip.appendChild(A.svgEl('path', { d: A.pathLine(compPts), fill: 'none', stroke: 'var(--ok)', 'stroke-width': 2.2 }));
    lineClip.appendChild(A.svgEl('path', { d: A.pathLine(abandPts), fill: 'none', stroke: 'var(--warn)', 'stroke-width': 1.6, 'stroke-dasharray': '4,3' }));

    A.axisBaseline(svg, pL, w - pR, lineH);

    const maxTotal = Math.max(...D.outcomes.map((r) => r.total));
    const barsTop = lineH + 16;
    const barScale = A.scaleLinear([0, maxTotal], [0, barH - 16]);
    const bw = (w - pL - pR) / D.nBuckets - 2;
    // The edge bars sit half a bar-width past pL and w-pR, so clip them to the
    // plot as the completed/abandoned bars in Economics are.
    const barClip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    D.outcomes.forEach((r, i) => {
      // Partition the magnitude bar the way the store partitions the cohort: completed, abandoned,
      // then the neutral rest (errored plus unknown). Drawing abandoned as its own warn segment
      // from the canonical count keeps the bar's abandoned share equal to the abandoned-rate line
      // above it; the old total-completed "rest" segment coloured errored and unknown as abandoned,
      // so a bucket with an errored session read a taller warn bar than its abandoned line.
      const completed = r.completed;
      const abandoned = r.abandoned;
      const other = Math.max(0, r.total - completed - abandoned);
      const x = xScale(i) - bw / 2;
      const base = barsTop + (barH - 16);
      const hComp = barScale(completed);
      const hAband = barScale(abandoned);
      const hOther = barScale(other);
      barClip.appendChild(A.svgEl('rect', { x, y: base - hComp, width: bw, height: hComp, fill: 'var(--ok)', opacity: '0.55' }));
      barClip.appendChild(A.svgEl('rect', { x, y: base - hComp - hAband, width: bw, height: hAband, fill: 'var(--warn)', opacity: '0.55' }));
      barClip.appendChild(A.svgEl('rect', { x, y: base - hComp - hAband - hOther, width: bw, height: hOther, fill: 'var(--muted)', opacity: '0.45' }));
    });

    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      const r = D.outcomes[i];
      const other = Math.max(0, r.total - r.completed - r.abandoned);
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--ok)">completed <b>' + r.completedRate.toFixed(1) + '%</b></div>' +
        '<div class="tt-row" style="color:var(--warn)">abandoned <b>' + r.abandonedRate.toFixed(1) + '%</b></div>' +
        '<div class="tt-row" style="color:var(--muted)">other <b>' + other + '</b></div>' +
        '<div class="tt-row">sessions <b>' + r.total + '</b></div>';
    });

    return svg;
  }

  function chartHygiene() {
    const D = window.AK_DATA;
    const w = W, h = H, pL = padL, pR = 100, pT = padT, pB = padB;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 10], [h - pB, pT]);
    A.axisTicksY(svg, [0, 2.5, 5, 7.5, 10], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    const series = [
      { key: 'terse', label: 'Terse prompts', color: 'var(--faint)' },
      { key: 'repeated', label: 'Repeated prompts', color: 'var(--viz-2)' },
      { key: 'noPointer', label: 'No code pointer', color: 'var(--viz-3)' },
      { key: 'unstructured', label: 'Unstructured start', color: 'var(--viz-8)' },
    ];
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const pendingLabels = [];
    series.forEach((s) => {
      const pts = D.hygiene[s.key].map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: s.color, 'stroke-width': s.key === 'noPointer' ? 2 : 1.5 }));
      const last = pts[pts.length - 1];
      pendingLabels.push({ y: last[1], color: s.color, text: s.label + ' ' + D.hygiene[s.key][D.hygiene[s.key].length - 1].toFixed(1) + '%' });
    });
    A.resolveLabelCollisions(pendingLabels, 14, pT, h - pB).forEach((lbl) => {
      const t = A.svgEl('text', { x: w - pR + 6, y: lbl.y + 3, class: 'callout-label', fill: lbl.color });
      t.textContent = lbl.text;
      svg.appendChild(t);
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);

    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      series.forEach((s) => { html += '<div class="tt-row" style="color:' + s.color + '">' + s.label + ' <b>' + D.hygiene[s.key][i].toFixed(1) + '%</b></div>'; });
      return html;
    });

    return svg;
  }

  function renderHygieneLegend() {
    const el = document.getElementById('hygiene-legend');
    if (!el) return;
    el.innerHTML = '';
    const items = [
      { label: 'Terse prompts', color: 'var(--faint)' },
      { label: 'Repeated prompts', color: 'var(--viz-2)' },
      { label: 'No code pointer', color: 'var(--viz-3)' },
      { label: 'Unstructured start', color: 'var(--viz-8)' },
    ];
    items.forEach((it) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + it.color + '"></span>' + it.label;
      el.appendChild(li);
    });
  }

  function chartContextHistogram() {
    const D = window.AK_DATA;
    const w = 1000, h = 300, pL = 44, pR = 16, pT = 34, pB = 28;
    const svg = A.svgRoot(w, h);
    const buckets = D.contextHistogram;
    const xScale = A.scaleLog([8000, 1024000], [pL, w - pR]);
    const maxCount = Math.max(...buckets.map((b) => b.count));
    const yScale = A.scaleLinear([0, maxCount * 1.08], [h - pB, pT]);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    buckets.forEach((b) => {
      const x0 = xScale(b.lo), x1 = xScale(b.hi);
      const y = yScale(b.count);
      clip.appendChild(A.svgEl('rect', {
        x: x0 + 1, y, width: Math.max(1, x1 - x0 - 2), height: (h - pB) - y,
        fill: 'var(--viz-1)', opacity: '0.75', class: 'scatter-dot',
      }));
    });

    [8000, 64000, 512000, 1024000].forEach((v) => {
      const x = xScale(v);
      const t = A.svgEl('text', { x, y: h - pB + 17, class: 'axis-tick-text', 'text-anchor': 'middle' });
      t.textContent = A.fmtK(v);
      svg.appendChild(t);
    });

    // p50/p90/max markers: only drawn when the server supplies them, so an
    // empty contextMarkers list quietly renders the bare histogram.
    (D.contextMarkers || []).forEach((m, idx) => {
      const x = xScale(m.v);
      svg.appendChild(A.svgEl('line', { x1: x, x2: x, y1: pT - 4, y2: h - pB, stroke: 'var(--subtext)', 'stroke-width': 1, 'stroke-dasharray': '3,3' }));
      const t = A.svgEl('text', { x, y: pT - 8 - (idx % 2) * 14, class: 'callout-label', 'text-anchor': 'middle', fill: 'var(--subtext)' });
      t.textContent = m.label;
      svg.appendChild(t);
    });

    A.axisBaseline(svg, pL, w - pR, h - pB);
    return svg;
  }

  function chartContextResets() {
    const D = window.AK_DATA;
    const w = 1000, h = 200, pL = 40, pR = 16, pT = 12, pB = 24;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const maxV = Math.max(...D.contextResets) * 1.15;
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);
    A.axisTicksY(svg, [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, false);
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const pts = D.contextResets.map((v, i) => [xScale(i), yScale(v)]);
    clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: 'var(--viz-4)', 'stroke-width': 2 }));
    D.contextResets.forEach((v, i) => {
      clip.appendChild(A.svgEl('circle', { cx: xScale(i), cy: yScale(v), r: 2, fill: 'var(--viz-4)' }));
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);
    return svg;
  }

  function renderContext() {
    const D = window.AK_DATA;
    const el = document.getElementById('chart-context');
    if (!el) return;
    el.innerHTML = '';
    const CS = D.contextSummary || {};

    const top = document.createElement('div');
    const tHead = document.createElement('div');
    tHead.className = 'chart-caption';
    tHead.innerHTML = '<span class="chart-title">Peak context per session</span><span class="chart-value mono">' + (CS.p50Label || '') + '</span>';
    top.appendChild(tHead);
    const topWrap = document.createElement('div');
    topWrap.className = 'overflow-x';
    const inner = document.createElement('div');
    inner.style.minWidth = '480px';
    inner.appendChild(chartContextHistogram());
    topWrap.appendChild(inner);
    top.appendChild(topWrap);

    const bottom = document.createElement('div');
    bottom.style.marginTop = '20px';
    const bHead = document.createElement('div');
    bHead.className = 'chart-caption';
    bHead.innerHTML = '<span class="chart-title">Weekly context resets</span><span class="chart-value mono">' + D.contextResets.reduce((a, b) => a + b, 0) + ' total</span>';
    bottom.appendChild(bHead);
    const bottomWrap = document.createElement('div');
    bottomWrap.className = 'overflow-x';
    const inner2 = document.createElement('div');
    inner2.style.minWidth = '480px';
    inner2.appendChild(chartContextResets());
    bottomWrap.appendChild(inner2);
    bottom.appendChild(bottomWrap);

    el.appendChild(top);
    el.appendChild(bottom);
    const cap = document.createElement('p');
    cap.className = 'panel-caption';
    cap.textContent = 'session_signals context peaks and weekly reset counts, from the same settle-pass rows as the other Health instruments.';
    el.appendChild(cap);
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function miniMultiple(id, title, valueText, chartFn) {
    const el = document.getElementById(id);
    if (!el) return;
    el.innerHTML = '';
    const head = document.createElement('div');
    head.className = 'chart-caption';
    head.innerHTML = '<span class="chart-title">' + title + '</span><span class="chart-value mono">' + valueText + '</span>';
    el.appendChild(head);
    el.appendChild(chartFn());
  }

  function tinyAreaChart(series, color, domainMax) {
    const D = window.AK_DATA;
    const w = 420, h = 210, pL = 6, pR = 6, pT = 8, pB = 8;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, domainMax], [h - pB, pT]);
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const pts = series.map((v, i) => [xScale(i), yScale(Math.min(v, domainMax))]);
    clip.appendChild(A.svgEl('path', { d: A.pathArea(pts, yScale(0)), fill: color, opacity: '0.2' }));
    clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: color, 'stroke-width': 1.6 }));
    svg.appendChild(A.svgEl('line', { x1: pL, x2: w - pR, y1: yScale(0), y2: yScale(0), class: 'axis-line' }));
    return svg;
  }

  function tinyHistogram() {
    const D = window.AK_DATA;
    const w = 420, h = 210, pL = 4, pR = 4, pT = 6, pB = 6;
    const svg = A.svgRoot(w, h);
    const buckets = D.contextHistogram;
    const xScale = A.scaleLog([8000, 1024000], [pL, w - pR]);
    const maxCount = Math.max(...buckets.map((b) => b.count));
    const yScale = A.scaleLinear([0, maxCount * 1.08], [h - pB, pT]);
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    buckets.forEach((b) => {
      const x0 = xScale(b.lo), x1 = xScale(b.hi);
      const y = yScale(b.count);
      clip.appendChild(A.svgEl('rect', { x: x0 + 1, y, width: Math.max(1, x1 - x0 - 2), height: (h - pB) - y, fill: 'var(--viz-1)', opacity: '0.7' }));
    });
    return svg;
  }

  function renderAllMinis() {
    const D = window.AK_DATA;
    const CS = D.contextSummary || {};
    const gradesLast = D.grades[D.grades.length - 1];
    miniMultiple('mini-grades', 'Grades', 'GPA ' + gradesLast.gpa.toFixed(2), () => miniGrades());
    const outcomesLast = D.outcomes[D.outcomes.length - 1];
    miniMultiple('mini-outcomes', 'Outcomes', outcomesLast.completedRate.toFixed(0) + '% completed', () => miniOutcomes());
    const hygLast = D.hygiene.noPointer[D.hygiene.noPointer.length - 1];
    miniMultiple('mini-hygiene', 'Hygiene', hygLast.toFixed(1) + '% no pointer', () => tinyAreaChart(D.hygiene.noPointer, 'var(--viz-3)', 10));
    miniMultiple('mini-context', 'Context', CS.p50Label || '', () => tinyHistogram());
  }

  function miniGrades() {
    const D = window.AK_DATA;
    const w = 420, h = 210, pL = 4, pR = 4, pT = 6, pB = 6;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([0, 100], [h - pB, pT]);
    const order = ['A', 'B', 'C', 'D', 'F', 'U'];
    const colors = { A: 'var(--viz-5)', B: 'var(--viz-2)', C: 'var(--viz-7)', D: 'var(--viz-3)', F: 'var(--viz-4)', U: 'var(--faint)' };
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    let cum = new Array(D.nBuckets).fill(0);
    order.forEach((key) => {
      const bottom = cum.slice();
      const top = cum.map((c, i) => c + D.grades[i][key]);
      const bottomPts = bottom.map((v, i) => [xScale(i), yScale(v)]);
      const topPts = top.map((v, i) => [xScale(i), yScale(v)]);
      clip.appendChild(A.svgEl('path', { d: A.pathBand(topPts, bottomPts), fill: colors[key], opacity: key === 'U' ? '0.5' : '0.82' }));
      cum = top;
    });
    return svg;
  }

  function miniOutcomes() {
    const D = window.AK_DATA;
    const w = 420, h = 210, pL = 4, pR = 4, pT = 8, pB = 8;
    const svg = A.svgRoot(w, h);
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const yScale = A.scaleLinear([60, 100], [h - pB, pT]);
    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const compPts = D.outcomes.map((r, i) => [xScale(i), yScale(r.completedRate)]);
    clip.appendChild(A.svgEl('path', { d: A.pathLine(compPts), fill: 'none', stroke: 'var(--ok)', 'stroke-width': 1.6 }));
    return svg;
  }

  function renderHealth() {
    mount('chart-grades', chartGrades());
    mount('chart-outcomes', chartOutcomes());
    mount('chart-hygiene', chartHygiene());
    renderHygieneLegend();
    renderContext();
    renderAllMinis();
  }

  window.AK_HEALTH = { renderHealth };
})();


/* ============================================================
   File churn treemap: squarified layout, drillable project ->
   folder -> file, with breadcrumb navigation and keyboard support.
   ============================================================ */
(function () {
  'use strict';

  function squarify(items, x, y, w, h) {
    const results = [];
    const total = items.reduce((s, it) => s + it.value, 0);
    if (total <= 0 || !items.length) return results;
    layout(items.slice(), x, y, w, h);
    return results;

    function layout(list, x, y, w, h) {
      if (!list.length) return;
      const total = list.reduce((s, it) => s + it.value, 0);
      const shortSide = Math.min(w, h);
      const areaScale = (w * h) / total;
      const scaled = list.map((it) => it.value * areaScale);

      let i = 1;
      while (i < list.length && worse(scaled.slice(0, i), shortSide) >= worse(scaled.slice(0, i + 1), shortSide)) {
        i++;
      }
      const row = list.slice(0, i);
      const rowSum = row.reduce((s, it) => s + it.value, 0);
      if (w >= h) {
        const rowW = (rowSum / total) * w;
        placeRow(row, x, y, rowW, h, 'vertical');
        const rest = list.slice(i);
        if (!rest.length) return;
        layout(rest, x + rowW, y, w - rowW, h);
      } else {
        const rowH = (rowSum / total) * h;
        placeRow(row, x, y, w, rowH, 'horizontal');
        const rest = list.slice(i);
        if (!rest.length) return;
        layout(rest, x, y + rowH, w, h - rowH);
      }
    }

    function worse(scaledRow, shortSide) {
      const sum = scaledRow.reduce((a, b) => a + b, 0);
      if (sum === 0) return Infinity;
      const max = Math.max(...scaledRow), min = Math.min(...scaledRow);
      const s2 = sum * sum;
      const ratio1 = (shortSide * shortSide * max) / s2;
      const ratio2 = s2 / (shortSide * shortSide * min);
      return Math.max(ratio1, ratio2);
    }

    function placeRow(row, x, y, w, h, orientation) {
      const sum = row.reduce((s, it) => s + it.value, 0);
      if (orientation === 'vertical') {
        let cy = y;
        row.forEach((it) => {
          const itemH = (it.value / sum) * h;
          results.push({ x, y: cy, w, h: itemH, item: it.item });
          cy += itemH;
        });
      } else {
        let cx = x;
        row.forEach((it) => {
          const itemW = (it.value / sum) * w;
          results.push({ x: cx, y, w: itemW, h, item: it.item });
          cx += itemW;
        });
      }
    }
  }

  // sessionsBrightness scales a cell's session count against the current
  // corpus's own maximum, so it is recomputed per render rather than once at
  // module load: AK_DATA is swapped wholesale on every htmx refresh, and a
  // stale max would misjudge brightness against the new data's scale.
  function sessionsBrightness(sessions, maxSessionsGlobal) {
    return Math.min(1, Math.sqrt(sessions / maxSessionsGlobal));
  }
  function mixRgb(baseRgb, targetRgb, t) {
    return baseRgb.map((v, i) => Math.round(v + (targetRgb[i] - v) * t));
  }

  const VIZ_RGB = {
    'var(--viz-1)': [198, 168, 242],
    'var(--viz-2)': [136, 207, 206],
    'var(--viz-3)': [240, 191, 146],
    'var(--viz-4)': [236, 152, 176],
    'var(--viz-5)': [166, 210, 158],
  };

  /* drill state: [] = all projects, [proj] = folders within a project,
     [proj, folder] = files within a folder */
  let path = [];

  function projectRows() {
    const D = window.AK_DATA;
    const byProject = {};
    D.churn.forEach((row) => { (byProject[row.project] = byProject[row.project] || []).push(row); });
    return D.projects.map((p) => {
      const rows = byProject[p] || [];
      return {
        value: rows.reduce((s, r) => s + r.edits, 0),
        sessions: rows.reduce((s, r) => s + r.sessions, 0),
        label: p, key: p, vizVar: D.projectViz[p],
      };
    }).filter((r) => r.value > 0).sort((a, b) => b.value - a.value);
  }

  function folderRows(project) {
    const D = window.AK_DATA;
    const rows = D.churn.filter((r) => r.project === project);
    const byFolder = {};
    rows.forEach((r) => { (byFolder[r.folder] = byFolder[r.folder] || []).push(r); });
    const folders = (D.folderPlan[project] || Object.keys(byFolder));
    return folders.map((f) => {
      const items = byFolder[f] || [];
      return {
        value: items.reduce((s, r) => s + r.edits, 0),
        sessions: items.reduce((s, r) => s + r.sessions, 0),
        label: f, key: f, vizVar: D.projectViz[project],
      };
    }).filter((r) => r.value > 0).sort((a, b) => b.value - a.value);
  }

  function fileRows(project, folder) {
    const D = window.AK_DATA;
    return D.churn.filter((r) => r.project === project && r.folder === folder).map((r) => ({
      value: r.edits, sessions: r.sessions, label: r.path.split('/').pop(), key: r.path,
      vizVar: D.projectViz[project], reading: r.reading, fullPath: r.project + '/' + r.path,
    })).sort((a, b) => b.value - a.value);
  }

  function currentRows() {
    if (path.length === 0) return { rows: projectRows(), level: 'project' };
    if (path.length === 1) return { rows: folderRows(path[0]), level: 'folder' };
    return { rows: fileRows(path[0], path[1]), level: 'file' };
  }

  function renderBreadcrumb() {
    const el = document.getElementById('treemap-breadcrumb');
    if (!el) return;
    el.innerHTML = '';
    const crumbs = [{ label: 'all projects', depth: 0 }];
    if (path[0]) crumbs.push({ label: path[0].replace('/', '-'), depth: 1 });
    if (path[1]) crumbs.push({ label: path[1].replace(/\//g, '-'), depth: 2 });

    crumbs.forEach((c, i) => {
      if (i > 0) {
        const sep = document.createElement('span');
        sep.className = 'crumb-sep';
        sep.textContent = '/';
        el.appendChild(sep);
      }
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = c.label;
      const isCurrent = i === crumbs.length - 1;
      if (isCurrent) btn.setAttribute('aria-current', 'true');
      btn.addEventListener('click', () => {
        path = path.slice(0, c.depth);
        renderTreemap();
      });
      el.appendChild(btn);
    });
  }

  function renderTreemap() {
    const D = window.AK_DATA;
    const el = document.getElementById('treemap');
    if (!el) return;
    el.innerHTML = '';
    renderBreadcrumb();

    const maxSessionsGlobal = Math.max(...D.churn.map((r) => r.sessions)) || 1;
    const rect = el.getBoundingClientRect();
    const w = rect.width || 700, h = 420;
    const { rows, level } = currentRows();
    const items = rows.map((r) => ({ value: r.value, item: r }));
    const rects = squarify(items, 0, 0, w, h);

    rects.forEach((r) => {
      const row = r.item;
      const drillable = level !== 'file';
      const cell = document.createElement(drillable ? 'button' : 'div');
      cell.className = 'treemap-cell';
      if (drillable) {
        cell.type = 'button';
        cell.setAttribute('role', 'button');
      } else {
        cell.tabIndex = 0;
        cell.setAttribute('role', 'button');
      }
      cell.style.position = 'absolute';
      cell.style.left = r.x + 'px';
      cell.style.top = r.y + 'px';
      cell.style.width = Math.max(0, r.w - 1) + 'px';
      cell.style.height = Math.max(0, r.h - 1) + 'px';

      const rgb = VIZ_RGB[row.vizVar] || [150, 150, 150];
      const t = sessionsBrightness(row.sessions, maxSessionsGlobal);
      const fillRgb = mixRgb([36, 34, 40], rgb, 0.18 + t * 0.55);
      cell.style.background = 'rgb(' + fillRgb.join(',') + ')';

      if (level === 'file') {
        cell.title = row.fullPath + ' (' + row.value + ' edits, ' + row.sessions + ' sessions)' + (row.reading ? ': ' + row.reading : '');
      } else {
        cell.title = row.label + ' (' + row.value + ' edits, ' + row.sessions + ' sessions)';
      }

      if (r.w >= 60 && r.h >= 32) {
        const fname = document.createElement('div');
        fname.className = 'fname';
        fname.textContent = row.label;
        cell.appendChild(fname);
        const edits = document.createElement('div');
        edits.className = 'fedits';
        edits.textContent = row.value + ' edits';
        cell.appendChild(edits);
      }

      if (drillable) {
        cell.addEventListener('click', () => {
          path = path.concat([row.key]);
          renderTreemap();
        });
        cell.addEventListener('keydown', (e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            path = path.concat([row.key]);
            renderTreemap();
          }
        });
      }
      el.appendChild(cell);
    });

    if (!el.dataset.escBound) {
      el.dataset.escBound = '1';
      el.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && path.length > 0) {
          path = path.slice(0, path.length - 1);
          renderTreemap();
        }
      });
    }
  }

  function renderChurnLegend() {
    const D = window.AK_DATA;
    const el = document.getElementById('churn-legend');
    if (!el) return;
    el.innerHTML = '';
    D.projects.forEach((p) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + D.projectViz[p] + '"></span>' + p;
      el.appendChild(li);
    });
  }

  function renderChurn() {
    renderTreemap();
    renderChurnLegend();
  }

  window.AK_CHURN = { renderChurn, resetDrill: () => { path = []; } };
})();


/* ============================================================
   Economics: cost of quality, cache savings + hit rate
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;
  const W = 1000, H = 380, mW = 480, mH = 220;

  function chartCostQuality(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : W, h = mini ? mH : H;
    const pL = mini ? 30 : 46, pR = mini ? 8 : 16, pT = mini ? 8 : 14, pB = mini ? 16 : 26;
    const svg = A.svgRoot(w, h);
    const rows = D.costQuality.rows;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const maxV = Math.max(...rows.map((r) => r.completed + r.abandoned + (r.other || 0))) * 1.1;
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, Math.round(maxV)] : [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => '$' + A.fmtK(v));
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const bw = (w - pL - pR) / D.nBuckets - (mini ? 1 : 2);
    rows.forEach((r, i) => {
      const x = xScale(i) - bw / 2;
      const other = r.other || 0;
      const yComp = yScale(r.completed);
      const yAband = yScale(r.completed + r.abandoned);
      const yTot = yScale(r.completed + r.abandoned + other);
      clip.appendChild(A.svgEl('rect', { x, y: yComp, width: bw, height: (h - pB) - yComp, fill: 'var(--ok)', opacity: '0.78' }));
      clip.appendChild(A.svgEl('rect', { x, y: yAband, width: bw, height: yComp - yAband, fill: 'var(--warn)', opacity: '0.82' }));
      if (other > 0) clip.appendChild(A.svgEl('rect', { x, y: yTot, width: bw, height: yAband - yTot, fill: 'var(--muted)', opacity: '0.5' }));
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      const r = rows[i];
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--ok)">completed <b>$' + r.completed.toFixed(0) + '</b></div>' +
        '<div class="tt-row" style="color:var(--warn)">abandoned <b>$' + r.abandoned.toFixed(0) + '</b></div>' +
        ((r.other || 0) > 0 ? '<div class="tt-row" style="color:var(--muted)">other <b>$' + r.other.toFixed(0) + '</b></div>' : '');
    });

    return svg;
  }

  function renderCostQualityLegend() {
    const el = document.getElementById('costquality-legend');
    if (!el) return;
    el.innerHTML = '';
    const items = [{ label: 'Completed sessions', color: 'var(--ok)' }, { label: 'Abandoned sessions', color: 'var(--warn)' }, { label: 'Other outcomes', color: 'var(--muted)' }];
    items.forEach((it) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + it.color + '"></span>' + it.label;
      el.appendChild(li);
    });
  }

  function renderCostQualityFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('costquality-figures');
    if (!el) return;
    el.innerHTML = '';
    const CQ = D.costQuality;
    // A trailing '+' marks a lower-bound spend, when the window folded a token-bearing unpriced
    // event; the flag is window-wide, so it rides both the total and the abandoned figure. The
    // per-completed median rides the gallery cohort's own flag.
    const spendMark = CQ.totalSpendIncomplete ? '+' : '';
    const figs = [
      { v: '$' + A.fmtInt(CQ.totalSpend) + spendMark, k: 'total spend' },
      { v: '$' + A.fmtInt(CQ.totalAbandoned) + spendMark + ' sunk', k: CQ.abandonedSharePct.toFixed(0) + '% of spend' },
      { v: '$' + CQ.medianPerCompletedSession.toFixed(2) + (CQ.medianCostIncomplete ? '+' : ''), k: 'median $ per completed session' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function chartCache(mini) {
    const D = window.AK_DATA;
    const w = mini ? mW : W, h = mini ? mH : H;
    const pL = mini ? 30 : 46, pR = mini ? 8 : 60, pT = mini ? 8 : 14, pB = mini ? 16 : 26;
    const svg = A.svgRoot(w, h);
    const C = D.cache;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const maxSavings = Math.max(...C.savings) * 1.15;
    const yScale = A.scaleLinear([0, maxSavings], [h - pB, pT]);
    const yScaleHit = A.scaleLinear([80, 92], [h - pB, pT]);

    A.axisTicksY(svg, mini ? [0, Math.round(maxSavings)] : [0, Math.round(maxSavings / 2), Math.round(maxSavings)], pL, w - pR, yScale, (v) => '$' + v);
    A.bucketAxis(svg, w, h, pB, pL, pR, mini);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const pts = C.savings.map((v, i) => [xScale(i), yScale(v)]);
    clip.appendChild(A.svgEl('path', { d: A.pathArea(pts, yScale(0)), fill: 'var(--viz-2)', opacity: '0.22' }));
    clip.appendChild(A.svgEl('path', { d: A.pathLine(pts), fill: 'none', stroke: 'var(--viz-2)', 'stroke-width': mini ? 1.4 : 2 }));

    // Draw the hit-rate line only across measured buckets, so an idle bucket reads as a gap
    // rather than a false drop to 0% on the 80-92% scale. hitRateMeasured is absent on older
    // payloads, so treat every bucket as measured then.
    const measured = C.hitRateMeasured || C.hitRate.map(() => true);
    const measuredPts = C.hitRate.map((v, i) => [xScale(i), yScaleHit(v)]).filter((_, i) => measured[i]);
    if (measuredPts.length) clip.appendChild(A.svgEl('path', { d: A.pathLine(measuredPts), fill: 'none', stroke: 'var(--viz-7)', 'stroke-width': mini ? 1.2 : 1.6, 'stroke-dasharray': '3,3' }));

    if (!mini) {
      [80, 85, 90].forEach((v) => {
        const y = yScaleHit(v);
        const t = A.svgEl('text', { x: w - pR + 6, y: y + 3, class: 'axis-tick-text', 'text-anchor': 'start' });
        t.textContent = v + '%';
        svg.appendChild(t);
      });
      const lastHit = measuredPts.length ? measuredPts[measuredPts.length - 1] : null;
      if (lastHit) {
        const t2 = A.svgEl('text', { x: w - pR + 6, y: lastHit[1] - 8, class: 'callout-label', fill: 'var(--viz-7)' });
        t2.textContent = 'hit rate ' + C.hitRateNow.toFixed(0) + '%';
        svg.appendChild(t2);
      }
    }

    A.axisBaseline(svg, pL, w - pR, h - pB);

    if (!mini) A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      const measuredHit = !C.hitRateMeasured || C.hitRateMeasured[i];
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--viz-2)">savings <b>$' + C.savings[i].toFixed(0) + '</b></div>' +
        '<div class="tt-row" style="color:var(--viz-7)">hit rate <b>' + (measuredHit ? C.hitRate[i].toFixed(1) + '%' : 'n/a') + '</b></div>';
    });

    return svg;
  }

  function renderCacheFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('cache-figures');
    if (!el) return;
    el.innerHTML = '';
    const savingsPerDollar = D.cache.totalSavings / D.costQuality.totalSpend;
    // Savings reads "partial", not the "+" lower-bound marker, because the omitted term for an
    // unpriced model can be either sign. The per-dollar figure is partial when either its saving
    // or the spend it divides by is incomplete.
    const savingsMark = D.cache.savingsIncomplete ? ' partial' : '';
    const perDollarMark = (D.cache.savingsIncomplete || D.costQuality.totalSpendIncomplete) ? ' partial' : '';
    const figs = [
      { v: '$' + A.fmtInt(D.cache.totalSavings) + savingsMark, k: 'savings total' },
      { v: D.cache.hitRateNow.toFixed(0) + '%', k: 'hit rate' },
      { v: '$' + savingsPerDollar.toFixed(2) + perDollarMark, k: 'savings per $1 spent' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function miniMultiple(id, title, valueText, chartFn) {
    const el = document.getElementById(id);
    if (!el) return;
    el.innerHTML = '';
    const head = document.createElement('div');
    head.className = 'chart-caption';
    head.innerHTML = '<span class="chart-title">' + title + '</span><span class="chart-value mono">' + valueText + '</span>';
    el.appendChild(head);
    el.appendChild(chartFn(true));
  }

  function renderEconomics() {
    const D = window.AK_DATA;
    renderCostQualityFigures();
    mount('chart-costquality-full', chartCostQuality(false));
    renderCostQualityLegend();
    renderCacheFigures();
    mount('chart-cache-full', chartCache(false));

    miniMultiple('mini-costquality', 'Cost of quality', D.costQuality.abandonedSharePct.toFixed(0) + '% abandoned $', chartCostQuality);
    miniMultiple('mini-cache', 'Cache', '$' + A.fmtInt(D.cache.totalSavings) + ' saved', chartCache);
  }

  window.AK_ECONOMICS = { renderEconomics };
})();


/* ============================================================
   Subagents: figures, weekly delegation share, fan-out strip.
   ============================================================ */
(function () {
  'use strict';
  const A = window.AK;

  function chartSubagents() {
    const D = window.AK_DATA;
    const w = 1000, h = 380, pL = 40, pR = 90, pT = 14, pB = 26;
    const svg = A.svgRoot(w, h);
    const S = D.subagents;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    // Floor at 1% so a window with no delegation draws a flat baseline rather than a
    // degenerate domain with NaN axis ticks.
    const maxV = Math.max(1, Math.max(...S.delegateShare, ...S.costShare, 0) * 1.15);
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => v + '%');
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    const delPts = S.delegateShare.map((v, i) => [xScale(i), yScale(v)]);
    const costPts = S.costShare.map((v, i) => [xScale(i), yScale(v)]);
    clip.appendChild(A.svgEl('path', { d: A.pathLine(delPts), fill: 'none', stroke: 'var(--accent)', 'stroke-width': 2.2 }));
    clip.appendChild(A.svgEl('path', { d: A.pathLine(costPts), fill: 'none', stroke: 'var(--muted)', 'stroke-width': 1.6, 'stroke-dasharray': '3,3' }));

    const lastDel = delPts[delPts.length - 1], lastCost = costPts[costPts.length - 1];
    const pendingLabels = [
      { y: lastDel[1], color: 'var(--accent)', text: 'delegate ' + S.delegateShare[S.delegateShare.length - 1].toFixed(0) + '%' },
      { y: lastCost[1], color: 'var(--muted)', text: 'cost share ' + S.costShare[S.costShare.length - 1].toFixed(0) + '%' },
    ];
    A.resolveLabelCollisions(pendingLabels, 14, pT, h - pB).forEach((lbl) => {
      const t = A.svgEl('text', { x: w - pR + 6, y: lbl.y + 3, class: 'callout-label', fill: lbl.color });
      t.textContent = lbl.text;
      svg.appendChild(t);
    });

    A.axisBaseline(svg, pL, w - pR, h - pB);
    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      return '<div class="tt-title">' + D.bucketLabels[i] + '</div>' +
        '<div class="tt-row" style="color:var(--accent)">root sessions delegating <b>' + S.delegateShare[i].toFixed(1) + '%</b></div>' +
        '<div class="tt-row" style="color:var(--muted)">cost via subagents <b>' + S.costShare[i].toFixed(1) + '%</b></div>';
    });

    return svg;
  }

  function renderFigures() {
    const D = window.AK_DATA;
    const el = document.getElementById('subagents-figures');
    if (!el) return;
    el.innerHTML = '';
    const S = D.subagents;
    // The cost share is a ratio of two lower-bound sums when a token-bearing event went unpriced,
    // so it reads "partial" (the ratio can move either way), not a "+" lower bound.
    const figs = [
      { v: S.sessionsThatDelegatePct + '%', k: 'sessions that delegate' },
      { v: A.fmtInt(S.subagentSessionsInWindow), k: 'subagent sessions in window' },
      { v: S.costRunThroughSubagentsPct + '%' + (S.costShareIncomplete ? ' partial' : ''), k: 'cost run through subagents' },
      { v: S.deepestTree, k: 'deepest tree (levels)' },
    ];
    figs.forEach((f) => {
      const d = document.createElement('div');
      d.innerHTML = '<div class="figure">' + f.v + '</div><div class="figure-key">' + f.k + '</div>';
      el.appendChild(d);
    });
  }

  function renderLegend() {
    const el = document.getElementById('subagents-legend');
    if (!el) return;
    el.innerHTML = '';
    const items = [{ label: 'Root sessions that delegate', color: 'var(--accent)' }, { label: 'Cost share via subagents', color: 'var(--muted)' }];
    items.forEach((it) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + it.color + '"></span>' + it.label;
      el.appendChild(li);
    });
  }

  function chartFanoutStack() {
    const D = window.AK_DATA;
    const w = 1000, h = 300, pL = 40, pR = 16, pT = 14, pB = 26;
    const svg = A.svgRoot(w, h);
    const S = D.subagents;
    const xScale = A.scaleLinear([0, D.nBuckets - 1], [pL, w - pR]);
    const totals = S.fanoutRows.map((r) => S.fanoutOrder.reduce((s, k) => s + (r[k] || 0), 0));
    // Floor the domain at 1 so a window with no fan-out (every bucket zero, or no rows at
    // all) draws a flat baseline instead of a degenerate [0,0]/[0,-Infinity] domain that
    // renders NaN axis ticks. Matches the guard the other stacked charts use.
    const maxV = Math.max(1, Math.max(...totals, 0) * 1.1);
    const yScale = A.scaleLinear([0, maxV], [h - pB, pT]);

    A.axisTicksY(svg, [0, Math.round(maxV / 2), Math.round(maxV)], pL, w - pR, yScale, (v) => v);
    A.bucketAxis(svg, w, h, pB, pL, pR, false);

    const clip = A.clipGroup(svg, pL, pT, w - pL - pR, h - pT - pB);
    let cum = new Array(D.nBuckets).fill(0);
    S.fanoutOrder.forEach((key) => {
      const bottom = cum.slice();
      const top = cum.map((c, i) => c + S.fanoutRows[i][key]);
      const bottomPts = bottom.map((v, i) => [xScale(i), yScale(v)]);
      const topPts = top.map((v, i) => [xScale(i), yScale(v)]);
      // fanoutColors already carries the ramp's opacity (faint buckets use
      // rgba), so no extra opacity attribute here to avoid double-attenuating.
      clip.appendChild(A.svgEl('path', { d: A.pathBand(topPts, bottomPts), fill: S.fanoutColors[key] }));
      cum = top;
    });
    A.axisBaseline(svg, pL, w - pR, h - pB);

    A.attachHoverBucket(svg, w, h, pL, pR, pT, pB, xScale, (i) => {
      const row = S.fanoutRows[i];
      let html = '<div class="tt-title">' + D.bucketLabels[i] + '</div>';
      S.fanoutOrder.forEach((key) => {
        html += '<div class="tt-row" style="color:' + S.fanoutColors[key] + '">' + S.fanoutLabels[key] + ' <b>' + row[key] + '</b></div>';
      });
      return html;
    });

    return svg;
  }

  function renderFanoutLegend() {
    const D = window.AK_DATA;
    const el = document.getElementById('fanout-legend');
    if (!el) return;
    el.innerHTML = '';
    const S = D.subagents;
    S.fanoutOrder.forEach((key) => {
      const li = document.createElement('li');
      li.className = 'legend-chip';
      li.innerHTML = '<span class="legend-swatch" style="background:' + S.fanoutColors[key] + '"></span>' + S.fanoutLabels[key];
      el.appendChild(li);
    });
  }

  function mount(id, node) {
    const el = document.getElementById(id);
    if (el) { el.innerHTML = ''; el.appendChild(node); }
  }

  function renderSubagents() {
    renderFigures();
    mount('chart-subagents-full', chartSubagents());
    renderLegend();
    mount('chart-fanout-full', chartFanoutStack());
    renderFanoutLegend();
  }

  window.AK_SUBAGENTS = { renderSubagents };
})();


/* ============================================================
   Tab strip behavior: role=tablist/tab/tabpanel, arrow-key nav,
   click handling, and the mini-multiple "jump to tab" wiring.
   Generalized to support multiple independent tab strips and a
   data-jump value of "stripId:tabId".
   ============================================================ */
(function () {
  'use strict';

  const strips = {};

  function initTabStrip(stripId) {
    const strip = document.getElementById(stripId);
    if (!strip) return;
    const tabs = Array.prototype.slice.call(strip.querySelectorAll('[role="tab"]'));

    function selectTab(tab, focus) {
      tabs.forEach((t) => {
        const selected = t === tab;
        t.setAttribute('aria-selected', selected ? 'true' : 'false');
        t.tabIndex = selected ? 0 : -1;
        const panel = document.getElementById(t.getAttribute('aria-controls'));
        if (panel) {
          if (selected) {
            panel.hidden = false;
            panel.classList.add('tabpanel-fade');
            // the treemap is measured off its own getBoundingClientRect(), which
            // reads 0 while its tabpanel is hidden; boot() renders it once behind
            // a hidden panel (falling back to a stale fixed width), so re-render
            // it now that the panel is actually laid out and can report a real width.
            if (panel.querySelector('#treemap') && window.AK_CHURN) window.AK_CHURN.renderChurn();
          } else {
            panel.hidden = true;
            panel.classList.remove('tabpanel-fade');
          }
        }
      });
      if (focus) tab.focus();
    }

    tabs.forEach((tab, i) => {
      tab.addEventListener('click', () => selectTab(tab, false));
      tab.addEventListener('keydown', (e) => {
        let next = null;
        if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = tabs[(i + 1) % tabs.length];
        else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = tabs[(i - 1 + tabs.length) % tabs.length];
        else if (e.key === 'Home') next = tabs[0];
        else if (e.key === 'End') next = tabs[tabs.length - 1];
        if (next) {
          e.preventDefault();
          selectTab(next, true);
        }
      });
    });

    strip.selectById = function (tabId) {
      const target = tabs.find((t) => t.id === tabId);
      if (target) selectTab(target, false);
    };
    strips[stripId] = strip;
  }

  const reduceMotion = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  function wireJumpButtons() {
    document.querySelectorAll('[data-jump]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const spec = btn.getAttribute('data-jump'); // "stripId:tabId"
        const [stripId, tabId] = spec.split(':');
        const strip = strips[stripId] || document.getElementById(stripId);
        if (strip && strip.selectById) strip.selectById(tabId);
        const scrollTarget = btn.getAttribute('data-scroll');
        if (scrollTarget) {
          const panel = document.getElementById(scrollTarget);
          if (panel) panel.scrollIntoView({ behavior: reduceMotion ? 'instant' : 'smooth', block: 'start' });
        }
      });
    });
  }

  window.AK_TABS = { initTabStrip, wireJumpButtons };
})();


/* ============================================================
   Bootstrap: load AK_DATA from the server-embedded #insights-data
   JSON and run every render module. Runs once on first load and
   again on every htmx:afterSwap of the #insights section, so a
   range change (or any other control that swaps the section) redraws
   with the fresh data the fragment carries.
   ============================================================ */
(function () {
  'use strict';

  // loadData parses the JSON payload the server embeds in #insights-data
  // into window.AK_DATA. Returns false (and leaves AK_DATA untouched) when
  // the element is missing or empty, so a page without the insights section
  // - or a fragment that failed to render it - is a quiet no-op rather than
  // a thrown error.
  function loadData() {
    const el = document.getElementById('insights-data');
    if (!el || !el.textContent) return false;
    try {
      window.AK_DATA = JSON.parse(el.textContent);
    } catch (e) {
      return false;
    }
    return true;
  }

  let resizeBound = false;

  function runInsights() {
    if (!loadData()) return;

    // A fresh swap carries a new #insights-data but the treemap module's
    // drill path is closed over in its own IIFE scope, so it survives the
    // swap unless explicitly reset here.
    if (window.AK_CHURN) window.AK_CHURN.resetDrill();

    window.AK_TABS.initTabStrip('velocity-tabs');
    window.AK_TABS.initTabStrip('tools-tabs');
    window.AK_TABS.initTabStrip('health-tabs');
    window.AK_TABS.initTabStrip('economics-tabs');
    window.AK_TABS.wireJumpButtons();

    window.AK_FLEETMIX.renderFleetMix();
    window.AK_GALLERY.renderGallery();
    window.AK_VELOCITY.renderVelocity();
    window.AK_TOOLS.renderTools();
    window.AK_HEALTH.renderHealth();
    window.AK_ECONOMICS.renderEconomics();
    window.AK_SUBAGENTS.renderSubagents();

    // treemap is measured off its own bounding box; re-render on resize
    // (debounced) so the squarified layout tracks the actual column width.
    // Bound once: a per-run listener would stack one leaked closure per
    // htmx swap, each still resolving the live #treemap by id, so nothing is
    // gained by rebinding and the accumulation is pure waste.
    if (!resizeBound) {
      resizeBound = true;
      let resizeTimer = null;
      window.addEventListener('resize', () => {
        clearTimeout(resizeTimer);
        resizeTimer = setTimeout(() => { if (window.AK_CHURN) window.AK_CHURN.renderChurn(); }, 150);
      });
    }
  }

  function boot() {
    runInsights();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }

  document.addEventListener('htmx:afterSwap', (e) => {
    const t = (e.detail && e.detail.target) || e.target;
    if (!t || t.id !== 'insights') return;
    // Read the live node by id, not the event's target: an outerHTML swap
    // reports the detached old node, whose #insights-data is stale.
    if (!document.getElementById('insights')) return;
    runInsights();
  });
})();
