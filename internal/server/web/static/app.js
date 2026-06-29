// akari UI behavior: live session updates, inline tool-body expansion with diff
// rendering for editing tools, transcript density, the timeline-rail scroll spy,
// and the instrument needle-settle on live stat changes. Kept dependency-free and
// static so the binary stays self-contained. Bodies are always inserted with
// textContent, never innerHTML, so a stored tool body can never inject markup.
(function () {
  "use strict";

  // ---------------- Density ----------------
  function markDensity(mode) {
    document.body.classList.toggle("density-compact", mode === "compact");
    Array.prototype.slice.call(document.querySelectorAll(".seg[data-density]")).forEach(function (b) {
      b.classList.toggle("active", b.getAttribute("data-density") === mode);
    });
  }
  function currentDensity() {
    try { return localStorage.getItem("akari-density") || "comfortable"; } catch (e) { return "comfortable"; }
  }
  document.addEventListener("click", function (ev) {
    var seg = ev.target.closest ? ev.target.closest(".seg[data-density]") : null;
    if (!seg) return;
    var mode = seg.getAttribute("data-density");
    try { localStorage.setItem("akari-density", mode); } catch (e) {}
    markDensity(mode);
  });

  // ---------------- Breakdown bars ----------------
  // Fill from 0 to the server-computed percentage so the bars grow in on load.
  function animateBars(root) {
    var bars = (root || document).querySelectorAll(".bar-fill[data-pct]");
    Array.prototype.slice.call(bars).forEach(function (el) {
      if (el._done) return;
      el._done = true;
      el.style.background = el.getAttribute("data-color") || "";
      var pct = el.getAttribute("data-pct") || "0";
      requestAnimationFrame(function () { requestAnimationFrame(function () { el.style.width = pct + "%"; }); });
    });
  }

  // ---------------- Outline scroll spy ----------------
  // Highlights the outline turn whose message is at the reading line. On scroll
  // it samples the one message under a fixed point (O(1), rAF-throttled) and
  // resolves its outline entry by id, so there are no per-message observers or
  // indexes — nothing whose cost or memory grows with the session. Set up once;
  // it reads the live DOM, so it keeps working across live transcript swaps.
  var outlineScrollHandler = null;
  function initOutlineSpy() {
    if (outlineScrollHandler) {
      window.removeEventListener("scroll", outlineScrollHandler);
      outlineScrollHandler = null;
    }
    if (!document.querySelector(".transcript")) return;
    var current = null;
    var ticking = false;
    function update() {
      ticking = false;
      var t = document.querySelector(".transcript");
      if (!t) return;
      var rect = t.getBoundingClientRect();
      var el = document.elementFromPoint(rect.left + Math.min(rect.width / 2, 180), window.innerHeight * 0.32);
      var msg = el && el.closest ? el.closest(".msg[data-ordinal]") : null;
      if (!msg) return;
      var entry = document.getElementById("ol-" + msg.getAttribute("data-ordinal"));
      if (!entry || entry === current) return;
      if (current) current.classList.remove("current");
      entry.classList.add("current");
      current = entry;
    }
    outlineScrollHandler = function () { if (!ticking) { ticking = true; requestAnimationFrame(update); } };
    window.addEventListener("scroll", outlineScrollHandler, { passive: true });
    update();
  }

  // ---------------- Stat needle-settle ----------------
  function snapshotStats() {
    var map = {};
    Array.prototype.slice.call(document.querySelectorAll("#session-stats .value[data-stat-key]")).forEach(function (el) {
      map[el.getAttribute("data-stat-key")] = el.textContent;
    });
    return map;
  }
  function flashChangedStats(before) {
    Array.prototype.slice.call(document.querySelectorAll("#session-stats .value[data-stat-key]")).forEach(function (el) {
      var k = el.getAttribute("data-stat-key");
      if (before[k] !== undefined && before[k] !== el.textContent) {
        el.classList.remove("settling");
        void el.offsetWidth; // restart the animation
        el.classList.add("settling");
      }
    });
  }

  // rehydrate runs after a live transcript swap. The outline spy and inspector
  // live outside the swapped region (they persist across updates), so only the
  // bars in the swapped fragment need re-animating here.
  function rehydrate() {
    animateBars();
  }

  // ---------------- Live session updates ----------------
  function initLive() {
    var el = document.getElementById("session-body");
    if (!el) return;
    var sse = el.getAttribute("data-sse");
    var body = el.getAttribute("data-body");
    if (!sse || !body || !window.EventSource) return;
    var es = new EventSource(sse);
    es.addEventListener("update", function () {
      if (!window.htmx) return;
      var before = snapshotStats();
      var p = window.htmx.ajax("GET", body, { target: "#session-body", swap: "innerHTML" });
      var after = function () { rehydrate(); flashChangedStats(before); };
      if (p && typeof p.then === "function") { p.then(after); } else { setTimeout(after, 60); }
    });
  }

  // ---------------- Inline diff rendering ----------------
  function lines(s) {
    var a = String(s).split("\n");
    if (a.length > 1 && a[a.length - 1] === "") a.pop();
    return a;
  }
  function appendLines(body, arr, cls) {
    arr.forEach(function (ln) {
      var span = document.createElement("span");
      span.className = "diff-line " + cls;
      span.textContent = ln;
      body.appendChild(span);
    });
  }
  // hunksFromJSON pulls old/new text out of the editing-tool input shapes across
  // the three agents. Returns null when the body is not a recognizable edit.
  function hunksFromJSON(obj) {
    var file = obj.file_path || obj.path || obj.filePath || "";
    var hunks = [];
    if (Array.isArray(obj.edits)) {
      obj.edits.forEach(function (e) { hunks.push({ del: lines(e.old_string || ""), add: lines(e.new_string || "") }); });
    } else if (obj.old_string !== undefined || obj.new_string !== undefined) {
      hunks.push({ del: lines(obj.old_string || ""), add: lines(obj.new_string || "") });
    } else if (obj.old_str !== undefined || obj.new_str !== undefined) {
      hunks.push({ del: lines(obj.old_str || ""), add: lines(obj.new_str || "") });
    } else if (obj.content !== undefined) {
      hunks.push({ del: [], add: lines(obj.content) });
    } else if (obj.file_text !== undefined) {
      hunks.push({ del: [], add: lines(obj.file_text) });
    } else {
      return null;
    }
    return { file: file, hunks: hunks };
  }
  // Patch-string fallback: a unified-diff / apply_patch body rendered by prefix.
  function patchElement(text) {
    if (!/^[*+-]|@@|\bBegin Patch\b/m.test(text)) return null;
    var wrap = document.createElement("div");
    wrap.className = "diff";
    var body = document.createElement("pre");
    body.className = "diff-body";
    lines(text).forEach(function (ln) {
      var span = document.createElement("span");
      var c = "diff-line";
      if (ln.indexOf("@@") === 0 || /Begin Patch|End Patch/.test(ln)) c += " diff-hunk";
      else if (ln[0] === "+" && ln.indexOf("+++") !== 0) c += " diff-add";
      else if (ln[0] === "-" && ln.indexOf("---") !== 0) c += " diff-del";
      span.className = c;
      span.textContent = ln;
      body.appendChild(span);
    });
    wrap.appendChild(body);
    return wrap;
  }
  function diffElement(toolName, text) {
    var parsed = null;
    try { parsed = JSON.parse(text); } catch (e) {}
    if (parsed && typeof parsed === "object") {
      var hj = hunksFromJSON(parsed);
      if (hj) {
        var wrap = document.createElement("div");
        wrap.className = "diff";
        if (hj.file) {
          var fh = document.createElement("div");
          fh.className = "diff-file";
          fh.textContent = hj.file;
          wrap.appendChild(fh);
        }
        var body = document.createElement("pre");
        body.className = "diff-body";
        hj.hunks.forEach(function (h) { appendLines(body, h.del, "diff-del"); appendLines(body, h.add, "diff-add"); });
        wrap.appendChild(body);
        return wrap;
      }
    }
    return patchElement(text);
  }

  // ---------------- Inspector pane ----------------
  // A selected tool call's bodies open in the right-hand inspector instead of
  // inline, so reading the transcript and inspecting a body never fight for the
  // same column. Triggers are the chip stamps (.body-toggle) and the outline
  // steps (.inspect-open); both resolve to the same view descriptor.
  function inspectorEl() { return document.getElementById("session-inspector"); }

  var inspectorEmptyHTML = "";

  function emptyInspector(insp) {
    if (inspectorEmptyHTML) { insp.innerHTML = inspectorEmptyHTML; }
    clearInspectSelection();
  }
  function resetInspector() {
    var insp = inspectorEl();
    if (!insp) return;
    if (!inspectorEmptyHTML) inspectorEmptyHTML = insp.innerHTML; // capture the server-rendered empty state once
    lastBody = { url: "", res: null }; // drop any retained body on (re)load
    emptyInspector(insp);
  }
  var selectedEl = null;
  function clearInspectSelection() {
    if (selectedEl) { selectedEl.classList.remove("inspect-selected"); selectedEl = null; }
  }

  // describe builds {tool, file, status, views:[{key,label,url,render}], initial}
  // from either a chip stamp or an outline step.
  function describe(trigger) {
    var views = [];
    var tool = "", file = "", status = "", initial = "";
    var diff = trigger.getAttribute("data-diff") === "1";
    if (trigger.classList.contains("body-toggle")) {
      var chip = trigger.closest(".tool-chip");
      if (chip) {
        var tn = chip.querySelector(".tname"); tool = tn ? tn.textContent : "";
        var tp = chip.querySelector(".tpath"); file = tp ? tp.textContent : "";
        var ts = chip.querySelector(".tstatus"); status = ts ? ts.textContent : "";
        var input = chip.querySelector('.body-toggle[data-slot="input"]');
        var result = chip.querySelector('.body-toggle[data-slot="result"]');
        var inputUrl = input ? input.getAttribute("data-blob-url") : "";
        var resultUrl = result ? result.getAttribute("data-blob-url") : "";
        var inputDiff = input ? input.getAttribute("data-diff") === "1" : false;
        views = buildViews(inputUrl, resultUrl, inputDiff);
      }
      initial = trigger.getAttribute("data-slot") === "result" ? "result" : (diff ? "diff" : "input");
    } else {
      tool = trigger.getAttribute("data-tool") || "";
      file = trigger.getAttribute("data-file") || "";
      status = trigger.getAttribute("data-status") || "";
      views = buildViews(trigger.getAttribute("data-input-url") || "", trigger.getAttribute("data-result-url") || "", diff);
    }
    if (!views.length) return null;
    if (!initial || !views.some(function (v) { return v.key === initial; })) initial = views[0].key;
    return { tool: tool, file: file, status: status, views: views, initial: initial };
  }
  function buildViews(inputUrl, resultUrl, inputDiff) {
    var views = [];
    if (inputUrl && inputDiff) views.push({ key: "diff", label: "Diff", url: inputUrl, render: "diff" });
    if (inputUrl) views.push({ key: "input", label: "Input", url: inputUrl, render: "text" });
    if (resultUrl) views.push({ key: "result", label: "Result", url: resultUrl, render: "text" });
    return views;
  }

  // One-entry cache holding only the bounded prefix: re-toggling the same view
  // does not refetch, and clicking through many bodies never retains more than
  // one capped body.
  var lastBody = { url: "", res: null };
  // Cap the text pulled into the page so a huge tool body cannot blow up memory;
  // the rest stays one click away as the raw blob.
  var BODY_DISPLAY_CAP = 200000;

  // fetchBounded streams the blob and stops once it has the display cap, so peak
  // memory tracks the cap rather than the full body size. Falls back to text()
  // where the Streams API is unavailable.
  function fetchBounded(url, cap) {
    return fetch(url, { credentials: "same-origin" }).then(function (r) {
      if (!r.ok) throw new Error("status " + r.status);
      var total = parseInt(r.headers.get("Content-Length") || "", 10);
      if (!r.body || !r.body.getReader || typeof TextDecoder === "undefined") {
        // Fail closed rather than read an input-sized body into memory: offer the
        // raw link instead of an inline preview.
        return { text: "", truncated: true, total: isNaN(total) ? -1 : total };
      }
      var reader = r.body.getReader();
      var decoder = new TextDecoder();
      var acc = "";
      var truncated = false;
      function pump() {
        return reader.read().then(function (res) {
          if (res.done) return;
          acc += decoder.decode(res.value, { stream: true });
          if (acc.length >= cap) {
            truncated = true;
            acc = acc.slice(0, cap);
            return reader.cancel(); // abort the rest; nothing more is buffered
          }
          return pump();
        });
      }
      return pump().then(function () {
        return { text: acc, truncated: truncated, total: isNaN(total) ? -1 : total };
      });
    });
  }

  function loadView(bodyEl, view, toolName) {
    function paint(res) {
      bodyEl.innerHTML = "";
      if (res.text) {
        var node = null;
        if (view.render === "diff" && !res.truncated) node = diffElement(toolName, res.text);
        if (!node) { node = document.createElement("pre"); node.className = "tool-body"; node.textContent = res.text; }
        bodyEl.appendChild(node);
      }
      if (res.truncated) {
        var note = document.createElement("div");
        note.className = "insp-trunc muted";
        var of = res.total > 0 ? " of " + Math.round(res.total / 1000) + " KB" : "";
        note.textContent = "Showing the first " + Math.round(BODY_DISPLAY_CAP / 1000) + " KB" + of + ". ";
        var a = document.createElement("a");
        a.href = view.url; a.target = "_blank"; a.rel = "noopener"; a.textContent = "Open raw";
        note.appendChild(a);
        bodyEl.appendChild(note);
      }
    }
    if (lastBody.url === view.url && lastBody.res) { paint(lastBody.res); return; }
    bodyEl.innerHTML = "";
    var loading = document.createElement("div");
    loading.className = "insp-loading muted"; loading.textContent = "Loading…";
    bodyEl.appendChild(loading);
    fetchBounded(view.url, BODY_DISPLAY_CAP)
      .then(function (res) { lastBody = { url: view.url, res: res }; paint(res); })
      .catch(function () {
        bodyEl.innerHTML = "";
        var pre = document.createElement("pre"); pre.className = "tool-body error";
        pre.textContent = "Could not load body."; bodyEl.appendChild(pre);
      });
  }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  function renderInspector(insp, desc) {
    insp.innerHTML = "";
    var head = el("div", "insp-head");
    head.appendChild(el("span", "insp-tn", desc.tool));
    if (desc.file) head.appendChild(el("span", "insp-file mono", desc.file));
    var close = el("button", "insp-close", "✕");
    close.setAttribute("aria-label", "Close inspector");
    close.addEventListener("click", function () { emptyInspector(insp); });
    head.appendChild(close);
    insp.appendChild(head);

    if (desc.status) {
      var meta = el("div", "insp-meta");
      var st = el("span", "tstatus " + (desc.status === "error" ? "err" : "ok"), desc.status);
      meta.appendChild(st);
      insp.appendChild(meta);
    }

    var body = el("div", "insp-body");
    var views = el("div", "seg-group insp-views");
    desc.views.forEach(function (v) {
      var b = el("button", "seg", v.label);
      b.addEventListener("click", function () {
        Array.prototype.slice.call(views.children).forEach(function (c) { c.classList.remove("active"); });
        b.classList.add("active");
        loadView(body, v, desc.tool);
      });
      views.appendChild(b);
    });
    insp.appendChild(views);
    insp.appendChild(body);

    // Activate the initial view.
    var initialBtn = views.children[desc.views.map(function (v) { return v.key; }).indexOf(desc.initial)] || views.children[0];
    if (initialBtn) initialBtn.click();
  }

  function openInspector(trigger) {
    var insp = inspectorEl();
    if (!insp) return false; // no pane on this page → caller falls back to inline
    if (!inspectorEmptyHTML) inspectorEmptyHTML = insp.innerHTML;
    var desc = describe(trigger);
    if (!desc) return false;
    renderInspector(insp, desc);
    clearInspectSelection();
    selectedEl = trigger.closest(".tool-chip") || trigger;
    selectedEl.classList.add("inspect-selected");
    return true;
  }

  // Inline fallback for pages without an inspector pane (kept minimal).
  function expandInline(btn) {
    if (btn._bodyEl) { btn._bodyEl.remove(); btn._bodyEl = null; btn.classList.remove("open"); return; }
    var url = btn.getAttribute("data-blob-url");
    if (!url) return;
    btn.classList.add("loading");
    fetch(url, { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("status " + r.status); return r.text(); })
      .then(function (text) {
        var node = null;
        if (btn.getAttribute("data-diff") === "1" && btn.getAttribute("data-slot") === "input") {
          node = diffElement(btn.getAttribute("data-tool") || "", text);
        }
        if (!node) { node = el("pre", "tool-body"); node.textContent = text; }
        (btn.closest(".tool-chip") || btn).after(node);
        btn._bodyEl = node; btn.classList.add("open");
      })
      .catch(function () {
        var pre = el("pre", "tool-body error"); pre.textContent = "Could not load body.";
        (btn.closest(".tool-chip") || btn).after(pre); btn._bodyEl = pre;
      })
      .finally(function () { btn.classList.remove("loading"); });
  }

  document.addEventListener("click", function (ev) {
    var trigger = ev.target.closest ? ev.target.closest(".body-toggle, .inspect-open") : null;
    if (!trigger) return;
    // Outline steps are anchors (they also scroll to the message); chip stamps
    // are buttons. Only suppress default for the buttons.
    if (trigger.classList.contains("body-toggle")) ev.preventDefault();
    if (openInspector(trigger)) return;
    if (trigger.classList.contains("body-toggle")) expandInline(trigger);
  });

  // ---------------- Whole-row navigation ----------------
  // A table row carrying data-row-href navigates as a unit, so the whole cell is
  // the hit target (see DESIGN.md: "the whole row is the hit target"). A click
  // that lands on a real control inside the row (a nested link, a button, a
  // field) falls through to that control instead of the row's destination.
  function rowHrefFrom(target) {
    if (!target || !target.closest) return null;
    var tr = target.closest("tr[data-row-href]");
    if (!tr) return null;
    if (target.closest("a, button, input, select, textarea, label, summary")) return null;
    return tr.getAttribute("data-row-href");
  }
  document.addEventListener("click", function (ev) {
    if (ev.defaultPrevented || ev.button !== 0) return;
    var href = rowHrefFrom(ev.target);
    if (!href) return;
    // Honor the usual open-in-new-tab modifiers.
    if (ev.metaKey || ev.ctrlKey || ev.shiftKey) { window.open(href, "_blank"); return; }
    window.location.assign(href);
  });
  document.addEventListener("auxclick", function (ev) {
    if (ev.button !== 1) return; // middle click opens a new tab
    var href = rowHrefFrom(ev.target);
    if (!href) return;
    window.open(href, "_blank");
  });

  // ---------------- Init ----------------
  function init() {
    markDensity(currentDensity());
    animateBars();
    initOutlineSpy();   // once; the spy reads the live DOM and survives swaps
    resetInspector();   // once; the inspector persists across live updates
    initLive();
  }
  // The overview's range selector swaps the usage panel; its replacement bars
  // start at width 0, so grow them in. animateBars guards already-grown bars, so
  // bars elsewhere on the page are left alone.
  document.addEventListener("htmx:afterSwap", function () { animateBars(); });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
