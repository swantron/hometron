# hometron

One dashboard for Nest cameras, the doorbell, the thermostat and the garage door. It talks straight to Google's
[Smart Device Management API](https://developers.google.com/nest/device-access), so nobody in the
house has to open the Google Home app or the Nest app.

- **Cameras / doorbell:** live view (WebRTC, straight from Google to the browser), plus a snapshot or
  clip for ring / person events on cameras that support it (see Gotchas).
- **Thermostat:** current temperature and humidity, heat/cool setpoints, mode, eco.
- **Garage door (optional):** open/closed state and a two-tap open/close, via a Tailwind iQ3's
  local API. No cloud involved.
- **Activity feed:** events stream in live (Server-Sent Events). Repeated motion folds into one row,
  and the default "Important" view shows only rings, people, clips and garage moves. Media is kept
  for 14 days.
- **Alerts:** doorbell and person push notifications on phones and laptops (the 🔔 in the header).
  Plain Web Push: hometron signs and encrypts each message itself and hands it to the browser
  vendor's push service, so there's no notification service or SDK in between.
- **Stills:** a frame grabbed from the live stream whenever someone watches a camera (or taps 📷),
  since Google won't give event snapshots for WebRTC-only cameras.
- **Installable:** add it to the home screen and it opens full-screen like an app.
- **Network (`#network`):** every device answering on the server's LAN, with its maker (from the MAC),
  a flag for anything that speaks the Tailwind API, and other routers that may be hiding devices
  behind them. It rescans every 10 minutes and remembers what dropped off. The maker table comes from
  Wireshark's `manuf` file, downloaded at most once a month.

A single Go binary, standard library only. It runs on an always-on Mac and is reachable over Tailscale.

```
browser ──HTTP──▶ hometron (Mac, Tailscale IP) ──▶ SDM API ──▶ Nest devices
   ▲                    │  ▲
   └── WebRTC media ────┼──┘ (video goes Google → browser directly)
                        ├── pulls events from Pub/Sub ◀── Device Access publishes
                        └── polls Tailwind on the LAN (garage)
```

## Setup (one time)

You need a consumer Google account that owns the Nest devices. Workspace accounts can't use Device Access.

1. **Device Access** ($5 one-time): register at <https://console.nest.google.com/device-access> and
   create a project.
2. **Google Cloud project**: enable the *Smart Device Management API* and the *Cloud Pub/Sub API*.
   Billing is not required.
3. **OAuth**: set the consent screen to External and add a homepage and privacy URL so it can be published
   to production. Testing mode kills refresh tokens after 7 days. Create a *Web application* client with
   the redirect `http://localhost:8765`, then put its client ID into the Device Access project.
4. **Authorize** with scopes `sdm.service` and `pubsub`:
   `https://nestservices.google.com/partnerconnections/<SDM_PROJECT>/auth?redirect_uri=http://localhost:8765&access_type=offline&prompt=consent&client_id=<CLIENT_ID>&response_type=code&scope=https://www.googleapis.com/auth/sdm.service%20https://www.googleapis.com/auth/pubsub`
   Exchange the code for a refresh token (a tiny localhost listener means the code never has to be copied
   by hand), and store it in `~/nest/token.json`.
5. **State dir** `~/nest/` (chmod 700) on the server:
   - `credentials.json`: `client_id`, `client_secret`, `gcp_project_id`, `sdm_project_id`
   - `token.json`: `refresh_token`
6. **First start** creates the Pub/Sub topic `home-nest-events`, grants
   `sdm-publisher@googlegroups.com` publish rights on it, and creates the pull subscription `hometron`.
   Then set the Device Access project's Pub/Sub topic to `projects/<GCP_PROJECT>/topics/home-nest-events`.
7. **Garage (optional):** get the *Local Control Key* from <https://web.gotailwind.com> and create
   `~/nest/tailwind.json`:
   ```json
   {"host": "192.168.1.50", "token": "123456", "names": {"door1": "Garage"}}
   ```
   The server has to be on the same network as the Tailwind. Without this file the garage card simply
   doesn't appear.

## HTTPS (needed for alerts and installing it as an app)

Browsers only allow push and home-screen install on https. Tailscale provides the certificate:

1. In the Tailscale admin console, enable **MagicDNS** and **HTTPS Certificates** (DNS page). Note
   that issued certificates, and so the machine's tailnet name, appear in public Certificate
   Transparency logs.
2. On the server: `tailscale serve --bg http://127.0.0.1:8080` (hometron listens on loopback too; see
   `LISTEN` in `local.mk.example`).
3. Open `https://<machine>.<tailnet>.ts.net`, tap 🔔, and allow notifications. On iPhone, add it to
   the Home Screen first; iOS only allows push for installed web apps.

## Build and deploy

Copy `local.mk.example` to `local.mk` (git-ignored) and set `HOST` (ssh target) and `LISTEN`
(the server's Tailscale IP and port).

```sh
make test      # vet + unit tests
make deploy    # cross-build darwin/arm64, scp to the server, (re)load the launchd agent, health check
make logs      # tail the server log
make status    # launchd state
```

The agent (`deploy/com.swantron.hometron.plist`) runs as the logged-in user. It restarts on crash and
keeps retrying at boot until the Tailscale address exists.

## Security notes

- It binds only to the Tailscale IP. There is no auth layer, so the tailnet *is* the access control.
- Commands from the browser are allow-listed (thermostat setpoints/mode/eco, live-stream start/extend/stop,
  garage open/close),
  and cross-origin POSTs are refused so other websites can't drive the thermostat through your browser.
- Secrets stay in `~/nest` on the server and never in this repo (see `.gitignore`).

## Adding another kind of device

Everything the UI shows comes from a `Provider` (`provider.go`):

```go
type Provider interface {
	Devices(ctx context.Context) ([]deviceView, error)
	Owns(ctx context.Context, id string) bool
	Command(ctx context.Context, id, command string, params map[string]any) (json.RawMessage, error)
}
```

`nest` wraps the SDM API; `Tailwind` (`tailwind.go`) is a self-contained example of a second one:
a config file, a poll loop that posts state changes to the feed with `events.Post`, and an allow-list
of commands. To add something new, write a provider, register it in `main`, give its devices their
own `kind`, and add a card for that kind in `web/index.html`.

## Gotchas found along the way

- Every camera here, even an older wired one, offers **WebRTC only**, never RTSP. The browser has to
  send an offer with audio, then video (both receive-only), then a data channel, in that order. Streams
  die after 5 minutes unless extended.
- **No snapshots for WebRTC cameras.** `GenerateImage` only works on RTSP models; everything else gets a
  400. Only cameras with `CameraClipPreview` (the doorbell here) send a short clip with each event. The
  rest get a feed row and a tap-to-go-live tile.
- Where snapshots do work, they're only fetchable for about **30 seconds** after the event, so they're
  downloaded the moment the Pub/Sub message arrives.
- New Device Access projects make you bring your **own** Pub/Sub topic. Google doesn't create one any more.
