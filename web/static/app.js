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
        var on = { like: res.liked, boost: res.boosted, bookmark: res.bookmarked }[kind];
        var count = { like: res.likes, boost: res.boosts }[kind];
        var id = form.action.split("/statuses/")[1].split("/")[0];
        // update every copy of this post on the page (it may appear boosted too)
        document.querySelectorAll('form.toggle[data-kind="' + kind + '"]').forEach(function (f) {
          if (f.action.indexOf("/statuses/" + id + "/") < 0) return;
          f.action = "/statuses/" + id + "/" + (on ? "un" : "") + kind;
          var b = f.querySelector("button");
          b.classList.toggle("on", !!on);
          var n = b.querySelector(".n");
          if (n && count !== undefined) n.textContent = count ? String(count) : "";
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

  // scheduling: send the local time as RFC 3339 with the browser's offset
  document.querySelectorAll(".schedule-local").forEach(function (input) {
    var form = input.closest("form");
    var hidden = form.querySelector(".schedule-at");
    var tz = form.querySelector(".schedule-tz");
    if (tz) tz.textContent = "(your local time)";
    var d = new Date(Date.now() + 60 * 60 * 1000);
    input.min = new Date(Date.now() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    input.addEventListener("change", function () {
      hidden.value = input.value ? new Date(input.value).toISOString() : "";
      var b = form.querySelector(".post-btn");
      if (b) b.textContent = input.value ? "Schedule" : (form.querySelector("input[name=in_reply_to]") ? "Reply" : "Post");
    });
  });
  // show times in the viewer's timezone
  document.querySelectorAll("time.localtime").forEach(function (t) {
    t.textContent = new Date(t.getAttribute("datetime")).toLocaleString();
  });

  // custom emoji picker: insert :shortcode: at the cursor
  document.addEventListener("click", function (ev) {
    var b = ev.target.closest && ev.target.closest(".emoji-pick");
    if (!b) return;
    var ta = b.closest("form").querySelector("textarea");
    var s = ta.selectionStart || ta.value.length, e = ta.selectionEnd || s;
    var pad = s > 0 && !/\s/.test(ta.value[s - 1]) ? " " : "";
    ta.value = ta.value.slice(0, s) + pad + b.dataset.code + " " + ta.value.slice(e);
    ta.selectionStart = ta.selectionEnd = s + pad.length + b.dataset.code.length + 1;
    ta.focus();
    ta.dispatchEvent(new Event("input"));
  });

  // post menus: copy link, close when clicking elsewhere
  document.addEventListener("click", function (ev) {
    var copy = ev.target.closest && ev.target.closest(".copy-link");
    if (copy) {
      var done = function () { copy.textContent = "✓ Copied"; };
      if (navigator.clipboard) navigator.clipboard.writeText(copy.dataset.url).then(done, function () { window.prompt("Link:", copy.dataset.url); });
      else window.prompt("Link:", copy.dataset.url);
      return;
    }
    document.querySelectorAll("details.menu[open]").forEach(function (d) {
      if (!d.contains(ev.target)) d.open = false;
    });
  });

  // live updates: "show new posts" banner and the notification badge
  if (document.body.dataset.live && window.fetch) {
    var banner = document.querySelector(".new-posts");
    var badge = document.querySelector(".notif-badge");
    var poll = function () {
      if (document.hidden) return;
      var q = banner && banner.dataset.since ? "?tl=" + banner.dataset.timeline + "&since=" + banner.dataset.since : "";
      fetch("/timeline/new" + q, { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          if (!d) return;
          if (banner && d.new > 0) {
            banner.textContent = d.new === 1 ? "Show 1 new post" : "Show " + d.new + " new posts";
            banner.hidden = false;
          }
          if (badge) {
            badge.textContent = String(d.notifications);
            badge.hidden = !d.notifications;
          }
          document.title = document.title.replace(/^\(\d+\) /, "");
          if (d.notifications) document.title = "(" + d.notifications + ") " + document.title;
        })
        .catch(function () {});
    };
    setInterval(poll, 30000);
    document.addEventListener("visibilitychange", function () { if (!document.hidden) poll(); });
  }

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

  // Compose autocomplete: @accounts, #hashtags and :custom_emoji: while typing.
  document.querySelectorAll("form.compose textarea").forEach(function (ta) {
    var box = document.createElement("ul");
    box.className = "ac-list";
    box.setAttribute("role", "listbox");
    box.hidden = true;
    ta.insertAdjacentElement("afterend", box);
    var items = [], sel = 0, token = null, timer = null, seq = 0;
    function current() {
      var before = ta.value.slice(0, ta.selectionStart);
      var m = /(^|[\s(])([@#:][\w.\-]*(?:@[\w.\-]*)?)$/u.exec(before);
      if (!m || m[2].length < 2 || (m[2][0] === ":" && m[2].length < 3)) return null;
      return { text: m[2], start: before.length - m[2].length };
    }
    function close() { box.hidden = true; items = []; token = null; }
    function render() {
      box.textContent = "";
      items.forEach(function (it, i) {
        var li = document.createElement("li");
        li.setAttribute("role", "option");
        li.className = i === sel ? "on" : "";
        if (it.image) { var img = document.createElement("img"); img.src = it.image; img.alt = ""; img.width = 24; img.height = 24; li.appendChild(img); }
        var b = document.createElement("b"); b.textContent = it.label; li.appendChild(b);
        if (it.sub) { var sub = document.createElement("span"); sub.textContent = it.sub; li.appendChild(sub); }
        li.addEventListener("mousedown", function (ev) { ev.preventDefault(); pick(i); });
        box.appendChild(li);
      });
      box.hidden = items.length === 0;
    }
    function pick(i) {
      var it = items[i];
      if (!it || !token) return;
      var after = ta.value.slice(ta.selectionStart);
      var ins = it.insert + " ";
      ta.value = ta.value.slice(0, token.start) + ins + after.replace(/^\S*/, "");
      var pos = token.start + ins.length;
      ta.setSelectionRange(pos, pos);
      ta.dispatchEvent(new Event("input"));
      close();
      ta.focus();
    }
    ta.addEventListener("input", function () {
      var t = current();
      if (!t) { close(); return; }
      token = t;
      clearTimeout(timer);
      timer = setTimeout(function () {
        var my = ++seq;
        fetch("/autocomplete?q=" + encodeURIComponent(t.text), { credentials: "same-origin" })
          .then(function (r) { return r.ok ? r.json() : []; })
          .then(function (list) {
            if (my !== seq || !token || token.text !== t.text) return;
            items = list || []; sel = 0; render();
          })
          .catch(close);
      }, 120);
    });
    ta.addEventListener("keydown", function (ev) {
      if (box.hidden) return;
      if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
        ev.preventDefault();
        sel = (sel + (ev.key === "ArrowDown" ? 1 : items.length - 1)) % items.length;
        render();
      } else if ((ev.key === "Enter" || ev.key === "Tab") && !ev.ctrlKey && !ev.metaKey) {
        ev.preventDefault();
        ev.stopImmediatePropagation();
        pick(sel);
      } else if (ev.key === "Escape") {
        close();
      }
    }, true);
    ta.addEventListener("blur", function () { setTimeout(close, 150); });
  });

  // Keyboard shortcuts (press ? for the list). Not while typing.
  var shortcuts = [
    ["j / k", "next / previous post"], ["o", "open the post"], ["r", "reply"], ["f", "favourite"],
    ["b", "boost"], ["n", "new post"], ["/", "search"], ["?", "show this help"], ["Esc", "close"]];
  var focused = null;
  function posts() { return Array.prototype.slice.call(document.querySelectorAll("main article.status")); }
  function focusPost(el) {
    if (focused) focused.classList.remove("kbd-focus");
    focused = el;
    if (!el) return;
    el.classList.add("kbd-focus");
    el.scrollIntoView({ block: "center", behavior: "smooth" });
  }
  function help() {
    var d = document.getElementById("kbd-help");
    if (d) { d.remove(); return; }
    d = document.createElement("div");
    d.id = "kbd-help";
    d.setAttribute("role", "dialog");
    d.setAttribute("aria-label", "Keyboard shortcuts");
    var h = document.createElement("h3"); h.textContent = "Keyboard shortcuts"; d.appendChild(h);
    var dl = document.createElement("dl");
    shortcuts.forEach(function (s) {
      var dt = document.createElement("dt"); var k = document.createElement("kbd"); k.textContent = s[0]; dt.appendChild(k);
      var dd = document.createElement("dd"); dd.textContent = s[1];
      dl.appendChild(dt); dl.appendChild(dd);
    });
    d.appendChild(dl);
    d.addEventListener("click", function () { d.remove(); });
    document.body.appendChild(d);
  }
  document.addEventListener("keydown", function (ev) {
    var t = ev.target;
    if (ev.ctrlKey || ev.metaKey || ev.altKey || (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)))) return;
    var list = posts(), i = focused ? list.indexOf(focused) : -1;
    var click = function (sel) { var b = focused && focused.querySelector(sel); if (b && !b.disabled) b.click(); };
    switch (ev.key) {
      case "j": focusPost(list[Math.min(i + 1, list.length - 1)]); break;
      case "k": focusPost(list[Math.max(i - 1, 0)]); break;
      case "o": if (!focused) return; click("a.time"); break;
      case "r": if (!focused) return; click('a.act[title="Reply"]'); break;
      case "f": if (!focused) return; click("button.act.like"); break;
      case "b": if (!focused) return; click("button.act.boost"); break;
      case "n":
        var ta = document.querySelector("form.compose textarea");
        if (ta) ta.focus(); else location.href = "/compose";
        break;
      case "/":
        var q = document.querySelector('input[type=search][name=q]');
        if (!q) return;
        q.focus();
        break;
      case "?": help(); break;
      case "Escape":
        var d = document.getElementById("kbd-help");
        if (d) d.remove(); else focusPost(null);
        break;
      default: return;
    }
    ev.preventDefault();
  });

  // Image viewer: open attached images large, with their description;
  // arrow keys or buttons go through a post's images, Esc or a click closes.
  var viewer = null;
  function closeViewer() { if (viewer) { viewer.remove(); viewer = null; } }
  function openViewer(links, index) {
    closeViewer();
    viewer = document.createElement("div");
    viewer.className = "viewer";
    viewer.setAttribute("role", "dialog");
    viewer.setAttribute("aria-label", "Image");
    var img = document.createElement("img");
    var cap = document.createElement("p");
    var prev = document.createElement("button"), next = document.createElement("button"), close = document.createElement("button");
    prev.className = "viewer-prev"; prev.textContent = "‹"; prev.setAttribute("aria-label", "Previous image");
    next.className = "viewer-next"; next.textContent = "›"; next.setAttribute("aria-label", "Next image");
    close.className = "viewer-close"; close.textContent = "×"; close.setAttribute("aria-label", "Close");
    function show(i) {
      index = (i + links.length) % links.length;
      var a = links[index], thumb = a.querySelector("img");
      img.src = a.href;
      img.alt = thumb ? thumb.alt : "";
      cap.textContent = img.alt;
      cap.hidden = !img.alt;
      prev.hidden = next.hidden = links.length < 2;
    }
    prev.addEventListener("click", function (ev) { ev.stopPropagation(); show(index - 1); });
    next.addEventListener("click", function (ev) { ev.stopPropagation(); show(index + 1); });
    close.addEventListener("click", closeViewer);
    viewer.addEventListener("click", function (ev) { if (ev.target === viewer) closeViewer(); });
    viewer.show = show;
    viewer.step = function (d) { show(index + d); };
    [img, cap, prev, next, close].forEach(function (el) { viewer.appendChild(el); });
    document.body.appendChild(viewer);
    show(index);
    close.focus();
  }
  document.addEventListener("click", function (ev) {
    var a = ev.target.closest && ev.target.closest("a.m-img");
    if (!a || ev.button !== 0 || ev.ctrlKey || ev.metaKey || ev.shiftKey) return;
    ev.preventDefault();
    var links = Array.prototype.slice.call(a.closest(".media").querySelectorAll("a.m-img"));
    openViewer(links, links.indexOf(a));
  });
  document.addEventListener("keydown", function (ev) {
    if (!viewer) return;
    if (ev.key === "Escape") closeViewer();
    else if (ev.key === "ArrowLeft") viewer.step(-1);
    else if (ev.key === "ArrowRight") viewer.step(1);
    else return;
    ev.preventDefault();
    ev.stopImmediatePropagation();
  }, true);

})();
