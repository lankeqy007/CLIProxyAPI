# Hugging Face Space

Use a separate upload bundle for Spaces. Do not upload the repository root directly.

GitHub and Hugging Face should contain different content:

- GitHub: full source repository
- Hugging Face: generated runtime bundle only

## Build the bundle

Run:

```sh
./scripts/package_hf_space.sh
```

This generates `.dist/hf-space/` containing only:

- `README.md`
- `Dockerfile`
- `entrypoint.sh`
- `config.example.yaml`
- `config.template.yaml`
- compiled server binary

## Publish

Run:

```sh
HF_SPACE_ID=your-hf-username/your-space-name ./scripts/publish_hf_space.sh
```

To update secrets during publish:

```sh
HF_SPACE_ID=your-hf-username/your-space-name \
HF_CLIENT_API_KEY=your-client-key \
HF_MANAGEMENT_PASSWORD=your-management-password \
./scripts/publish_hf_space.sh
```

To enable the Postgres store:

```sh
HF_SPACE_ID=your-hf-username/your-space-name \
HF_PGSTORE_DSN='postgresql://...' \
HF_PGSTORE_SCHEMA=public \
HF_PGSTORE_LOCAL_PATH=/tmp/app \
./scripts/publish_hf_space.sh
```

## Recommended secrets

```env
CLIENT_API_KEY=<client-auth-key>
MANAGEMENT_PASSWORD=<strong-secret>
TOKEN_ATLAS_API_KEY=<codex-refill-api-key>
```

`CLIENT_API_KEY` is required for remote use.
`TOKEN_ATLAS_API_KEY` is required only if you enable `quota-exceeded.codex-auto-refill`.

## Codex Auto Refill

To enable automatic Codex auth-file replenishment on Spaces, turn on `quota-exceeded.codex-auto-refill.enable`
in your config and set the `TOKEN_ATLAS_API_KEY` secret.

The bundled config templates already include the full `codex-auto-refill` block with safe defaults.

## Persistence

Free Spaces use ephemeral disk by default. Runtime data is recreated after rebuilds or cold starts.

For Postgres-backed deployments on free Spaces, set `PGSTORE_LOCAL_PATH=/tmp/app`.
Only use `/data/app` if you have explicitly enabled persistent storage for the Space.
