// The service worker of Web Push (#611, spec 14.5). It shows a push as a notification and opens the page
// when it is clicked, and does nothing else: no fetch handler (so it never changes how a page loads or
// what is cached), no pushsubscriptionchange handler (the worker has no CSRF token to tell the server a new
// subscription; the Settings page notices a subscription the server does not know and offers Subscribe
// again, push.js "gone").
//
// The server serves this file at <base>/sw.js (not under /static/), so its scope is the whole app,
// including the start_url of the installed app. The notification is built by notify.js toNotification,
// the function the open tabs use: the same tag ("tasmofleet-<event id>"), so one event is one notification
// whether the tab or the push shows it first. notify.js is imported by its plain URL (revalidated with its
// ETag), never the ?v= one that browsers keep for a year: an upgraded TasmoFleet must not run an old
// mapping in the worker.
importScripts("static/notify.js");

// show is the notification of a push the payload of which is not the one the server sends (it never is
// here). A push must show something (userVisibleOnly), so say that rather than let the browser show its own.
function fallback() {
  return {
    title: "TasmoFleet",
    options: {
      body: "A notification arrived that could not be shown.",
      tag: "tasmofleet-unreadable",
    },
  };
}

self.addEventListener("push", function (event) {
  var raw = null;
  try {
    raw = event.data ? event.data.json() : null;
  } catch (e) {
    raw = null;
  }
  var n = self.tasmofleetNotify.toNotification(raw) || fallback();
  event.waitUntil(self.registration.showNotification(n.title, n.options));
});

// target is the page a click opens: the path of the payload (it carries the base path), only on this
// origin; the app's root otherwise.
function target(path) {
  try {
    var u = new URL(path || "", self.location.origin);
    if (path && u.origin === self.location.origin) {
      return u.href;
    }
  } catch (e) {
    // not a URL: the root below
  }
  return self.registration.scope;
}

self.addEventListener("notificationclick", function (event) {
  event.notification.close();
  var url = target(event.notification.data && event.notification.data.path);
  event.waitUntil(
    self.clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then(function (list) {
        for (var i = 0; i < list.length; i++) {
          if (list[i].url === url && "focus" in list[i]) {
            return list[i].focus();
          }
        }
        return self.clients.openWindow(url);
      }),
  );
});
