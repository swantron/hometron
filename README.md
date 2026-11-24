# hometron

One dashboard for the house: Nest cameras, the doorbell, the thermostat, the garage door, laundry, weather, the family calendar, and printer and internet status. It talks straight to Google's
[Smart Device Management API](https://developers.google.com/nest/device-access), so nobody in the
house has to open the Google Home app or the Nest app.

- **Cameras / doorbell:** live view (WebRTC, straight from Google to the browser), plus a snapshot or
  clip for ring / person events on cameras that support it (see Gotchas).
- **Thermostat:** current temperature and humidity, heat/cool setpoints, mode, eco, plus a 24-hour
  chart (inside, setpoint, outside, with heating/cooling shaded) and how long the heat ran today and
  yesterday. Recorded from the thermostat updates Google already sends.
- **Doorbell chime (optional):** rings Google/Nest speakers when the doorbell is pressed, over the local
  Cast protocol, with an optional spoken line made by macOS's `say`. No Assistant or cloud involved.
- **Laundry (optional):** LG washer/dryer state and time left, and a "done" alert, via LG's official
  ThinQ Connect API (cloud; the machines have no local API).
- **Weather (optional):** outside now and the next few periods from the National Weather Service.
- **Calendar and chores (optional):** this week from Google Calendar's secret iCal address, plus trash and
  recycling days with a reminder the night before.
- **Printer:** ink levels and state over IPP (found automatically).
- **Network health:** every minute, pings each step out of the house (Wi-Fi → the router it plugs into →
  the ISP's first hop → the internet) plus a DNS lookup, so a problem is pinned to one of them. A 24-hour
  response-time/packet-loss chart, 7/30-day uptime with an outage log, Google/Nest Wifi units' uptime and
  firmware (from their unauthenticated local status page, with a feed entry when one restarts), always-on
  devices dropping off or new devices joining, Tailscale devices with key-expiry warnings, and the
  server's own Wi-Fi link. Speed tests run in the browser of whoever taps the button (against Cloudflare's
  public endpoints), since a server on Wi-Fi can't measure the internet itself; results are kept.
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

### Optional extras

Each is a file in `~/nest/` (chmod 600). Leave it out and that feature stays off. Restart hometron
(`make restart`) after adding one.

- **`cast.json`: doorbell chime on speakers.** Names as shown in the Google Home app:
  ```json
  {"speakers": ["Kitchen speaker"], "say": "Someone is at the front door", "volume": 0.6}
  ```
  `say` and `volume` are optional; the speaker's own volume is restored afterwards. The speakers fetch the
  sound from the server's LAN address on port 8081 (only the sound is served there). Casting stops
  whatever the speaker was playing. `GET /api/chime/speakers` lists what was found.
- **`lg.json`: LG washer and dryer.** Create a personal access token at
  <https://connect-pat.lgthinq.com> (sign in with the LG ThinQ account the machines are in):
  ```json
  {"token": "thinqpat_…", "country": "US"}
  ```
- **`weather.json`: outside weather** (US only; two decimals is plenty):
  ```json
  {"lat": 45.68, "lon": -111.04}
  ```
- **`calendar.json`: calendar and chores.** In Google Calendar → Settings → your calendar →
  *Secret address in iCal format* (anyone with that link can read the calendar, so keep it in this file
  only):
  ```json
  {
    "calendars": [{"name": "Family", "url": "https://calendar.google.com/calendar/ical/…/private-…/basic.ics"}],
    "chores": [
      {"name": "Trash", "weekday": "Tuesday"},
      {"name": "Recycling", "weekday": "Tuesday", "weeks": 2, "from": "2026-12-01"}
    ],
    "remind": "19:00"
  }
  ```
- **`smartthings.json`: Samsung fridge** (cloud; Samsung appliances have no local API). Door left open
  2+ minutes, fridge above 45°F or freezer above 15°F for half an hour, plus temperatures, water filter
  and ice maker on the dashboard. Either a personal access token from
  <https://account.smartthings.com/tokens> (scope: devices, read), which SmartThings expires after 24 hours,
  so only good for a test:
  ```json
  {"token": "..."}
  ```
  or, to keep it working, an OAuth app (created with the SmartThings CLI or Developer Workspace, with
  scopes `r:devices:*` and `r:locations:*`, and this server's `/smartthings/callback` as its redirect URI):
  ```json
  {"clientId": "...", "clientSecret": "...", "redirectUri": "https://<server>/smartthings/callback"}
  ```
  then tap **Connect** on the fridge card once. hometron refreshes the sign-in itself.

  **Without the paid plan** (Samsung's API plans start at $4.99/month as of late 2026; personal
  tokens are the free option and last 24 hours), add `"auto": true` next to the token. hometron then
  makes a fresh read-only token every ~20 hours the same way the tokens page does, in a headless Chrome
  profile (`~/nest/smartthings-browser`, needs Google Chrome on the server) that someone signed into
  once. It never stores the Samsung password. When that session expires, the fridge card shows
  **Sign in**, which opens Chrome on the server's screen for a Screen Sharing sign-in. Only tokens it
  created (named `hometron-auto-…`) are ever deleted.

  SmartThings' own sign-in lasts 2 hours. After that it quietly signs in again through Samsung's
  login, which idles out after about 30 minutes. So every 10 minutes hometron also runs the site's
  sign-in redirect to keep Samsung's side active. The log says whether that round trip is working
  (`Samsung sign-in alive` or `has lapsed`).
- **Camera pictures with no tab open.** If Google Chrome is installed on the server, hometron keeps
  the dashboard open in a headless Chrome (`~/nest/capture-browser`, loopback only, no sign-ins).
  It grabs a picture when a person shows up or the doorbell rings. On motion, it refreshes a camera's
  still if that still is over 10 minutes old. Otherwise it keeps every still under an hour old. Each
  grab is a short live stream. To turn it off, `touch ~/nest/capture-off` and restart.
- **`printer.json`:** only if the printer isn't found automatically: `{"host": "192.168.1.20"}`.
- **`fleet.json`: the Fleet page, which covers sites, domains and cloud inventory.**
  ```json
  {"gcpQuotaProject": "my-project",
   "domains": [{"name": "example.com", "expires": "2027-11-08", "registrar": "Squarespace"}],
   "checks": ["https://status.example.com/health"],
   "fixed": [{"name": "Claude Pro", "usd": 20}]}
  ```
  Every domain that has a website, plus each extra check, is fetched once a minute. Minutes when the
  home internet is down aren't counted. A site that fails 3 minutes in a row raises a `fleet` alert,
  and so does one coming back, a TLS certificate under 14 days, or a registration under 30 days.
  Registration dates come from the public registry (RDAP), and the config dates are the fallback.

  The inventory runs every 6 hours, read-only:
  - `digitalocean.json` `{"token": "..."}`: a read-only token. It lists apps, droplets, volumes,
    snapshots, databases and IPs, with month-to-date spend and invoices.
  - `fleet-token.json`: a Google sign-in with `cloud-platform.read-only`, made with the same OAuth
    client (`fleet-exchange.sh`). It lists every project, plus Cloud Run (with 30-day request
    counts), VMs (7-day CPU), disks, IPs, functions, image repos, Firebase sites and buckets.

  Each Google call is charged to the project it reads. The project list and billing lookups go to
  `gcpQuotaProject`, which needs the Cloud Resource Manager and Cloud Billing APIs turned on.
  Anything that appears or disappears between inventories posts an event. The **Worth a look** list
  flags empty projects, unattached disks and volumes, unused IPs, Cloud Run with no traffic, oversized
  App Platform plans, image repos over 1 GB, and extra e2-micro VMs beyond the free tier.

  **Google Workspace seats.** If the Fleet sign-in also has `admin.directory.user.readonly` and
  `admin.reports.usage.readonly`, Fleet lists each user with their last sign-in, the last activity
  per app (from Google's usage report, which trails by 2–4 days) and whether they have 2-step
  verification. It flags suspended accounts that are still billed, seats unused for 60 days, and
  accounts with no 2-step verification. Admins without it are marked "fix".

  Google has no billing API for Workspace bought directly, so set the price in `fleet.json`:
  `"workspace": {"plan": "Business Standard", "usdPerUser": 14}`. A Workspace domain that blocks
  unverified apps needs this OAuth client marked Trusted, under Admin console → Security →
  API controls → App access control.

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
