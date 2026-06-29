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

  // ---------------- Timeline rail scroll spy ----------------
  var railObserver = null;
  function initRailSpy() {
    if (railObserver) { railObserver.disconnect(); railObserver = null; }
    var msgs = document.querySelectorAll(".transcript .msg[data-ordinal]");
    if (!msgs.length || !("IntersectionObserver" in window)) return;
    railObserver = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (!e.isIntersecting) return;
        var ord = e.target.getAttribute("data-ordinal");
        Array.prototype.slice.call(document.querySelectorAll(".rail-tick")).forEach(function (t) {
          t.classList.toggle("current", t.getAttribute("data-rail") === ord);
        });
      });
    }, { rootMargin: "-45% 0px -50% 0px", threshold: 0 });
    Array.prototype.slice.call(msgs).forEach(function (m) { railObserver.observe(m); });
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

  function rehydrate() {
    animateBars();
    initRailSpy();
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

  // ---------------- Tool body expansion ----------------
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest(".body-toggle") : null;
    if (!btn) return;
    ev.preventDefault();
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
        if (!node) {
          node = document.createElement("pre");
          node.className = "tool-body";
          node.textContent = text;
        }
        var chip = btn.closest(".tool-chip");
        (chip || btn).after(node);
        btn._bodyEl = node;
        btn.classList.add("open");
      })
      .catch(function () {
        var pre = document.createElement("pre");
        pre.className = "tool-body error";
        pre.textContent = "Could not load body.";
        var chip = btn.closest(".tool-chip");
        (chip || btn).after(pre);
        btn._bodyEl = pre;
      })
      .finally(function () { btn.classList.remove("loading"); });
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
    rehydrate();
    initLive();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
