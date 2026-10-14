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
      var b = form.querySelector(".post-btn");
      if (b) { b.disabled = true; b.textContent = "Posting…"; }
    });
    // put the cursor after prefilled @mentions when replying
    if (ta.value && form.querySelector("input[name=in_reply_to]") && document.activeElement === document.body && location.pathname === "/compose") {
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
    if (cw && !cw.value) cw.setAttribute("autocomplete", "off");
  });

  // AI drafting: stream the draft into the textarea (server-sent events).
  // Without JS the AI buttons submit the form and the page comes back filled in.
  document.querySelectorAll("form.compose").forEach(function (form) {
    var ai = form.querySelector(".ai");
    if (!ai || !window.fetch || !window.TextDecoder || !window.ReadableStream) return;
    var ta = form.querySelector("textarea");
    var input = ai.querySelector(".ai-input");
    var status = ai.querySelector(".ai-status");
    var undo = ai.querySelector(".ai-undo");
    var retry = ai.querySelector(".ai-retry");
    var note = ai.querySelector(".ai-note");
    var buttons = form.querySelectorAll('button[formaction="/compose/ai"]');
    var before = null, last = null, busy = false;
    var mentionPrefix = /^(\s*@[A-Za-z0-9_.\-]+(@[A-Za-z0-9.\-:]+)?)+\s*/;

    function hasText() { return ta.value.replace(mentionPrefix, "").trim() !== ""; }
    function refresh() { ai.classList.toggle("has-text", hasText()); }
    ta.addEventListener("input", refresh);
    refresh();

    function setStatus(text, cls) {
      status.textContent = text;
      status.className = "ai-status" + (cls ? " " + cls : "");
      note.hidden = !!text;
    }
    function setBusy(b) {
      busy = b;
      buttons.forEach(function (x) { x.disabled = b; });
      ta.readOnly = b;
      form.classList.toggle("ai-busy", b);
    }

    input.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter") {
        ev.preventDefault();
        ai.querySelector(".ai-go").click();
      }
    });
    undo.addEventListener("click", function () {
      if (before !== null) ta.value = before;
      ta.dispatchEvent(new Event("input"));
      undo.hidden = retry.hidden = true;
      setStatus("");
      ta.focus();
    });
    retry.addEventListener("click", function () {
      if (!last) return;
      ta.value = last.draft;
      run(last.btn, last.draft);
    });

    function run(btn, draft) {
      var fd = new FormData(form);
      fd.set(btn.name, btn.value);
      fd.set("text", draft);
      for (var i = 0; i < 4; i++) fd.delete("media" + i);
      last = { btn: btn, draft: draft };
      before = draft;
      undo.hidden = retry.hidden = true;
      setBusy(true);
      setStatus(btn.value === "lucky" ? "Rolling the dice…" : "Asking the AI…", "busy");
      var prefix = "", got = "", finished = false;
      function handle(m) {
        if (m.error) {
          ta.value = before;
          finished = true;
          setStatus(m.error, "err");
          retry.hidden = false;
        } else if (m.start) {
          prefix = m.prefix || "";
        } else if (m.thinking) {
          setStatus("Thinking…", "busy");
        } else if (m.delta) {
          if (!got) setStatus("Writing…", "busy");
          got += m.delta;
          ta.value = prefix + got;
          ta.scrollTop = ta.scrollHeight;
          ta.dispatchEvent(new Event("input"));
        } else if (m.done) {
          finished = true;
          ta.value = m.text;
          setStatus("✓ Draft ready — edit it, then post.");
          undo.hidden = retry.hidden = false;
          if (btn.name === "ai_mode" && (btn.value === "auto" || btn.value === "write" || btn.value === "reply")) input.value = "";
        }
      }
      fetch("/compose/ai", { method: "POST", body: fd, credentials: "same-origin", headers: { "X-Requested-With": "fetch" } })
        .then(function (resp) {
          if (!resp.ok || !resp.body) throw new Error("HTTP " + resp.status);
          var reader = resp.body.getReader();
          var dec = new TextDecoder();
          var buf = "";
          function pump() {
            return reader.read().then(function (r) {
              if (r.done) return;
              buf += dec.decode(r.value, { stream: true });
              var i;
              while ((i = buf.indexOf("\n\n")) >= 0) {
                var evt = buf.slice(0, i);
                buf = buf.slice(i + 2);
                evt.split("\n").forEach(function (line) {
                  if (line.indexOf("data:") === 0) {
                    try { handle(JSON.parse(line.slice(5))); } catch (e) { /* ignore */ }
                  }
                });
              }
              return pump();
            });
          }
          return pump();
        })
        .catch(function () {
          ta.value = before;
          setStatus("Could not reach the server.", "err");
          retry.hidden = false;
          finished = true;
        })
        .then(function () {
          if (!finished) {
            ta.value = before;
            setStatus("The draft was interrupted.", "err");
            retry.hidden = false;
          }
          setBusy(false);
          ta.dispatchEvent(new Event("input"));
          ta.focus();
        });
    }

    buttons.forEach(function (btn) {
      btn.addEventListener("click", function (ev) {
        ev.preventDefault();
        if (busy) return;
        if (btn.value === "auto" && !input.value.trim() && !hasText() && !ai.dataset.reply) {
          setStatus("Type what the post should be about, or try 🎲 I'm feeling lucky.", "err");
          input.focus();
          return;
        }
        run(btn, ta.value);
      });
    });
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
