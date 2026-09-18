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
          username: apiuser
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

A rejected caller therefore gets exactly:

```http
HTTP/1.1 403 Forbidden
Content-Type: text/html; charset=utf-8
Content-Length: 4396
X-Content-Type-Options: nosniff
```

### Redirecting rejected users to Knocknoc

Because you can set the deny page HTML, you can unauthenticated users to your Knocknoc login
portal and have them returned to the protected URL once access is granted. This way an
unauthenticated user who hits the service gets bounced through sign-in instead of hitting a dead end. See
[Redirecting Users](https://docs.knocknoc.io/books/admin-guide/page/redirecting-users).

The example below renders a branded 403 which can redirect the user to your Knocknoc login page (uncomment the script section).

<details>
<summary>Branded 403 page with redirect to the Knocknoc login</summary>

```yaml
http:
  middlewares:
    knocknoc:
      plugin:
        knocknoc:
          sourceURL: https://knocknoc.example.com/edl/<your-edl-uuid>
          username: traefik
          secret: "{{ env `KNOCKNOC_SECRET` }}"
          pollInterval: 1s
          rejectBody: |
            <!DOCTYPE html>
            <html lang="en">
            <head>
                    <meta charset="UTF-8">
                    <meta name="viewport" content="width=device-width, initial-scale=1.0">
                    <title>Forbidden</title>
                    <link rel="preconnect" href="https://fonts.googleapis.com">
                    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
                    <link href="https://fonts.googleapis.com/css2?family=Raleway:ital,wght@0,100..900;1,100..900&display=swap" rel="stylesheet">
                    <style>
                            body {
                                    text-align: center;
                                    background-color: #171717;
                                    color: #ffffff;
                            }
                            .container {
                                    margin: 60px 0;
                                    padding: 60px;
                            }
                            h1 {
                                    font-family: "Raleway", sans-serif;
                                    font-optical-sizing: auto;
                                    font-size: 4em;
                                    font-weight: 300;
                                    font-style: normal;
                            }
                            span {
                                    color: #35b59b;
                            }
                    </style>
            </head>
            <body>
                    <div class="container">
                            <h1>Access Denied.</h1>
                            <h1>You must <span>Knoc</span> before you enter.</h1>
                    </div>

                    <svg id="Layer_1" data-name="Layer 1" xmlns="http://www.w3.org/2000/svg" width="300" height="300" viewBox="0 0 166.57 156.56">
                            <defs>
                                    <style>
                                            .cls-1 {
                                                    fill: none;
                                            }
                                            .cls-1, .cls-2 {
                                                    stroke-width: 0px;
                                            }
                                            .cls-2 {
                                                    fill: #35b59b;
                                            }
                                    </style>
                            </defs>
                            <g id="Layer_1-2" data-name="Layer 1">
                                    <path class="cls-2" d="M166.57,119.18c0,4-2.29,5.09-5,6.28l-103.5,31.1L0,126.1l86-17.7c.36-.16.73-.3,1.11-.41,3.26-.87,4.2-.65,8.58,1l42,17.66.14-117.91L90.75.81V.24l75.77-.24.05,119.18Z"/>
                                    <line class="cls-1" x1="55.63" y1="32.13" x2="41.78" y2="18.28"/>
                                    <rect class="cls-2" x="44.57" y="15.4" width="8.27" height="19.59" transform="translate(-3.55 41.82) rotate(-45)"/>
                                    <rect class="cls-2" x="29.03" y="46.02" width="18.27" height="8.27"/>
                                    <line class="cls-1" x1="54.19" y1="67.14" x2="40.34" y2="79.15"/>
                                    <rect class="cls-2" x="38.1" y="69.01" width="18.34" height="8.27" transform="translate(-36.37 48.85) rotate(-40.93)"/>
                            </g>
                    </svg>

                    <!-- If this Javascript is uncommented, it will redirect this 403 Forbidden page to the Knocknoc user portal login, which can then redirect back to the protected website once access is granted. -->
                    <!-- <script>
                            const baseUrl = '<<INSERT KNOCKNOC USER LOGIN PORTAL URL, eg: knocknoc.mycompany.com>>';
                            // Milliseconds to wait before redirecting. Set to 0 for an instant redirect.
                            const REDIRECT_DELAY_MS = 3000;

                            setTimeout(() => {
                                    const currentUrl = document.URL;
                                    if (!currentUrl || !baseUrl) return;

                                    try {
                                            // Add http:// or https:// if it doesn't already have a scheme
                                            let normalizedBase = baseUrl.trim();
                                            if (!/^https?:\/\//i.test(normalizedBase)) {
                                                    normalizedBase = 'https://' + normalizedBase;
                                            }

                                            const encodedRef = encodeURIComponent(currentUrl);
                                            const redirectUrl = normalizedBase + '?referrer=' + encodedRef;

                                            const url = new URL(redirectUrl);
                                            window.location.href = url.toString();
                                    } catch (err) {
                                            console.error('Invalid redirect URL:', err);
                                    }
                            }, REDIRECT_DELAY_MS);
                    </script> -->

            </body>
            </html>
```

</details>

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
