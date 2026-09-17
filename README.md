# Knocknoc middleware for Traefik

This is a Traefik middleware that lets a request through only if the caller's IP is on
the allowlist (EDL) published by [Knocknoc](https://knocknoc.io).

Knocknoc grants network access just-in-time: when a user authenticates, their
current source IP is stored in an allowlist until their session ends, or their access is revoked.
Knocknoc exposes that allowlist as an [EDL (External Dynamic List)](https://docs.knocknoc.io/books/admin-guide/page/allowlist-edls).
This plugin polls that EDL and only allows http requests through Traefik from Knocknoc authenticated users.

## How it works

**Poll the Knocknoc server directly.** The plugin fetches the EDL straight from your Knocknoc server, or from a Knocknoc Agent caching the EDL.

![Traefik polling the Knocknoc server directly](https://raw.githubusercontent.com/knocknoc-io/traefik-plugin-knocknoc/main/.assets/approach-direct.png)

**Poll a local Knocknoc agent.** An agent running alongside Traefik in-cluster caches the allowlist, and receives updates from the server.
The plugin polls the agent instead, which removes the network delay associated with a round trip to the Knocknoc Server.

![Traefik polling a local Knocknoc agent](https://raw.githubusercontent.com/knocknoc-io/traefik-plugin-knocknoc/main/.assets/approach-agent.png)

Either way the plugin configuration is the same.
Interested in the agent-based setup? [Get in touch](https://knocknoc.io/demo).

## Installation

Add the plugin to Traefik's **static** configuration:

```yaml
experimental:
  plugins:
    knocknoc:
      moduleName: github.com/knocknoc-io/traefik-plugin-knocknoc
      version: v0.1.0
```

## Usage

Declare a middleware in the **dynamic** configuration and attach it to a router.

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
          sourceURL: https://knocknoc.example.com/edl/<your-edl-uuid>
          username: traefik
          secret: "{{ env `KNOCKNOC_SECRET` }}"
          pollInterval: 1s
```

<details>
<summary>Docker labels</summary>

```yaml
labels:
  - "traefik.http.routers.my-app.middlewares=knocknoc"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.sourceURL=https://knocknoc.example.com/edl/<your-edl-uuid>"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.username=traefik"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.secret=your-secret"
  - "traefik.http.middlewares.knocknoc.plugin.knocknoc.pollInterval=1s"
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
      sourceURL: https://knocknoc.example.com/edl/<your-edl-uuid>
      username: traefik
      secret: your-secret
      pollInterval: 1s
```

</details>

## Configuration

`sourceURL` is the only required option.

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `sourceURL` | string | — | **Required.** URL of the Knocknoc EDL. Must not embed credentials in its userinfo — use `username` and `secret`. |
| `username` | string | `""` | HTTP basic auth username. When empty, no `Authorization` header is sent. |
| `secret` | string | `""` | HTTP basic auth password. Setting it without a `username` is a configuration error. |
| `pollInterval` | duration | `2s` | How long a fetched allowlist is reused before the next fetch. Minimum `1s`. |
| `requestTimeout` | duration | `5s` | Timeout for a single fetch. `0` disables the timeout entirely. |
| `rejectStatusCode` | int | `403` | Status returned to a caller that is not on the allowlist. Must be 4xx or 5xx. |
| `rejectBody` | string | `""` | Response body served to a rejected caller. When empty, a plain `Forbidden` is returned. |
| `rejectContentType` | string | `text/html; charset=utf-8` | Content type for `rejectBody`. Setting it without a `rejectBody` is a configuration error. |
| `insecureSkipVerify` | bool | `false` | Skip TLS verification when fetching the EDL. For testing against a self-signed server only. |
| `ipStrategy.depth` | int | `0` | Which `X-Forwarded-For` entry identifies the caller. `0` uses the TCP remote address. |
| `ipStrategy.excludedIPs` | []string | `[]` | IPs or CIDR blocks removed from `X-Forwarded-For` before `depth` is applied. |

Durations use Go syntax (`500ms`, `1s`, `2m`). Invalid values are rejected on startup.

### Identifying the caller

By default the plugin uses the TCP remote address and ignores
`X-Forwarded-For`. If Traefik is behind a load balancer or CDN, set `depth` to pick an entry
from that header, counting from the right.

Given `X-Forwarded-For: 203.0.113.7, 198.51.100.4, 10.0.0.5`:

| Setting | Selected IP |
| --- | --- |
| `depth: 0` (default) | the TCP remote address |
| `depth: 1` | `10.0.0.5` |
| `depth: 2` | `198.51.100.4` |
| `depth: 3` | `203.0.113.7` |

`excludedIPs` drops matching entries before `depth` is counted, keeping `depth`
consistent when a variable number of trusted proxies append themselves.

> Only set `depth` if every hop in front of Traefik is trusted to rewrite
> `X-Forwarded-For`. Otherwise a caller can present any IP they like.

### Custom rejection page

```yaml
rejectBody: |
  <!doctype html>
  <html><body><h1>Access denied</h1></body></html>
```

Served verbatim with `X-Content-Type-Options: nosniff`. There is no templating.
For an API, set `rejectContentType: application/json`.

## Behaviour

- **A failed refresh keeps the previous allowlist**, so a brief Knocknoc outage
  does not lock out users who were already allowed.
- **Staleness is bounded by `pollInterval`**. A user whose session just ended
  may still be admitted for up to that long.
- **Refreshes are lazy and shared**. There will be one fetch per `pollInterval`, only when
  traffic arrives.

## Development

```sh
make    # lint, test, and run the tests under the Yaegi interpreter
```

## License

[MIT](LICENSE)
