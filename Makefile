# hometron runs on an always-on Mac with no Go toolchain, so we cross-build here and ship the binary.
#
# Machine-specific settings live in local.mk (git-ignored). Copy local.mk.example and fill it in:
#   HOST   = you@your-server          ssh target
#   LISTEN = 100.x.y.z:8080,127.0.0.1:8080   addresses to bind: the Tailscale IP, plus loopback
#                                             for `tailscale serve` (HTTPS) to proxy to
-include local.mk
LABEL := com.swantron.hometron
comma := ,
HEALTH = $(firstword $(subst $(comma), ,$(LISTEN)))

.PHONY: build test deploy logs restart status check-host

build:
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/hometron .

test:
	go vet ./... && go test ./...

check-host:
	@test -n "$(HOST)" -a -n "$(LISTEN)" || { echo "set HOST and LISTEN in local.mk (see local.mk.example)"; exit 1; }

# Copies the binary + launchd agent and (re)starts it as the logged-in user (the server auto-logs-in).
# The plist template's __HOME__ and __LISTEN__ are filled in on the server.
deploy: check-host test build
	ssh $(HOST) 'mkdir -p ~/bin ~/Library/LaunchAgents ~/nest/logs'
	scp -q bin/hometron $(HOST):bin/hometron.new
	ssh $(HOST) 'sed -e "s#__HOME__#$$HOME#g" -e "s#__LISTEN__#$(LISTEN)#g" > ~/Library/LaunchAgents/$(LABEL).plist' < deploy/$(LABEL).plist
	ssh $(HOST) 'mv ~/bin/hometron.new ~/bin/hometron && \
	  launchctl bootout gui/`id -u`/$(LABEL) 2>/dev/null; \
	  for i in 1 2 3 4 5 6 7 8 9 10; do launchctl print gui/`id -u`/$(LABEL) >/dev/null 2>&1 || break; sleep 1; done; \
	  for i in 1 2 3 4 5; do launchctl bootstrap gui/`id -u` ~/Library/LaunchAgents/$(LABEL).plist 2>/dev/null && break; sleep 2; done; \
	  launchctl print gui/`id -u`/$(LABEL) >/dev/null 2>&1 || { echo "launchd would not load $(LABEL)"; exit 1; }; \
	  for i in 1 2 3 4 5 6 7 8 9 10; do sleep 2; curl -fsS http://$(HEALTH)/healthz 2>/dev/null && exit 0; done; \
	  echo "not healthy after 20s; check make logs"; exit 1'

restart: check-host
	ssh $(HOST) 'launchctl kickstart -k gui/`id -u`/$(LABEL)'

status: check-host
	ssh $(HOST) 'launchctl print gui/`id -u`/$(LABEL) | grep -E "state|pid|last exit"'

logs: check-host
	ssh $(HOST) 'tail -n 50 ~/nest/logs/hometron.log'
