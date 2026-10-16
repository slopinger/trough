// Fits embedded trough posts (iframes with class "trough-embed") to their content.
(function () {
  if (window.troughEmbeds) return;
  window.troughEmbeds = true;
  window.addEventListener("message", function (ev) {
    var d = ev.data;
    if (!d || d.type !== "trough:height" || typeof d.height !== "number") return;
    document.querySelectorAll("iframe.trough-embed").forEach(function (f) {
      if (f.contentWindow === ev.source) f.height = String(Math.min(Math.max(Math.ceil(d.height), 50), 5000));
    });
  });
})();
