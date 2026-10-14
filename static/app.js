// trough: progressive enhancement. Everything works without JavaScript;
// this only avoids full page reloads for likes/boosts and adds niceties.
(function () {
  "use strict";

  // like / boost without reloading
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (form.classList.contains("confirm")) {
      if (!window.confirm(form.dataset.confirm || "Are you sure?")) {
        ev.preventDefault();
      }
      return;
    }
    if (!form.classList.contains("toggle") || !window.fetch) return;
    ev.preventDefault();
    var btn = form.querySelector("button");
    if (!btn || btn.classList.contains("busy")) return;
    btn.classList.add("busy");
    fetch(form.action, {
      method: "POST",
      body: new FormData(form),
      headers: { "X-Requested-With": "fetch" },
      credentials: "same-origin",
    })
      .then(function (r) { return r.json(); })
      .then(function (res) {
        btn.classList.remove("busy");
        if (res.error) { window.alert(res.error); return; }
        var kind = form.dataset.kind;
        var on = kind === "like" ? res.liked : res.boosted;
        var count = kind === "like" ? res.likes : res.boosts;
        var id = form.action.split("/statuses/")[1].split("/")[0];
        // update every copy of this post on the page (it may appear boosted too)
        document.querySelectorAll('form.toggle[data-kind="' + kind + '"]').forEach(function (f) {
          if (f.action.indexOf("/statuses/" + id + "/") < 0) return;
          f.action = "/statuses/" + id + "/" + (on ? "un" : "") + kind;
          var b = f.querySelector("button");
          b.classList.toggle("on", !!on);
          var n = b.querySelector(".n");
          if (n) n.textContent = count ? String(count) : "";
        });
      })
      .catch(function () {
        btn.classList.remove("busy");
        form.submit();
      });
  });

  // character counter + ctrl/cmd+enter to post
  document.querySelectorAll("form.compose").forEach(function (form) {
    var ta = form.querySelector("textarea");
    var cw = form.querySelector(".cw-input");
    var counter = form.querySelector(".counter");
    var max = parseInt(ta.getAttribute("maxlength"), 10) || 5000;
    ta.removeAttribute("maxlength");
    function update() {
      var left = max - Array.from(ta.value).length;
      counter.textContent = String(left);
      counter.classList.toggle("over", left < 0);
    }
    ta.addEventListener("input", update);
    update();
    ta.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) {
        ev.preventDefault();
        form.requestSubmit ? form.requestSubmit() : form.submit();
      }
    });
    form.addEventListener("submit", function (ev) {
      if (Array.from(ta.value).length > max) {
        ev.preventDefault();
        window.alert("Your post is too long.");
        return;
      }
      var b = form.querySelector("button[type=submit]");
      if (b) { b.disabled = true; b.textContent = "Posting…"; }
    });
    // put the cursor after prefilled @mentions when replying
    if (ta.value && form.querySelector("input[name=in_reply_to]") && document.activeElement === document.body && location.pathname === "/compose") {
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
    if (cw && !cw.value) cw.setAttribute("autocomplete", "off");
  });

  // keep relative timestamps fresh-ish
  function rel(t) {
    var s = (Date.now() - t.getTime()) / 1000;
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    if (s < 604800) return Math.floor(s / 86400) + "d";
    return null;
  }
  setInterval(function () {
    document.querySelectorAll("time[datetime]").forEach(function (el) {
      var r = rel(new Date(el.getAttribute("datetime")));
      if (r) el.textContent = r;
    });
  }, 60000);
})();
