// Progressive enhancement for the user guide docs pages: copy the page as
// Markdown, the mobile navigation drawer, and the table-of-contents scroll-spy.
// The page reads fine without any of it; this only adds affordances.
(function () {
  "use strict";
  var body = document.body;

  // ---- Copy page as Markdown -------------------------------------------
  // The exact served Markdown sits in a hidden textarea; reading .value decodes
  // the HTML entities, so the clipboard gets the source byte-for-byte.
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest && ev.target.closest("[data-copy-page]");
    if (!btn) return;
    var src = document.getElementById("guide-page-markdown");
    var label = btn.querySelector("[data-copy-label]");
    if (!src || !navigator.clipboard) return;
    navigator.clipboard.writeText(src.value).then(function () {
      btn.setAttribute("data-copied", "");
      if (label) label.textContent = "Copied";
      setTimeout(function () {
        btn.removeAttribute("data-copied");
        if (label) label.textContent = "Copy page";
      }, 1600);
    });
  });

  // ---- Mobile navigation drawer ----------------------------------------
  function setNav(open) {
    body.classList.toggle("is-guide-nav-open", open);
    var toggle = document.querySelector("[data-guide-toggle]");
    if (toggle) toggle.setAttribute("aria-expanded", String(open));
  }
  document.addEventListener("click", function (ev) {
    if (ev.target.closest && ev.target.closest("[data-guide-toggle]")) {
      setNav(!body.classList.contains("is-guide-nav-open"));
    } else if (ev.target.closest && ev.target.closest("[data-guide-close]")) {
      setNav(false);
    } else if (ev.target.closest && ev.target.closest("#guide-sidebar a")) {
      setNav(false);
    }
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") setNav(false);
  });

  // ---- Table-of-contents scroll-spy ------------------------------------
  var tocLinks = Array.prototype.slice.call(
    document.querySelectorAll("[data-guide-toc]")
  );
  if (tocLinks.length && "IntersectionObserver" in window) {
    var active = "";
    function mark(id) {
      if (id === active) return;
      active = id;
      tocLinks.forEach(function (l) {
        l.classList.toggle("is-active", l.getAttribute("data-guide-toc") === id);
      });
    }
    var targets = tocLinks
      .map(function (l) {
        return document.getElementById(l.getAttribute("data-guide-toc"));
      })
      .filter(Boolean);
    var spy = new IntersectionObserver(
      function (entries) {
        var visible = entries
          .filter(function (e) {
            return e.isIntersecting;
          })
          .sort(function (a, b) {
            return a.boundingClientRect.top - b.boundingClientRect.top;
          });
        if (visible.length) mark(visible[0].target.id);
      },
      { rootMargin: "-64px 0px -70% 0px", threshold: 0 }
    );
    targets.forEach(function (t) {
      spy.observe(t);
    });
  }
})();
