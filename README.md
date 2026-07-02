# Web Worker Shell

Small local web UI for persistent shell sessions and workspace file browsing.

## Run

```bash
npm install
npm start
```

Open the printed URL. If `WEB_WORKER_TOKEN` is not set, the server generates a runtime token and prints it.

## Security defaults

- Binds to `127.0.0.1` by default.
- Requires a bearer token for shell and file APIs.
- Refuses non-local `HOST` unless `WEB_WORKER_TOKEN` is set.
- Restricts file browsing/upload/download to `WEB_WORKER_ROOT` or the project directory.
- Blocks path traversal and symlink escapes with `realpath`.
- Limits uploads with `WEB_WORKER_MAX_UPLOAD_MB` (default `100`).

For remote use, put it behind SSH/VPN/reverse proxy auth and set a long `WEB_WORKER_TOKEN`.
