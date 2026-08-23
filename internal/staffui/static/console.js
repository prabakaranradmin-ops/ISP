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
    initNasSecretGenerator();
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

  // ── NAS registration helper (Routers screen) ────────────────────────────
  //
  // Generates the RADIUS secret client-side (Web Crypto, never touches the
  // server until "Register device" is actually submitted) and fills it
  // straight into the field, then builds the matching router-side setup
  // commands from whatever else is already in the form plus the exact host
  // this page was loaded from — the address a router configured to point
  // here would actually use. This is the browser-native equivalent of
  // scripts/windows/new_nas_registration.ps1; see that script's own
  // comments for why the secret is generated rather than typed twice.

  function initNasSecretGenerator() {
    var genBtn = document.getElementById("nas-generate-secret");
    var secretInput = document.getElementById("nas-radius-secret");
    if (!genBtn || !secretInput) return;

    var form = genBtn.closest("form");
    var commandsBox = document.getElementById("nas-router-commands");
    var commandsText = document.getElementById("nas-router-commands-text");
    var copyBtn = document.getElementById("nas-copy-commands");

    genBtn.addEventListener("click", function () {
      var secret = randomSecret(24);
      secretInput.value = secret;
      if (!commandsBox || !commandsText) return;

      var vendorField = form.querySelector('[name="vendor"]');
      var vendor = vendorField ? vendorField.value : "";
      var coaField = form.querySelector('[name="coa_port"]');
      var coaPort = (coaField && coaField.value.trim()) || "1700";
      var serverAddress = window.location.hostname;

      commandsText.textContent = vendor === "mikrotik"
        ? mikrotikCommands(serverAddress, secret, coaPort)
        : genericNote(vendor, serverAddress, secret, coaPort);
      commandsBox.hidden = false;
    });

    if (copyBtn) {
      copyBtn.addEventListener("click", function () {
        if (!(navigator.clipboard && navigator.clipboard.writeText)) return;
        navigator.clipboard.writeText(commandsText.textContent).then(function () {
          var original = copyBtn.textContent;
          copyBtn.textContent = "Copied";
          setTimeout(function () { copyBtn.textContent = original; }, 1500);
        });
      });
    }
  }

  function mikrotikCommands(serverAddress, secret, coaPort) {
    return [
      "/radius",
      "add service=ppp,hotspot address=" + serverAddress + " secret=" + secret +
        " authentication-port=1812 accounting-port=1813 timeout=3s",
      "",
      "/ppp aaa",
      "set use-radius=yes",
      "",
      "/radius incoming",
      "set accept=yes port=" + coaPort,
      "",
      "# If this router also does hotspot/Wi-Fi (not just PPPoE):",
      "# /ip hotspot profile",
      "# set [find] use-radius=yes",
      "# /ip hotspot walled-garden",
      "# add dst-host=" + serverAddress
    ].join("\n");
  }

  function genericNote(vendor, serverAddress, secret, coaPort) {
    return "Ready-made commands are only available for MikroTik right now.\n" +
      "The generated secret above still applies - configure this " + (vendor || "router") + "'s\n" +
      "own RADIUS client by hand with:\n\n" +
      "  RADIUS server:       " + serverAddress + "\n" +
      "  shared secret:       " + secret + "\n" +
      "  authentication port: 1812\n" +
      "  accounting port:     1813\n" +
      "  CoA/disconnect port: " + coaPort;
  }

  // randomSecret avoids Math.random (not cryptographically secure) in
  // favour of the Web Crypto API, available in every browser this console
  // targets since it already requires HTTPS.
  function randomSecret(length) {
    var alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
    var bytes = new Uint8Array(length);
    window.crypto.getRandomValues(bytes);
    var out = "";
    for (var i = 0; i < length; i++) {
      out += alphabet[bytes[i] % alphabet.length];
    }
    return out;
  }
})();
