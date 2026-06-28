// Live session updates: when the server signals new parsed bytes over SSE, swap
// in a fresh session body via htmx. The session ids ride on data attributes so
// this script stays static and the binary self-contained.
document.addEventListener("DOMContentLoaded", function () {
  var el = document.getElementById("session-body");
  if (!el) return;
  var sse = el.getAttribute("data-sse");
  var body = el.getAttribute("data-body");
  if (!sse || !body || !window.EventSource) return;
  var es = new EventSource(sse);
  es.addEventListener("update", function () {
    if (window.htmx) {
      window.htmx.ajax("GET", body, { target: "#session-body", swap: "innerHTML" });
    }
  });
});

// Tool body expansion: a chip carrying data-blob-url fetches its body from the
// CAS on click and shows it inline. The body is inserted with textContent, never
// innerHTML, so a stored body can never inject markup into the page. A second
// click hides it. Delegated from the document so it survives SSE body swaps.
document.addEventListener("click", function (ev) {
  var btn = ev.target.closest ? ev.target.closest(".body-toggle") : null;
  if (!btn) return;
  ev.preventDefault();
  var existing = btn._bodyEl;
  if (existing) {
    existing.remove();
    btn._bodyEl = null;
    btn.classList.remove("open");
    return;
  }
  var url = btn.getAttribute("data-blob-url");
  if (!url) return;
  btn.classList.add("loading");
  fetch(url, { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("status " + r.status);
      return r.text();
    })
    .then(function (text) {
      var pre = document.createElement("pre");
      pre.className = "tool-body";
      pre.textContent = text;
      var chip = btn.closest(".tool-chip");
      (chip || btn).after(pre);
      btn._bodyEl = pre;
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
    .finally(function () {
      btn.classList.remove("loading");
    });
});
