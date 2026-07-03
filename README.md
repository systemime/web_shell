# Web Worker Shell

Small local web UI for persistent shell sessions and workspace file browsing.

## Run

```bash
npm install
npm start
```

Open the printed URL.

## OpenResty HTTPS

The OpenResty config serves the app on `https://<host>:18787` and proxies to the local Node server. The TLS cert/key are runtime files and are not committed:

```bash
mkdir -p openresty/certs
openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
  -keyout openresty/certs/web-worker.key \
  -out openresty/certs/web-worker.crt \
  -subj '/CN=<host>' \
  -addext 'subjectAltName=IP:<host>,IP:127.0.0.1,DNS:localhost'
```

## systemd

The `webshell` service runs the Node app and starts/reloads the project OpenResty proxy:

```bash
install -m 600 /dev/null /etc/webshell.env
$EDITOR /etc/webshell.env
ln -sf /opt/project/web_worker/systemd/webshell.service /etc/systemd/system/webshell.service
systemctl daemon-reload
systemctl enable --now webshell
```

## Security defaults

- Binds to `127.0.0.1` by default.
- Keep the Node app bound to `127.0.0.1` and expose it through OpenResty Basic Auth.
- Restricts file browsing/upload/download to `WEB_WORKER_ROOT` or the project directory.
- Blocks path traversal and symlink escapes with `realpath`.
- Limits uploads with `WEB_WORKER_MAX_UPLOAD_MB` (default `100`).

For remote use, keep it behind SSH/VPN/reverse proxy auth.
