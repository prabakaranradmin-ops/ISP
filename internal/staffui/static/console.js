// Operations console — progressive-enhancement layer.
//
// Everything here is a convenience on top of a console that already works
// without it: every form is a plain HTML form, every nav link is a plain
// <a href>, and tab panels are visible (just stacked) with this script
// absent. Nothing in this file is load-bearing for a task to be completable.
(function () {
  "use strict";

  onReady(function () {
    initShortcutsHelp();
    initGoToNav();
    initSearchFocus();
    initTabs();
    initSelectAll();
  });

  function onReady(fn) {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", fn);
    } else {
      fn();
    }
  }

  // ── Shortcuts help panel (?) ────────────────────────────────────────────

  function initShortcutsHelp() {
    var panel = document.getElementById("shortcuts-help");
    if (!panel) return;
    var openBtn = document.getElementById("shortcuts-open");
    var closeBtn = document.getElementById("shortcuts-close");

    function open() { panel.hidden = false; }
    function close() { panel.hidden = true; }

    if (openBtn) openBtn.addEventListener("click", open);
    if (closeBtn) closeBtn.addEventListener("click", close);
    panel.addEventListener("click", function (e) {
      if (e.target === panel) close(); // click on the backdrop
    });

    document.addEventListener("keydown", function (e) {
      if (isTypingTarget(e.target)) return;
      if (e.key === "?") {
        e.preventDefault();
        panel.hidden ? open() : close();
      } else if (e.key === "Escape" && !panel.hidden) {
        close();
      }
    });
  }

  // ── "g" then a letter jumps to a section ───────────────────────────────
  //
  // Only ever navigates to a link actually rendered in this page's nav —
  // that nav already reflects the signed-in role (AllowedSections), so a
  // role that cannot reach a section simply has no shortcut registered for
  // it, rather than this script needing its own copy of the role table.

  function initGoToNav() {
    var links = document.querySelectorAll(".topbar nav a[data-shortcut]");
    if (!links.length) return;
    var byLetter = {};
    links.forEach(function (a) {
      var letter = a.getAttribute("data-shortcut");
      if (letter) byLetter[letter] = a.getAttribute("href");
    });

    var awaitingLetter = false;
    var timer = null;

    document.addEventListener("keydown", function (e) {
      if (isTypingTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      if (awaitingLetter) {
        awaitingLetter = false;
        clearTimeout(timer);
        var href = byLetter[e.key.toLowerCase()];
        if (href) {
          e.preventDefault();
          window.location.href = href;
        }
        return;
      }
      if (e.key === "g") {
        awaitingLetter = true;
        // A stray "g" with no follow-up letter should not leave the
        // listener armed indefinitely waiting for one.
        timer = setTimeout(function () { awaitingLetter = false; }, 1500);
      }
    });
  }

  // ── "/" focuses the page's search box ──────────────────────────────────

  function initSearchFocus() {
    document.addEventListener("keydown", function (e) {
      if (isTypingTarget(e.target)) return;
      if (e.key !== "/") return;
      var input = document.querySelector(".searchbar input");
      if (!input) return;
      e.preventDefault();
      input.focus();
      input.select();
    });
  }

  function isTypingTarget(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
  }

  // ── Tabs ────────────────────────────────────────────────────────────────
  //
  // Markup contract: a container with [data-tabs] holds some number of
  // [data-tab] buttons and matching [data-tab-panel] sections. Panels start
  // fully visible in the HTML (no `hidden` attribute) precisely so a page
  // with this script disabled or failing to load still shows everything —
  // this only narrows to one panel at a time once it has actually run.

  function initTabs() {
    document.querySelectorAll("[data-tabs]").forEach(function (container) {
      var buttons = container.querySelectorAll("[data-tab]");
      var panels = container.querySelectorAll("[data-tab-panel]");
      if (!buttons.length || !panels.length) return;

      function show(name) {
        panels.forEach(function (p) {
          p.hidden = p.getAttribute("data-tab-panel") !== name;
        });
        buttons.forEach(function (b) {
          b.classList.toggle("on", b.getAttribute("data-tab") === name);
        });
      }

      buttons.forEach(function (b) {
        b.addEventListener("click", function () { show(b.getAttribute("data-tab")); });
      });

      show(buttons[0].getAttribute("data-tab"));
    });
  }

  // ── Bulk-select "select all" ────────────────────────────────────────────

  function initSelectAll() {
    var all = document.getElementById("select-all");
    if (!all) return;
    var boxes = document.querySelectorAll('input[name="ids"]');
    all.addEventListener("change", function () {
      boxes.forEach(function (b) { b.checked = all.checked; });
    });
  }
})();
