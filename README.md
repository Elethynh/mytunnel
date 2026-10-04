# mytunnel

TLDR: You know ngrok? This is your basic ngrok - free (except for a domain)

![Starting a public HTTPS tunnel with mytunnel](docs/assets/demo.gif)

Expose local HTTP services on subdomains of your own domain with a small Go CLI and [Cloudflare Tunnel](https://developers.cloudflare.com/tunnel/).

```sh
mytunnel 5173 --subdomain local-app
# https://local-app.example.com → 127.0.0.1:5173
```

One computer runs one shared `cloudflared` process and a local router. Each startup command adds one or more routes, keeps them alive, and removes its routes when you press Ctrl+C. Start another command or use a project file to serve more ports. A single wildcard DNS record covers every route, so starting a service requires no DNS changes or Cloudflare API calls.

Without a configured domain, the same commands work locally at `http://local-app.localhost:43187`.

## Install

### Download a binary

Download an archive and `checksums.txt` from the [latest release](https://github.com/Elethynh/mytunnel/releases/latest). The binary needs no Go runtime. Public tunnels also require `cloudflared`.

| System | Archive suffix |
| --- | --- |
| Linux, x86-64 | `linux_amd64.tar.gz` |
| Linux, ARM64 | `linux_arm64.tar.gz` |
| macOS, Intel | `darwin_amd64.tar.gz` |
| macOS, Apple Silicon | `darwin_arm64.tar.gz` |

Check the archive's SHA-256 against `checksums.txt` using `sha256sum` on Linux or `shasum -a 256` on macOS. For example, install the Linux x86-64 release with:

```sh
tar -xzf mytunnel_0.2.0_linux_amd64.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 0755 mytunnel "$HOME/.local/bin/mytunnel"
```

Make sure `$HOME/.local/bin` is in your `PATH`. In zsh, run `rehash` if the new command is not found.

Each archive also contains `LICENSE`, `README.md`, and `THIRD_PARTY_NOTICES.md`.

### Install with Go

Go 1.22 or newer is required:

```sh
go install github.com/Elethynh/mytunnel@latest
```

The command is installed in `$(go env GOPATH)/bin`, unless you set `GOBIN`. Add that directory to your `PATH`.

### Build from source

```sh
git clone https://github.com/Elethynh/mytunnel.git
cd mytunnel
go build -o mytunnel .
```

## Try it before buying a domain

Run your HTTP application, then start a route:

```sh
mytunnel 5173 --subdomain local-app
# http://local-app.localhost:43187 → 127.0.0.1:5173
```

In another terminal:

```sh
mytunnel 8080 --subdomain api
# http://api.localhost:43187 → 127.0.0.1:8080
```

Omit `--subdomain` to generate a random name. `mytunnel status` lists active routes, and `mytunnel --version` prints the installed version.

Each command owns its route until Ctrl+C or a disconnected control session. Stopping one route leaves the others running. The shared daemon exits after the final route has been inactive for five seconds.

If your system does not resolve `*.localhost`, send the hostname explicitly:

```sh
curl -H 'Host: local-app.localhost' http://127.0.0.1:43187/
```

Local mode is accessible only on your computer.

## Start a project

Add a `.mytunnel.json` file to a project when you want to start several routes together:

```json
{
  "version": 1,
  "routes": [
    { "port": 5173, "subdomain": "local-app" },
    { "port": 8080, "subdomain": "api" }
  ]
}
```

Start every route declared in the file:

```sh
mytunnel up
```

`mytunnel up` reads `.mytunnel.json` from the current directory. It does not search parent directories. Select an exact alternate path with `--config`:

```sh
mytunnel up --config ./config/dev.mytunnel.json
```

The version 1 schema accepts only `version` and `routes` at the top level, and only an explicit `port` and `subdomain` for every route. Unknown fields are rejected. The file does not contain credentials, domain setup, shell commands, or sharing preferences.

The complete file is validated before any route starts. If registration later fails, mytunnel reports the failed route and releases only the routes acquired by that `up` command. Routes owned by other commands remain active.

## Share URLs and inspect requests

The startup flags work with both the port-first command and `up`. They are optional and can be combined:

```sh
mytunnel 5173 --subdomain local-app --open --copy --qr --logs
mytunnel up --config ./config/dev.mytunnel.json --open --copy --qr --logs
```

- `--open` opens each local URL immediately. In public mode it opens a route only after public confirmation succeeds. A timed-out public route is not opened automatically.
- `--copy` sends all registered URLs to the clipboard once, as one newline-separated payload in project-file order.
- `--qr` prints a labelled terminal QR for each public URL. The QR is generated inside mytunnel, so no QR runtime program is required. In local mode, mytunnel keeps printing the ordinary URL and explains that a `localhost` URL cannot be reached from a phone.
- `--logs` streams completed requests for only the routes owned by that command. Each line contains the route label, method, path, status, and duration, for example `[local-app] GET /products 200 12ms`.

On macOS, `--open` uses `open` and `--copy` uses `pbcopy`. On Linux, opening uses `xdg-open`; copying uses `wl-copy` in a Wayland session or `xclip` in an X11 session. These helpers are optional. A missing helper, QR error, or failed sharing action produces a warning and leaves the routes active.

Clipboard copying happens after registration, before public confirmation, and preserves the declared route order. Public QR codes are also printed at registration. Sharing flags never change the project file.

Request logs omit query strings, headers, request and response bodies, credentials, and readiness probes. Metadata is escaped for safe terminal output, and request events are never written to `daemon.log`. A streaming request appears after its stream ends, when its final status and duration are known. Logging uses bounded queues so a slow terminal cannot delay HTTP or WebSocket traffic; mytunnel warns when events are dropped.

## Set up public HTTPS

This requires a domain, a Cloudflare account, and `cloudflared` on the computer serving your applications. The domain can be registered with Cloudflare or another registrar.

1. Add the domain to Cloudflare. If another registrar manages it, change its nameservers to those assigned by Cloudflare. Wait for the zone to become active and its Universal SSL certificate to be issued.
2. [Install cloudflared](https://developers.cloudflare.com/tunnel/downloads/), then create a locally managed tunnel:

   ```sh
   cloudflared tunnel login
   cloudflared tunnel create mytunnel
   ```

   Note the tunnel UUID and the path to its generated `<UUID>.json` credentials file.
3. Stop any active `mytunnel` commands, then configure the domain and tunnel:

   ```sh
   mytunnel configure \
     --domain example.com \
     --tunnel 6ff42ae2-765d-4adf-8112-31c55c1551ef \
     --credentials /absolute/path/to/6ff42ae2-765d-4adf-8112-31c55c1551ef.json
   ```

4. In Cloudflare DNS, create one **proxied CNAME** record: name `*`, target `<UUID>.cfargotunnel.com`. A specific DNS record takes precedence over the wildcard; remove a conflicting record if you want that hostname to reach the tunnel.
5. Start a route:

   ```sh
   mytunnel 5173 --subdomain local-app
   # https://local-app.example.com → 127.0.0.1:5173
   ```

The local route is registered and printed immediately. `cloudflared` may need a moment to connect to Cloudflare. For every public route, mytunnel concurrently retries a route-specific HTTPS readiness URL for up to 30 seconds. Confirmation requires valid TLS, the exact requested hostname, and the proof created for that route invocation. It does not call your application or certify application health. A timeout prints an actionable warning and keeps the route active.

Public HTTPS is available while your computer and CLI sessions are running. Once the tunnel stops, its DNS record remains.

The router supports HTTP and WebSocket connections, and routes use one subdomain label such as `local-app.example.com`. These public services have no additional authentication. Some development servers check the HTTP Host header; allow your chosen public hostname explicitly in the application's server settings.

## How it works

```mermaid
flowchart LR
    browser["Browser: local-app.example.com"] --> edge["Cloudflare HTTPS"]
    edge --> tunnel["cloudflared"]
    tunnel --> router["Local router"]
    router --> app["127.0.0.1:5173"]
```

The first CLI session starts the shared daemon and, in public mode, `cloudflared`. A private Unix socket registers routes and tracks their owning sessions. Requests are forwarded to the selected local port according to their hostname.

Settings and daemon logs live in `~/.config/mytunnel/`, or `$XDG_CONFIG_HOME/mytunnel/`. The settings file and control socket have mode `0600`, and their directory has mode `0700`. Tunnel configuration is generated from the settings when the daemon starts. `MYTUNNEL_HOME` overrides the settings directory for development and isolated tests.

The CLI and daemon negotiate feature support. If a newer CLI reports that the running daemon is from an older version, stop the active mytunnel commands you own and retry the new command. mytunnel does not forcibly kill the old daemon or unrelated sessions.

## Development and releases

```sh
go test -race ./...
go vet ./...
go build .
```

Tests cover first-time settings creation, strict project files and rollback, concurrent route ownership, public readiness, URL sharing, bounded request logs, HTTP streaming, and WebSocket upgrades. GitHub Actions runs the race-enabled suite on Linux and macOS with current Go, plus a separate test and build with Go 1.22 and `GOTOOLCHAIN=local`. A real public tunnel needs a configured Cloudflare account and domain to test end to end.

To build the four release archives and their SHA-256 checksums:

```sh
./scripts/package-release.sh v0.2.0
# dist/v0.2.0/
```

Pushing a version tag such as `v0.2.0` runs the release workflow, repeats the checks, and publishes the archives to GitHub Releases.

## License

[MIT](LICENSE).

Cloudflare documentation: [locally managed tunnels](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/), [ingress rules](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/configuration-file/), [wildcard DNS](https://developers.cloudflare.com/dns/manage-dns-records/reference/wildcard-dns-records/), and [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/).
