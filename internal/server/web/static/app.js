// akari UI behavior: live session updates, the tool-body modal with diff rendering
// for editing tools, the timeline-rail scroll spy, and the instrument needle-settle
// on live stat changes. Kept dependency-free and static so the binary stays
// self-contained. Bodies are always inserted with textContent, never innerHTML, so
// a stored tool body can never inject markup.
(function () {
  "use strict";

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
  // indexes, nothing whose cost or memory grows with the session. Set up once;
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
    // Serialize body refreshes: an SSE burst (several "update" events while a fetch
    // is in flight) must not start overlapping htmx.ajax calls, whose swaps could
    // land out of order and leave the transcript showing a stale snapshot. Run at
    // most one refresh at a time and collapse any events that arrive mid-flight
    // into a single trailing refresh, so the final swap always reflects the latest
    // state without a swap-per-event pile-up.
    var fetching = false;
    var pending = false;
    function refresh() {
      if (!window.htmx) return;
      if (fetching) { pending = true; return; }
      fetching = true;
      var before = snapshotStats();
      var done = function () {
        fetching = false;
        rehydrate();
        flashChangedStats(before);
        if (pending) { pending = false; refresh(); }
      };
      var p = window.htmx.ajax("GET", body, { target: "#session-body", swap: "innerHTML" });
      if (p && typeof p.then === "function") { p.then(done, done); } else { setTimeout(done, 60); }
    }
    es.addEventListener("update", refresh);
  }

  // ---------------- Reparse progress ----------------
  // While the server rebuilds the parsed projection, parsed views are gated and
  // show a progress card. The watch reads live status off the SSE stream (with a
  // poll fallback) and reloads the page when the in-progress state flips from what
  // the page rendered: a gated page returns to its real view when the reparse
  // finishes, and the account page swaps idle for progress when one starts.
  function initReparseWatch() {
    var root = document.querySelector("[data-reparse-watch]");
    if (!root) return;
    var initial = root.getAttribute("data-reparse-initial") === "1";
    var reloaded = false;
    function num(sel, v) {
      var el = root.querySelector(sel);
      if (el) el.textContent = String(v);
    }
    function apply(status) {
      if (reloaded || !status) return;
      var fill = root.querySelector("[data-reparse-fill]");
      if (fill && status.total > 0) {
        fill._done = true; // claim it from animateBars so the two never fight
        fill.style.width = Math.min(100, Math.round((status.done * 100) / status.total)) + "%";
      }
      num("[data-reparse-done]", status.done || 0);
      num("[data-reparse-total]", status.total || 0);
      num("[data-reparse-failed]", status.failed || 0);
      if (!!status.in_progress !== initial) {
        reloaded = true;
        window.location.reload();
      }
    }
    var sseUrl = root.getAttribute("data-sse");
    if (sseUrl && window.EventSource) {
      var es = new EventSource(sseUrl);
      es.addEventListener("status", function (ev) {
        try { apply(JSON.parse(ev.data)); } catch (e) {}
      });
    }
    var statusUrl = root.getAttribute("data-status");
    if (statusUrl) {
      var timer = setInterval(function () {
        if (reloaded) { clearInterval(timer); return; }
        fetch(statusUrl, { credentials: "same-origin" })
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(apply)
          .catch(function () {});
      }, 3000);
    }
  }

  // The logged-out public session view has no credential to watch the status
  // stream, so its reparse stand-in just reloads on a timer until the real view
  // returns.
  function initReparsePublic() {
    if (!document.querySelector("[data-reparse-public]")) return;
    setTimeout(function () { window.location.reload(); }, 5000);
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

  // ---------------- Tool-body modal ----------------
  // A selected tool call's bodies open in a large centered dialog instead of a
  // docked column, so reading a body has real room and the transcript keeps the
  // full width when nothing is open. Triggers are the chip stamps (.body-toggle)
  // and the outline steps (.inspect-open); both resolve to the same view
  // descriptor. The overlay starts hidden; #session-inspector is the dialog whose
  // contents are built per open and cleared on close.
  function overlayEl() { return document.getElementById("session-modal"); }
  function inspectorEl() { return document.getElementById("session-inspector"); }

  // The dialog declares aria-modal, which promises assistive tech that focus is
  // constrained to it. Honor that: remember what had focus, move focus into the
  // dialog on open, keep Tab cycling inside it, and restore focus to the trigger
  // on close. focusables lists the dialog's tabbable controls in DOM order.
  var lastFocused = null;
  function focusables() {
    var insp = inspectorEl();
    if (!insp) return [];
    return Array.prototype.slice.call(
      insp.querySelectorAll('a[href], button, [tabindex]:not([tabindex="-1"])')
    ).filter(function (el) { return !el.disabled && el.offsetParent !== null; });
  }
  function trapTab(ev) {
    if (ev.key !== "Tab") return;
    var ov = overlayEl();
    if (!ov || ov.hidden) return;
    var f = focusables();
    if (!f.length) { ev.preventDefault(); return; }
    var first = f[0], last = f[f.length - 1];
    // The dialog container itself carries tabindex -1 and is not in the tabbable
    // list, so when it holds focus (right after open) indexOf is -1. Treat that as
    // the top edge: Shift+Tab there wraps to the last control instead of falling out
    // of the dialog to the page behind it, and Tab there enters at the first.
    var idx = f.indexOf(document.activeElement);
    if (ev.shiftKey && idx <= 0) {
      ev.preventDefault(); last.focus();
    } else if (!ev.shiftKey && (idx === -1 || idx === f.length - 1)) {
      ev.preventDefault(); first.focus();
    }
  }

  function openModal() {
    var ov = overlayEl();
    if (!ov) return;
    lastFocused = document.activeElement;
    ov.hidden = false;
    document.body.classList.add("modal-open");
    // Move focus into the dialog so the keyboard and screen-reader user lands in
    // the opened body rather than staying behind it. The dialog itself takes focus
    // (tabindex -1) so its aria-label is announced; Tab then reaches the controls.
    var insp = inspectorEl();
    if (insp) { insp.setAttribute("tabindex", "-1"); insp.focus(); }
  }
  function closeModal() {
    var ov = overlayEl();
    if (!ov) return;
    ov.hidden = true;
    document.body.classList.remove("modal-open");
    var insp = inspectorEl();
    if (insp) insp.innerHTML = "";
    lastBody = { url: "", res: null }; // drop the retained body so it is refetched next open
    clearInspectSelection();
    // Return focus to the element that opened the modal, if it is still in the DOM,
    // so keyboard focus is not dumped at the top of the document.
    if (lastFocused && lastFocused.focus && lastFocused.isConnected) lastFocused.focus();
    lastFocused = null;
  }
  var selectedEl = null;
  function clearInspectSelection() {
    if (selectedEl) { selectedEl.classList.remove("inspect-selected"); selectedEl = null; }
  }

  // describe builds {tool, file, detail, status, views:[{key,label,url,render}], initial}
  // from either a chip stamp or an outline step.
  function describe(trigger) {
    var views = [];
    var tool = "", file = "", detail = "", status = "", initial = "";
    var diff = trigger.getAttribute("data-diff") === "1";
    if (trigger.classList.contains("body-toggle")) {
      var chip = trigger.closest(".tool-chip");
      if (chip) {
        var tn = chip.querySelector(".tname"); tool = tn ? tn.textContent : "";
        var tp = chip.querySelector(".tpath"); file = tp ? tp.textContent : "";
        // The chip's title attribute carries the full Detail text; the span's
        // own textContent is the truncated display label, not what we want here.
        var td = chip.querySelector(".tdetail"); detail = td ? td.getAttribute("title") : "";
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
      detail = trigger.getAttribute("data-detail") || "";
      status = trigger.getAttribute("data-status") || "";
      views = buildViews(trigger.getAttribute("data-input-url") || "", trigger.getAttribute("data-result-url") || "", diff);
    }
    if (!views.length) return null;
    if (!initial || !views.some(function (v) { return v.key === initial; })) initial = views[0].key;
    return { tool: tool, file: file, detail: detail, status: status, views: views, initial: initial };
  }
  function buildViews(inputUrl, resultUrl, inputDiff) {
    var views = [];
    if (inputUrl && inputDiff) views.push({ key: "diff", label: "Diff", url: inputUrl, render: "diff" });
    if (inputUrl) views.push({ key: "input", label: "Input", url: inputUrl, render: "text" });
    if (resultUrl) views.push({ key: "result", label: "Output", url: resultUrl, render: "text" });
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
    if (desc.detail) head.appendChild(el("span", "insp-detail mono", desc.detail));
    head.appendChild(el("span", "insp-spacer"));
    // The status reads in the header rather than on its own row: a small signal
    // word (Sage ok, Rose error) sat beside the close control.
    if (desc.status) {
      head.appendChild(el("span", "insp-status tstatus " + (desc.status === "error" ? "err" : "ok"), desc.status));
    }
    var close = el("button", "insp-close", "✕");
    close.setAttribute("aria-label", "Close");
    close.addEventListener("click", closeModal);
    head.appendChild(close);
    insp.appendChild(head);

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
    if (!insp) return false; // no modal on this page → caller falls back to inline
    var desc = describe(trigger);
    if (!desc) return false;
    renderInspector(insp, desc);
    openModal();
    clearInspectSelection();
    selectedEl = trigger.closest(".tool-chip") || trigger;
    selectedEl.classList.add("inspect-selected");
    return true;
  }

  // Inline fallback for pages without a modal (kept minimal).
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

  // Dismiss the modal on a backdrop click (the overlay itself, not the dialog) or
  // Escape. Both no-op when the overlay is already hidden.
  document.addEventListener("click", function (ev) {
    if (ev.target === overlayEl()) closeModal();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") {
      var ov = overlayEl();
      if (ov && !ov.hidden) closeModal();
      return;
    }
    trapTab(ev);
  });

  // ---------------- Whole-row navigation ----------------
  // A row carrying data-row-href navigates as a unit, so the whole row is the hit
  // target (see DESIGN.md: "the whole row is the hit target"). This covers both the
  // project page's table rows (<tr data-row-href>) and the Sessions feed's row divs
  // (.srow), so the selector matches any element, not just <tr>. A click that lands
  // on a real control inside the row (a nested link, a button, a field) falls
  // through to that control instead of the row's destination.
  function rowHrefFrom(target) {
    if (!target || !target.closest) return null;
    var row = target.closest("[data-row-href]");
    if (!row) return null;
    if (target.closest("a, button, input, select, textarea, label, summary")) return null;
    return row.getAttribute("data-row-href");
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

  // ---------------- Auto-submitting filter selects ----------------
  // The Sessions toolbar is a GET form whose selects carry data-autosubmit: picking
  // a value submits the form (a full navigation), so the toolbar, the active-filter
  // chips, and the feed all re-render in agreement. A no-script Apply button covers
  // the case where this never runs.
  document.addEventListener("change", function (e) {
    var el = e.target;
    if (!el || !el.matches || !el.matches("select[data-autosubmit]")) return;
    var form = el.form;
    if (!form) return;
    if (form.requestSubmit) form.requestSubmit();
    else form.submit();
  });

  // ---------------- Notice banner ----------------
  // The one-shot success banner (see layout.templ) removes itself on a dismiss
  // click and auto-removes after a few seconds. Under prefers-reduced-motion the
  // fade transition is disabled in CSS (.notice-out), so adding the class there
  // removes it instantly instead of animating; either way the DOM removal itself
  // is immediate once the fade (or lack of one) has had a frame to apply.
  var NOTICE_AUTO_DISMISS_MS = 6000;
  function dismissNotice(el) {
    if (!el || el._dismissed) return;
    el._dismissed = true;
    el.classList.add("notice-out");
    var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    var remove = function () { if (el.parentNode) el.parentNode.removeChild(el); };
    if (reduced) { remove(); return; }
    el.addEventListener("transitionend", remove, { once: true });
    setTimeout(remove, 400); // fallback in case transitionend never fires
  }
  function initNotice() {
    var el = document.querySelector("[data-notice]");
    if (!el) return;
    setTimeout(function () { dismissNotice(el); }, NOTICE_AUTO_DISMISS_MS);
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest("[data-notice-dismiss]") : null;
    if (!btn) return;
    dismissNotice(btn.closest("[data-notice]"));
  });

  // ---------------- Init ----------------
  function init() {
    animateBars();
    initOutlineSpy();   // once; the spy reads the live DOM and survives swaps
    initLive();
    initReparseWatch();
    initReparsePublic();
    initNotice();
  }
  // ---------------- Overview user filter ----------------
  // The per-user filter is a <details> that lives inside #usage, so every range or
  // user-toggle swap replaces it and it would otherwise snap shut mid-selection.
  // Track its open state (the toggle event fires on the element; capture catches it
  // since it does not bubble) and reapply it after the swap, so picking several
  // users keeps the menu open. A click outside an open menu, or Escape, closes it.
  var userFilterOpen = false;
  document.addEventListener("toggle", function (e) {
    var d = e.target;
    if (d && d.classList && d.classList.contains("userfilter")) userFilterOpen = d.open;
  }, true);
  document.addEventListener("click", function (e) {
    var open = document.querySelector(".userfilter[open]");
    if (!open) return;
    if (e.target.closest && e.target.closest(".userfilter")) return; // a click inside keeps it open
    open.open = false;
    userFilterOpen = false;
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var open = document.querySelector(".userfilter[open]");
    if (open) { open.open = false; userFilterOpen = false; }
  });

  // Re-grow the bars after a range or filter swaps a panel by outerHTML: the usage
  // panel (#usage), the insights section (#insights), or the project view
  // (#project-view). The replacement bars start at width 0, so they need
  // re-animating. Gate O(1) on the swapped target's id so live transcript appends
  // (which swap #session-body) do no work here, then animate the live node by id (an
  // outerHTML swap's target is the detached old node). The usage swap also replaces
  // the per-user filter, so reopen it when it was open before.
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = (e.detail && e.detail.target) || e.target;
    if (!t || (t.id !== "usage" && t.id !== "insights" && t.id !== "project-view")) return;
    var root = document.getElementById(t.id);
    if (!root) return;
    animateBars(root);
    if (t.id === "usage" && userFilterOpen) {
      var d = root.querySelector(".userfilter");
      if (d) d.open = true;
    }
  });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
