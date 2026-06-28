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
