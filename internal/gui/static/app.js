/* mirafleet admin GUI: progressive enhancement only (docs/M4.md 2.7).
   Allowed: copy-to-clipboard buttons, the registration-code countdown, relative-age refresh.
   No eval, no inline handlers, no dependencies. Every flow works without this file. */
(function () {
  "use strict";

  var root = document.documentElement;
  root.classList.add("js");

  // --- Copy buttons: <button data-copy="ID">, copies the text of the element with that id. ---
  function selectText(el) {
    var sel = window.getSelection();
    var range = document.createRange();
    range.selectNodeContents(el);
    sel.removeAllRanges();
    sel.addRange(range);
  }

  function flashLabel(btn, text) {
    var old = btn.getAttribute("data-label") || btn.textContent;
    btn.setAttribute("data-label", old);
    btn.textContent = text;
    window.setTimeout(function () { btn.textContent = old; }, 1800);
  }

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest("[data-copy]") : null;
    if (!btn) { return; }
    var src = document.getElementById(btn.getAttribute("data-copy"));
    if (!src) { return; }
    var text = src.textContent.trim();
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () { flashLabel(btn, "Copied"); },
        function () { selectText(src); flashLabel(btn, "Press Ctrl+C"); }
      );
    } else {
      selectText(src);
      flashLabel(btn, "Press Ctrl+C");
    }
  });

  // --- Countdown: <span data-countdown="SECONDS" data-expired-text="...">. ---
  // The server renders the static number; this replaces it with a live one.
  function startCountdowns(scope) {
    var els = scope.querySelectorAll("[data-countdown]:not([data-running])");
    Array.prototype.forEach.call(els, function (el) {
      var left = parseInt(el.getAttribute("data-countdown"), 10);
      if (isNaN(left)) { return; }
      el.setAttribute("data-running", "1");
      var end = Date.now() + left * 1000;
      var timer = window.setInterval(function () {
        var s = Math.max(0, Math.round((end - Date.now()) / 1000));
        if (!document.body.contains(el)) { window.clearInterval(timer); return; }
        if (s === 0) {
          el.textContent = el.getAttribute("data-expired-text") || "expired";
          el.classList.add("expired");
          window.clearInterval(timer);
        } else {
          el.textContent = s + "s";
        }
      }, 1000);
    });
  }

  // --- Relative age: <time datetime="ISO" data-rel>. The server renders the first text. ---
  function ago(ms) {
    var future = ms < 0;
    var s = Math.floor(Math.abs(ms) / 1000);
    var out;
    if (s < 5) { return "just now"; }
    if (s < 60) { out = s + "s"; }
    else if (s < 3600) { out = Math.floor(s / 60) + "m"; }
    else if (s < 86400) { out = Math.floor(s / 3600) + "h"; }
    else { out = Math.floor(s / 86400) + "d"; }
    return future ? "in " + out : out + " ago";
  }

  function refreshTimes(scope) {
    var els = scope.querySelectorAll("time[data-rel][datetime]");
    var now = Date.now();
    Array.prototype.forEach.call(els, function (el) {
      var t = Date.parse(el.getAttribute("datetime"));
      if (!isNaN(t)) { el.textContent = ago(now - t); }
    });
  }

  function init(scope) {
    startCountdowns(scope);
    refreshTimes(scope);
  }

  init(document);
  document.addEventListener("htmx:afterSwap", function () { init(document); });
  window.setInterval(function () { refreshTimes(document); }, 30000);
})();
