# Docker Deployment

CLIProxyAPI can run as a single Docker container on generic platforms, including Hugging Face Docker Spaces and Render.

## Runtime Environment Variables

The container now supports runtime overrides in addition to `config.yaml`:

- `DEPLOY=cloud`: enables cloud-friendly startup behavior when configuration is provided later or from a remote store
- `HOST`: overrides `host` from `config.yaml`
- `PORT`: overrides `port` from `config.yaml`
- `APP_PORT`: alias for `PORT`
- `PGSTORE_DSN`: enables the Postgres-backed config/token store
- `PGSTORE_SCHEMA`: optional Postgres schema, defaults to `public`
- `PGSTORE_LOCAL_PATH`: optional local spool directory
- `MANAGEMENT_PASSWORD`: enables the management API without hardcoding a secret in `config.yaml`

The unauthenticated health endpoints are:

- `GET /healthz`
- `GET /readyz`

## Hugging Face Docker Spaces

Set the Space metadata in `README.md`:

```yaml
---
title: CLIProxyAPI
sdk: docker
app_port: 8317
---
```

Recommended Space secrets:

```env
MANAGEMENT_PASSWORD=<strong-secret>
```

Runtime behavior on Hugging Face:

- the Docker entrypoint detects `SPACE_ID` / `SPACE_HOST`
- `HOST` defaults to `0.0.0.0`
- `PORT` defaults to `8317`
- a writable runtime root is chosen automatically:
  - `/data/cliproxyapi` when persistent storage exists
  - `/tmp/cliproxyapi` otherwise
- if no config exists yet, the container bootstraps one automatically

Important limitation:

- Hugging Face free Spaces have ephemeral disk by default
- direct Postgres DSNs such as `PGSTORE_DSN` are not the primary recommendation on HF
- for free HF-first deployment, start with local runtime storage and configure the service through the management UI

If you need data to survive restarts, use Hugging Face persistent storage mounted at `/data` or add a later HTTPS-based persistence layer.

## Render

Render injects a `PORT` environment variable for web services. CLIProxyAPI will now honor it automatically.

Minimal Blueprint example:

```yaml
services:
  - type: web
    name: cliproxyapi
    runtime: docker
    plan: free
    healthCheckPath: /healthz
    envVars:
      - key: DEPLOY
        value: cloud
      - key: MANAGEMENT_PASSWORD
        sync: false
      - key: PGSTORE_DSN
        sync: false
      - key: PGSTORE_SCHEMA
        value: public
```

You can also configure the same values directly in the Render dashboard instead of using a Blueprint.

## Supabase

Supabase remains a better fit for platforms that allow direct Postgres connections, such as Render, Railway, Fly.io, or your own VPS/container host.
