# Web Shell

一个放在服务器上的网页终端。浏览器打开后，可以进 shell、保留 tmux 会话，也能在限定目录里浏览、下载、上传文件。

默认结构很简单：

- Node 服务只听 `127.0.0.1:8787`
- OpenResty 对外提供 `https://服务器:18787`
- OpenResty 负责 HTTPS、Basic Auth、WebSocket 反代
- 文件访问范围默认是项目目录，也可以用 `WEB_WORKER_ROOT` 改

不要把 `8787` 端口直接暴露到公网。要远程访问，走 OpenResty、SSH/VPN，或你自己的反向代理。

## 本地试跑

先准备 Node.js、npm、tmux：

```bash
npm ci
npm start
```

看到类似下面的输出后，用浏览器打开：

```text
Web worker shell: http://127.0.0.1:8787/
```

## 服务器部署

下面按默认路径 `/opt/project/web_worker` 写。路径可以改，但改了以后要同步调整 `systemd/webshell.service` 和 `openresty/conf/nginx.conf` 里的绝对路径。

### 1. 放代码

```bash
sudo mkdir -p /opt/project
sudo git clone git@github.com:systemime/web_shell.git /opt/project/web_worker
cd /opt/project/web_worker
npm ci --omit=dev
```

如果用源码包部署：

```bash
sudo mkdir -p /opt/project
sudo tar -xzf web_shell-*.tar.gz -C /opt/project
cd /opt/project/web_worker
npm ci --omit=dev
```

如果 `npm ci` 在 `node-pty` 这里失败，通常是缺编译工具。Debian/Ubuntu 可以先装：

```bash
sudo apt install -y build-essential python3 make g++
```

### 2. 准备环境变量

```bash
sudo install -m 600 /dev/null /etc/webshell.env
sudo nano /etc/webshell.env
```

建议先写这些：

```ini
HOST=127.0.0.1
PORT=8787
WEB_WORKER_ROOT=/opt/project/web_worker
WEB_WORKER_MAX_UPLOAD_MB=100
WEB_WORKER_TMUX_SOCKET=web-worker-shell
```

需要注意：

- `HOST` 建议保持 `127.0.0.1`，让 Node 只在本机监听。
- `PORT` 如果改了，`openresty/conf/nginx.conf` 里的 `proxy_pass http://127.0.0.1:8787;` 也要一起改。
- `WEB_WORKER_ROOT` 是网页里能看到和操作的根目录。不要随手设成 `/`。
- `WEB_WORKER_MAX_UPLOAD_MB` 如果改大，OpenResty 的 `client_max_body_size 100m;` 也要一起改。

### 3. 生成登录密码

OpenResty 用 Basic Auth 挡在外面。先建目录：

```bash
mkdir -p openresty/auth openresty/certs openresty/logs
```

创建账号，默认用户名这里用 `webworker`：

```bash
read -r -s -p 'Password: ' WEB_SHELL_PASSWORD; echo
printf 'webworker:%s\n' "$(openssl passwd -apr1 "$WEB_SHELL_PASSWORD")" > openresty/auth/.htpasswd
unset WEB_SHELL_PASSWORD
chmod 644 openresty/auth/.htpasswd
```

以后访问页面时，浏览器会弹用户名和密码。

### 4. 生成 HTTPS 证书

有域名时：

```bash
SERVER_NAME=example.com
SAN=DNS:example.com
openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
  -keyout openresty/certs/web-worker.key \
  -out openresty/certs/web-worker.crt \
  -subj "/CN=${SERVER_NAME}" \
  -addext "subjectAltName=${SAN},IP:127.0.0.1,DNS:localhost"
chmod 600 openresty/certs/web-worker.key openresty/certs/web-worker.crt
```

没有域名、只用 IP 时，把前两行换成：

```bash
SERVER_NAME=1.2.3.4
SAN=IP:1.2.3.4
```

自签证书浏览器会提示不安全，这是正常的。要消掉提示，换成正式证书，把证书路径仍放到 `openresty/certs/web-worker.crt` 和 `openresty/certs/web-worker.key`，或修改 `openresty/conf/nginx.conf` 里的证书路径。

### 5. 安装 systemd 服务

确认机器上能找到这两个命令：

```bash
which node
which openresty
```

默认 service 写的是 `/usr/local/bin/node` 和 `/usr/local/bin/openresty`。如果你的路径不同，先改 `systemd/webshell.service`。

安装并启动：

```bash
sudo cp systemd/webshell.service /etc/systemd/system/webshell.service
sudo systemctl daemon-reload
sudo systemctl enable --now webshell
sudo systemctl status webshell --no-pager
```

访问：

```text
https://你的服务器IP:18787
```

如果云服务器有安全组，只放行 `18787`。不要放行 `8787`。

## 挂到现有反向代理后面

最省事的做法：保留本项目自带 OpenResty，让你的外层 Nginx/宝塔/Caddy 代理到 `https://127.0.0.1:18787`。这样 Basic Auth、WebSocket、上传限制仍由项目内的 OpenResty 处理。

Nginx 示例：

```nginx
server {
  listen 443 ssl;
  server_name shell.example.com;

  ssl_certificate /path/to/fullchain.pem;
  ssl_certificate_key /path/to/privkey.pem;

  location / {
    proxy_pass https://127.0.0.1:18787;
    proxy_ssl_verify off;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 1d;
    proxy_send_timeout 1d;
    proxy_buffering off;
    client_max_body_size 100m;
  }
}
```

注意三点：

- 尽量用单独域名，比如 `shell.example.com`，不要挂到 `/webshell/` 这种子路径。
- 外层代理也要保留 WebSocket 的 `Upgrade` 和 `Connection` 头。
- 如果你决定不用项目内 OpenResty，外层代理必须自己加登录保护，然后代理到 `http://127.0.0.1:8787`。

## 常用检查

```bash
sudo systemctl status webshell --no-pager
sudo journalctl -u webshell -f
tail -f openresty/logs/error.log
curl -I http://127.0.0.1:8787/
curl -k -I https://127.0.0.1:18787/
```

常见问题：

- `502`：Node 没起来，先看 `journalctl -u webshell -f`。
- 登录一直失败：重新生成 `openresty/auth/.htpasswd`，并确认文件能被 OpenResty 读取。
- 上传失败：同时检查 `WEB_WORKER_MAX_UPLOAD_MB` 和 `client_max_body_size`。
- 页面能打开但终端连不上：反向代理少了 WebSocket 相关配置。

## 会写到本地的文件

这些文件不会提交到仓库：

- `openresty/auth/.htpasswd`
- `openresty/certs/*`
- `openresty/logs/*`
- `.web-worker-tmp/`
- `session-titles.json`

换服务器部署时，重新生成密码和证书即可。
