# Web Shell

Web Shell 是一个放在服务器上的网页终端。

它做三件事：

- 在浏览器里打开服务器 shell
- 用 tmux 保留会话，刷新页面后还能接回去
- 在指定目录里浏览、下载、上传文件

这个仓库只包含 Node.js 应用代码、前端静态文件、OpenResty 配置示例和 systemd 服务文件。它不包含 Node.js、npm、tmux、OpenResty 这些程序本身，部署前需要在服务器上安装好。

## 端口和组件

默认端口：

| 组件 | 默认监听 | 说明 |
| --- | --- | --- |
| Node.js 应用 | `127.0.0.1:8787` | 只给本机访问，不建议直接暴露公网 |
| OpenResty 示例配置 | `0.0.0.0:18787` | 可选，用来做 HTTPS、Basic Auth、WebSocket 反代 |

默认环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | Node.js 监听地址，生产环境建议保持这个值 |
| `PORT` | `8787` | Node.js 监听端口 |
| `WEB_WORKER_ROOT` | 当前项目目录 | 网页里能访问的文件根目录 |
| `WEB_WORKER_MAX_UPLOAD_MB` | `100` | 单个上传文件大小限制，单位 MB |
| `WEB_WORKER_TMUX_SOCKET` | `web-worker-shell` | tmux socket 名称 |

安全底线：

- 不要把 `8787` 直接开放到公网。
- 不要把 `WEB_WORKER_ROOT` 设成 `/`，除非你明确知道风险。
- 正式使用必须放在 HTTPS 和登录保护后面。

## 服务器准备

以下命令按 Debian/Ubuntu 写，其他系统按同样思路安装即可。

```bash
sudo apt update
sudo apt install -y git tmux openssl build-essential python3 make g++
```

还需要 Node.js 和 npm。建议用生产环境中仍在维护的 Node.js 版本，并确认：

```bash
node -v
npm -v
tmux -V
```

`node-pty` 是原生模块，安装依赖时可能需要编译工具。上面的 `build-essential python3 make g++` 就是为这个准备的。

## 安装代码

默认安装到 `/opt/project/web_worker`：

```bash
sudo mkdir -p /opt/project
sudo chown "$USER":"$USER" /opt/project
git clone git@github.com:systemime/web_shell.git /opt/project/web_worker
cd /opt/project/web_worker
npm ci --omit=dev
```

这是 npm 项目的生产安装方式：按 `package-lock.json` 精确安装依赖，并跳过开发依赖。本项目没有打包步骤，静态文件直接由 Node.js 服务提供，所以不需要 `npm run build`。

如果以后更新代码：

```bash
cd /opt/project/web_worker
git pull
npm ci --omit=dev
sudo systemctl restart webshell
```

## 配置环境变量

创建 `/etc/webshell.env`：

```bash
sudo install -m 600 /dev/null /etc/webshell.env
sudo nano /etc/webshell.env
```

推荐先写：

```ini
NODE_ENV=production
HOST=127.0.0.1
PORT=8787
WEB_WORKER_ROOT=/opt/project/web_worker
WEB_WORKER_MAX_UPLOAD_MB=100
WEB_WORKER_TMUX_SOCKET=web-worker-shell
```

改参数时注意：

- 改 `PORT` 后，反向代理里的 upstream 端口也要一起改。
- 改 `WEB_WORKER_ROOT` 后，运行服务的用户必须有这个目录的读写权限。
- 改 `WEB_WORKER_MAX_UPLOAD_MB` 后，反向代理的上传限制也要一起改，比如 Nginx/OpenResty 的 `client_max_body_size`。

## 生产运行

不要用 `npm start` 在 SSH 里长期挂着。正式部署用 systemd、PM2 或同类进程管理工具。

仓库里的 `systemd/webshell.service` 是一个现成的 systemd 示例，但它假设：

- 项目路径是 `/opt/project/web_worker`
- `node` 在 `/usr/local/bin/node`
- `openresty` 在 `/usr/local/bin/openresty`
- 你要使用仓库里的 `openresty/conf/nginx.conf`

先确认路径：

```bash
which node
which openresty
```

如果路径不同，先改 `systemd/webshell.service`。如果你不用 OpenResty，也不要直接复制这个 service，见下面“已有 Nginx/Caddy/宝塔”一节。

这个服务文件没有写 `User=`，systemd 默认会用 root 启动。网页终端会拿到同样的系统权限。更稳的做法是给它单独建用户，并在 service 的 `[Service]` 里加 `User=你的用户`；如果你确实要 root shell，再保留默认行为。

安装 service：

```bash
sudo cp systemd/webshell.service /etc/systemd/system/webshell.service
sudo systemctl daemon-reload
sudo systemctl enable --now webshell
sudo systemctl status webshell --no-pager
```

查看日志：

```bash
sudo journalctl -u webshell -f
```

## 方式一：使用仓库里的 OpenResty 配置

适合没有现成反向代理的机器。

注意：仓库只提供 `openresty/conf/nginx.conf`，不提供 OpenResty 程序。你必须先在系统里安装 OpenResty，并确保 `openresty` 命令可用。

### 1. 创建运行目录

```bash
cd /opt/project/web_worker
mkdir -p openresty/auth openresty/certs openresty/logs
```

### 2. 设置登录密码

这里用户名用 `webworker`，密码由你输入：

```bash
read -r -s -p 'Password: ' WEB_SHELL_PASSWORD; echo
printf 'webworker:%s\n' "$(openssl passwd -apr1 "$WEB_SHELL_PASSWORD")" > openresty/auth/.htpasswd
unset WEB_SHELL_PASSWORD
chmod 644 openresty/auth/.htpasswd
```

### 3. 生成 HTTPS 证书

有域名时：

```bash
SERVER_NAME=shell.example.com
SAN=DNS:shell.example.com
openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
  -keyout openresty/certs/web-worker.key \
  -out openresty/certs/web-worker.crt \
  -subj "/CN=${SERVER_NAME}" \
  -addext "subjectAltName=${SAN},IP:127.0.0.1,DNS:localhost"
chmod 600 openresty/certs/web-worker.key openresty/certs/web-worker.crt
```

只用 IP 时：

```bash
SERVER_NAME=1.2.3.4
SAN=IP:1.2.3.4
openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
  -keyout openresty/certs/web-worker.key \
  -out openresty/certs/web-worker.crt \
  -subj "/CN=${SERVER_NAME}" \
  -addext "subjectAltName=${SAN},IP:127.0.0.1,DNS:localhost"
chmod 600 openresty/certs/web-worker.key openresty/certs/web-worker.crt
```

自签证书会被浏览器提示“不安全”，这是正常现象。生产环境最好换成正式证书，然后把证书路径写到 `openresty/conf/nginx.conf` 的 `ssl_certificate` 和 `ssl_certificate_key`。

### 4. 检查配置并访问

```bash
sudo systemctl restart webshell
curl -k -I https://127.0.0.1:18787/
```

浏览器访问：

```text
https://服务器IP:18787
```

云服务器安全组只需要放行 `18787`。不要放行 `8787`。

## 方式二：已有 Nginx/Caddy/宝塔

适合服务器上已经有统一入口，比如 Nginx、Caddy、宝塔面板。

这种情况下可以不使用仓库里的 OpenResty 配置。你只需要让 Node.js 应用监听本机 `127.0.0.1:8787`，然后在现有反向代理里完成：

- HTTPS
- 登录保护
- WebSocket 转发
- 上传大小限制

### Node-only systemd 示例

如果不用 OpenResty，可以新建一个更简单的 service：

```ini
[Unit]
Description=Web Shell
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/project/web_worker
EnvironmentFile=/etc/webshell.env
# User=webshell
ExecStart=/usr/local/bin/node /opt/project/web_worker/server.js
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

保存到 `/etc/systemd/system/webshell.service` 后：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now webshell
```

如果 `node` 不在 `/usr/local/bin/node`，用 `which node` 查到的路径替换。

### Nginx 示例

下面示例直接代理到 Node.js：

```nginx
server {
  listen 443 ssl;
  server_name shell.example.com;

  ssl_certificate /path/to/fullchain.pem;
  ssl_certificate_key /path/to/privkey.pem;

  client_max_body_size 100m;

  auth_basic "web-shell";
  auth_basic_user_file /etc/nginx/webshell.htpasswd;

  location / {
    proxy_pass http://127.0.0.1:8787;
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
  }
}
```

生成 Nginx Basic Auth 密码文件：

```bash
read -r -s -p 'Password: ' WEB_SHELL_PASSWORD; echo
printf 'webworker:%s\n' "$(openssl passwd -apr1 "$WEB_SHELL_PASSWORD")" | sudo tee /etc/nginx/webshell.htpasswd >/dev/null
unset WEB_SHELL_PASSWORD
sudo chmod 644 /etc/nginx/webshell.htpasswd
sudo nginx -t
sudo systemctl reload nginx
```

建议使用独立域名，例如 `shell.example.com`。不要挂到 `/webshell/` 子路径；当前前端资源和接口按根路径设计，子路径部署需要额外改代理规则。

## 常用检查

检查 Node.js 应用：

```bash
curl -I http://127.0.0.1:8787/
```

检查 OpenResty 入口：

```bash
curl -k -I https://127.0.0.1:18787/
```

检查 systemd：

```bash
sudo systemctl status webshell --no-pager
sudo journalctl -u webshell -f
```

检查 OpenResty 日志：

```bash
tail -f /opt/project/web_worker/openresty/logs/error.log
```

常见问题：

- `502 Bad Gateway`：Node.js 没起来，先看 `journalctl -u webshell -f`。
- 能打开页面但终端连不上：反向代理少了 WebSocket 的 `Upgrade`/`Connection` 配置。
- 上传失败：同时检查 `WEB_WORKER_MAX_UPLOAD_MB` 和反向代理的 `client_max_body_size`。
- Basic Auth 一直失败：重新生成 `.htpasswd`，并确认反向代理能读取这个文件。
- `openresty: command not found`：系统没有安装 OpenResty，仓库里只有配置文件。

## 运行时文件

这些文件是服务器运行时生成的，不应该提交到仓库：

- `openresty/auth/.htpasswd`
- `openresty/certs/*`
- `openresty/logs/*`
- `.web-worker-tmp/`
- `session-titles.json`

换机器部署时，重新生成密码和证书即可。
