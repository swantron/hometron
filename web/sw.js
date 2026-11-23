// hometron service worker: shows push alerts and opens the dashboard when one is tapped.
// No offline caching on purpose: a stale home dashboard is worse than an error.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(self.clients.claim()));

self.addEventListener('push', e => {
  let msg = {};
  try { msg = e.data ? e.data.json() : {}; } catch { msg = { title: 'hometron', body: e.data && e.data.text() }; }
  e.waitUntil(self.registration.showNotification(msg.title || 'hometron', {
    body: msg.body || '',
    tag: msg.tag,
    renotify: !!msg.tag,
    timestamp: msg.ts || Date.now(),
    icon: '/icon-192.png',
    badge: '/icon-192.png',
    data: { url: msg.url || '/' },
  }));
});

self.addEventListener('notificationclick', e => {
  e.notification.close();
  const url = new URL(e.notification.data?.url || '/', self.location.origin).href;
  e.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const open = wins.find(w => w.url.startsWith(self.location.origin));
    if (open) { await open.focus(); return open.navigate(url); }
    return self.clients.openWindow(url);
  })());
});
