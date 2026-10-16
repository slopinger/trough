// trough's service worker: shows browser notifications pushed by the server
// and opens the post (or the notifications page) when one is clicked.
self.addEventListener("push", function (ev) {
  var d = {};
  try { d = ev.data ? ev.data.json() : {}; } catch (e) {}
  ev.waitUntil(self.registration.showNotification(d.title || "trough", {
    body: d.body || "", icon: d.icon || "/static/favicon.svg", tag: d.tag, data: { url: d.url || "/notifications" }
  }));
});

self.addEventListener("notificationclick", function (ev) {
  ev.notification.close();
  var url = new URL((ev.notification.data && ev.notification.data.url) || "/notifications", self.location.origin).href;
  ev.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (list) {
    for (var i = 0; i < list.length; i++) {
      if (list[i].url === url && "focus" in list[i]) return list[i].focus();
    }
    return self.clients.openWindow(url);
  }));
});
