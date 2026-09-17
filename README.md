# Knocknoc middleware for Traefik

A Traefik middleware that lets a request through only if the caller's IP is on
the allowlist published by a [Knocknoc](https://knocknoc.io) server.

Knocknoc grants network access to authenticated users just-in-time: when a user
authenticates, their current source IP joins an allowlist, and it leaves again
when their session ends. This plugin applies that allowlist at the Traefik
layer, so an HTTP service can sit behind Knocknoc without the service itself
knowing anything about Knocknoc, and without a separate agent on the host.

The plugin polls the allowlist in the background and evaluates it per request.
It **fails closed**: if the list has never been fetched successfully, or is
empty, every request is rejected.

## Installation

Add the plugin to Traefik's **static** configuration:

```yaml
experimental:
  plugins:
    knocknoc:
      moduleName: github.com/knocknoc-io/traefik-plugin-knocknoc
      version: v0.1.0
```

<details>
<summary>TOML</summary>

```toml
[experimental.plugins.knocknoc]
  moduleName = "github.com/knocknoc-io/traefik-plugin-knocknoc"
  version = "v0.1.0"
```

</details>

<details>
<summary>CLI</summary>

```
--experimental.plugins.knocknoc.modulename=github.com/knocknoc-io/traefik-plugin-knocknoc
--experimental.plugins.knocknoc.version=v0.1.0
```

</details>

## Configuration

Then declare a middleware in the **dynamic** configuration and attach it to a
router:

```yaml
http:
  routers:
    my-app:
      rule: Host(`app.example.com`)
      service: my-app
      middlewares:
        - knocknoc

  middlewares:
    knocknoc:
      plugin:
        knocknoc:
          sourceURL: https://knocknoc.example.com/api/allowlist
          username: traefik
          secret: "{{ env `KNOCKNOC_SECRET` }}"
          pollInterval: 5s
```

<details>
<summary>Docker labels</summary>

```yaml
labels:
  - "traefik.http.routers.my-app.middlewares=knocknoc"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.sourceURL=https://knocknoc.example.com/api/allowlist"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.username=traefik"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.secret=your-secret"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.pollInterval=5s"
```

</details>

<details>
<summary>Kubernetes CRD</summary>

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: knocknoc
spec:
  plugin:
    knocknoc:
      sourceURL: https://knocknoc.example.com/api/allowlist
      username: traefik
      secret: your-secret
      pollInterval: 5s
```

</details>

### Options

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `sourceURL` | string | — | **Required.** URL of the Knocknoc allowlist endpoint. Credentials must not be embedded in the URL's userinfo; use `username` and `secret`. |
| `username` | string | `""` | HTTP basic auth username. When empty, no `Authorization` header is sent. |
| `secret` | string | `""` | HTTP basic auth password. Setting it without a `username` is a configuration error. |
| `pollInterval` | duration | `2s` | How long a fetched allowlist is reused before the next fetch. Minimum `1s`. |
| `requestTimeout` | duration | `5s` | Timeout for a single fetch of the allowlist. |
| `rejectStatusCode` | int | `403` | Status returned to a caller that is not on the allowlist. Must be 4xx or 5xx. |
| `rejectBody` | string | `""` | Response body served to a rejected caller. When empty, a plain `Forbidden` is returned. |
| `rejectContentType` | string | `text/html; charset=utf-8` | Content type for `rejectBody`. Setting it without a `rejectBody` is a configuration error. |
| `insecureSkipVerify` | bool | `false` | Skip TLS verification when fetching the allowlist. For testing against a self-signed Knocknoc server only. |
| `ipStrategy.depth` | int | `0` | Which `X-Forwarded-For` entry identifies the caller. See below. |
| `ipStrategy.excludedIPs` | []string | `[]` | IPs or CIDR blocks removed from `X-Forwarded-For` before `depth` is applied. |

Durations use Go syntax: `500ms`, `5s`, `2m`.

### Choosing the caller's IP

By default (`ipStrategy` absent, or `depth: 0`) the plugin uses the remote
address of the TCP connection and ignores `X-Forwarded-For` entirely. That is
the correct and safest setting when clients connect to Traefik directly, since
`X-Forwarded-For` is caller-controlled and trivially spoofed.

When Traefik sits behind a load balancer or CDN, set `depth` to select an entry
from `X-Forwarded-For`, counting from the right. This uses the **same
convention as Traefik's own `ipStrategy`**, so a `depth` that works for
`IPAllowList` works here too.

Given `X-Forwarded-For: 203.0.113.7, 198.51.100.4, 10.0.0.5`:

| Setting | Selected IP |
| --- | --- |
| `depth: 0` (default) | the TCP remote address |
| `depth: 1` | `10.0.0.5` |
| `depth: 2` | `198.51.100.4` |
| `depth: 3` | `203.0.113.7` |

`excludedIPs` drops matching entries before `depth` is counted, which keeps
`depth` stable when a variable number of trusted proxies append themselves:

```yaml
ipStrategy:
  depth: 1
  excludedIPs:
    - 10.0.0.0/8
    - 172.16.0.0/12
```

Entries are matched by address, not by text, so `2001:DB8::A`,
`2001:db8:0:0:0:0:0:a`, and `2001:db8::/64` all match the same address.

If `depth` exceeds the number of remaining entries, the caller's IP cannot be
determined and the request is rejected.

> **Only set `depth` if every hop in front of Traefik is trusted to rewrite
> `X-Forwarded-For`.** A caller who can inject their own header can otherwise
> present any IP they like, including one on the allowlist.

### Customising the rejection response

By default a rejected caller gets a plain-text `Forbidden`. Set `rejectBody` to
serve your own page instead:

```yaml
http:
  middlewares:
    knocknoc:
      plugin:
        knocknoc:
          sourceURL: https://knocknoc.example.com/api/allowlist
          rejectBody: |
            <!doctype html>
            <html>
              <head><title>Access denied</title></head>
              <body>
                <h1>Access denied</h1>
                <p>Authenticate with Knocknoc to reach this service.</p>
              </body>
            </html>
```

The body is served verbatim with `Content-Type: text/html; charset=utf-8` and
`X-Content-Type-Options: nosniff`. It is static: the plugin does no templating,
so nothing about the request or the caller can leak into it.

For an API rather than a browser, set `rejectContentType` to match:

```yaml
rejectBody: '{"error":"not authorized"}'
rejectContentType: application/json
```

`rejectBody` is also what a caller sees on the fail-closed path, when the
allowlist has never loaded. If you want that case to be distinguishable from an
ordinary rejection, pair it with a `rejectStatusCode` your monitoring can alert
on.

## Allowlist format

The plugin expects the endpoint to return `200` with one entry per line. Each
entry is a bare IP address or a CIDR block, IPv4 or IPv6. Blank lines and lines
starting with `#` are ignored, as are IPv6 zone suffixes and surrounding
brackets. Unparseable lines are skipped with a log line rather than failing the
whole refresh.

```
# Knocknoc session allowlist
203.0.113.7
198.51.100.0/24
2001:db8::1
2001:db8:1234::/48
```

## Behaviour

**Refresh.** The allowlist is fetched once when the middleware is created, then
refreshed lazily: the first request arriving after `pollInterval` has elapsed
triggers a refresh, and concurrent requests wait for that single fetch rather
than each starting their own. There is no background goroutine, so an idle
router performs no polling.

**A failed refresh keeps the previous allowlist.** A Knocknoc server that is
briefly unreachable will not lock out users who were already allowed; the list
simply goes stale until the next successful fetch. The failure is logged.

**Startup with no allowlist rejects everything.** If the very first fetch fails,
there is no previous list to fall back on and every request is rejected until a
fetch succeeds.

**Staleness is bounded by `pollInterval`.** A user whose Knocknoc session has
just ended may still be admitted for up to `pollInterval`. Lower it to tighten
that window, at the cost of more requests to the Knocknoc server.

**Secrets are kept out of errors.** The configured `secret` is never included in
error messages or logs, and URL parse errors are stripped of the URL they came
from.

## Development

```sh
make          # lint, test, and run the tests under the Yaegi interpreter
make test     # go test
make lint     # golangci-lint
make yaegi_test
```

Traefik does not compile plugins; it interprets them with
[Yaegi](https://github.com/traefik/yaegi). Code that compiles can still fail to
interpret, so `make yaegi_test` is the check that matters before tagging a
release.

To try the plugin against a local Traefik without publishing it, use a local
plugin mount:

```yaml
experimental:
  localPlugins:
    knocknoc:
      moduleName: github.com/knocknoc-io/traefik-plugin-knocknoc
```

with this repository checked out at
`./plugins-local/src/github.com/knocknoc-io/traefik-plugin-knocknoc` relative to
Traefik's working directory.

## License

[MIT](LICENSE)
