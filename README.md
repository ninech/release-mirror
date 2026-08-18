# release-mirror

An on-demand, pull-through mirror for release assets backed by S3-compatible storage.

## How it works

Requests are answered immediately with an HTTP `302` redirect to the asset's public S3/CDN URL. In the background, workers fetch the asset from upstream and store it in S3, using S3-based distributed locking to avoid duplicate downloads across replicas.

Object keys are prefixed with the upstream host, so the same owner and repository on two forges never collide.

## Usage

Prefix the upstream URL with the mirror hostname:

```bash
# Direct
curl -LO https://github.com/ninech/foo/releases/download/v1.0.0/foo_linux_amd64.tar.gz

# Via mirror
curl -LO https://mirror.example.com/github.com/ninech/foo/releases/download/v1.0.0/foo_linux_amd64.tar.gz
```

The same works for any other forge, whatever its URL layout:

```bash
curl -LO https://mirror.example.com/codeberg.org/ninech/foo/releases/download/v1.0.0/foo.tar.gz
curl -LO https://mirror.example.com/gitlab.com/group/subgroup/foo/-/releases/v1.0.0/downloads/foo.tar.gz
```

## Quick start

```bash
release-mirror serve \
  --s3-endpoint="minio.example.com" \
  --s3-bucket="releases-mirror" \
  --s3-public-url="https://cdn.example.com/releases-mirror" \
  --allowed-paths="github.com/ninech/*,github.com/prometheus/*,codeberg.org/ninech/*"
```

## Path patterns

`--allowed-paths` and `--denied-paths` match an asset, given as `<host>/<upstream path>`, segment by segment against shell-style globs. A wildcard never crosses a slash, so each segment is addressed separately. Matching is case-insensitive.

**Allow patterns are anchored at the host** and match a prefix of the path, covering everything below the last segment they name:

| Pattern                   | Matches                                                      |
| ------------------------- | ------------------------------------------------------------ |
| `github.com/ninech/*`     | every repository of the `ninech` organisation on GitHub      |
| `codeberg.org/ninech/foo` | a single repository on Codeberg                              |
| `*/prometheus/alert*`     | `prometheus` repositories starting with `alert`, on any host |
| `github.com`              | everything on github.com                                     |

Nothing is mirrored when `--allowed-paths` is empty; requests are still redirected.

**Deny patterns are not anchored** and match anywhere in the path. Since the mirror does not parse upstream layouts, this is how a path element is rejected wherever a given forge places it. A denied path is never mirrored, however wide the allow list is.

The default, `*/releases/latest/*,permalink/latest`, rejects the aliases forges expose for "the newest release". Those resolve to different bytes over time, while a mirrored object is copied once and served with a one-year immutable cache lifetime — mirroring one would pin a moving target with no way to invalidate it. Pass an empty value to disable.

## Security notes

Because the upstream host comes from the request path, `--allowed-paths` is what decides where the mirror will connect. Nothing is fetched before a path is allowed. Hosts must be plain DNS names: ports, userinfo and IP literals are rejected, so a request cannot address the network the mirror runs in.

## Configuration

Flags can also be set via environment variables using `RELEASE_MIRROR_<NAME>` (e.g. `RELEASE_MIRROR_S3_BUCKET`), unless noted otherwise.

| Flag                     | Default            | Description                                                                 |
| ------------------------ | ------------------ | --------------------------------------------------------------------------- |
| `--s3-bucket`            | _(required)_       | Target S3 bucket                                                            |
| `--s3-endpoint`          | `s3.amazonaws.com` | S3 endpoint (`host[:port]` or URL)                                          |
| `--s3-public-url`        | _(derived)_        | Base public URL where assets are downloaded                                 |
| `--s3-region`            | _(auto)_           | S3 region                                                                   |
| `--s3-access-key-id`     |                    | Static S3 access key (`AWS_ACCESS_KEY_ID` supported)                        |
| `--s3-secret-access-key` |                    | Static S3 secret key (`AWS_SECRET_ACCESS_KEY` supported)                    |
| `--s3-path-style`        | `false`            | Use path-style S3 URLs (`host/bucket/key`)                                  |
| `--s3-insecure`          | `false`            | Connect over plain HTTP                                                     |
| `--allowed-paths`        |                    | Comma-separated host-qualified globs to mirror (e.g. `github.com/ninech/*`) |
| `--denied-paths`         | _(see below)_      | Comma-separated globs never mirrored, matched at any depth                  |
| `--bind`                 | `:8080`            | Listen address (`ADDR` or `PORT` supported)                                 |
| `--metrics`              | `false`            | Enable metrics collection and the `/metrics` endpoint                       |
| `--metrics-bind`         |                    | Dedicated metrics address (e.g. `:9090`, implies metrics)                   |
| `--key-prefix`           |                    | Prefix for mirrored assets in S3                                            |
| `--mirror-workers`       | `4`                | Concurrent copy workers                                                     |
| `--mirror-queue-size`    | `256`              | Pending mirror job queue size                                               |
| `--mirror-timeout`       | `10m`              | Timeout per asset copy                                                      |
| `--max-asset-size`       | `2GiB`             | Max asset size in bytes (`0` for unlimited)                                 |
| `--lock-prefix`          | `.locks/`          | S3 key prefix for distributed locks                                         |
| `--lock-ttl`             | `15m`              | Lock lease duration                                                         |
| `--trusted-proxies`      | `0`                | Upstream reverse proxy count for `X-Forwarded-For`                          |
| `--verbose`              | `false`            | Enable debug logging                                                        |
| `--profile`              | `false`            | Enable `/debug/` profiling endpoints                                        |
| `--sentry-dsn`           |                    | Sentry DSN for error reporting                                              |
| `--sentry-app-name`      |                    | Application name for Sentry (`DEPLOIO_APP_NAME` supported)                  |

## Endpoints

- `/healthz` — Health check endpoint (`release-mirror healthcheck`)
- `/metrics` — Prometheus metrics (when `--metrics` is enabled). With `--metrics-bind` the endpoint is served on that address instead, isolated from the application listener.
- `/debug/` — pprof and fgprof profiling (when `--profile` is enabled)
