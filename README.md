# Web Shell

Web Shell 是一个 Go 单二进制网页终端：Web UI/API 通过独立 `webshell sessiond` 守护进程管理本机 shell PTY，前端用 xterm.js 渲染终端，并提供指定目录内的文件浏览、上传、下载。

## 功能

- 浏览器终端，基于 xterm.js
- 独立 sessiond + 轻量 session worker 持有 PTY，无需 tmux；webshell 主进程重启后可重新接回
- 多 shell 会话管理、重命名、关闭
- 指定根目录内的文件浏览、上传、下载
- 支持 OpenResty/Nginx 反向代理和 WebSocket 转发

## 直接使用

下载对应架构的二进制：

```bash
# x86_64 / amd64
curl -L -o webshell https://github.com/systemime/web_shell/releases/latest/download/webshell-linux-amd64

# arm64 / aarch64
curl -L -o webshell https://github.com/systemime/web_shell/releases/latest/download/webshell-linux-arm64

chmod +x webshell
```

启动：

```bash
# 先启动独立会话守护进程
WEB_WORKER_ROOT=/opt ./webshell sessiond &

# 再启动 Web UI/API
WEB_WORKER_ROOT=/opt ./webshell
```

默认监听：

```text
http://127.0.0.1:8787
```

常用环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | 监听地址 |
| `PORT` | `8787` | 监听端口 |
| `WEB_WORKER_ROOT` | 当前目录 | 网页可访问的文件根目录 |
| `WEB_WORKER_MAX_UPLOAD_MB` | `100` | 单文件上传大小限制 |
| `SHELL` | `/bin/bash` | 新建 shell 使用的程序 |

会话说明：

- shell 由独立 `webshell sessiond` 守护进程管理，PTY 仍由轻量 `session-worker` 持有，元数据和 Unix socket 放在 `.web-worker-sessions/`。
- systemd 配置里的 `webshell.service` 依赖 `webshell-sessiond.service`；重启主 `webshell` 不会重启 sessiond，因此重新部署后可恢复已有终端。
- 如果手动杀掉 `session-worker`、重启机器，或把 systemd 改回 `KillMode=control-group`，对应终端仍会结束。

## 使用 OpenResty/Nginx 代理

推荐公网只暴露代理端口，让 Web Shell 继续监听本机 `127.0.0.1:8787`。

### systemd 服务

```bash
sudo install -m 755 webshell /opt/project/web_worker/webshell

sudo install -m 600 /dev/null /etc/webshell.env
sudo tee /etc/webshell.env >/dev/null <<'EOF_ENV'
HOST=127.0.0.1
PORT=8787
WEB_WORKER_ROOT=/opt
WEB_WORKER_MAX_UPLOAD_MB=100
SHELL=/bin/bash
EOF_ENV

sudo tee /etc/systemd/system/webshell-sessiond.service >/dev/null <<'EOF_SESSIOND'
[Unit]
Description=Web Shell Session Daemon
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/project/web_worker
EnvironmentFile=/etc/webshell.env
ExecStart=/opt/project/web_worker/webshell sessiond
KillMode=process
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF_SESSIOND

sudo tee /etc/systemd/system/webshell.service >/dev/null <<'EOF_SERVICE'
[Unit]
Description=Web Shell
Requires=webshell-sessiond.service
After=network.target webshell-sessiond.service

[Service]
Type=simple
WorkingDirectory=/opt/project/web_worker
EnvironmentFile=/etc/webshell.env
ExecStart=/opt/project/web_worker/webshell
KillMode=process
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF_SERVICE

sudo systemctl daemon-reload
sudo systemctl enable --now webshell-sessiond webshell
```

### OpenResty/Nginx 配置

下面示例监听 `18787`，代理到本机 `8787`，并支持 WebSocket：

```nginx
map $http_upgrade $connection_upgrade {
  default upgrade;
  '' close;
}

server {
  listen 18787 ssl;
  server_name _;

  ssl_certificate /path/to/fullchain.pem;
  ssl_certificate_key /path/to/privkey.pem;

  client_max_body_size 100m;

  location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 1d;
    proxy_send_timeout 1d;
    proxy_buffering off;
  }
}
```

重载代理：

```bash
sudo nginx -t && sudo systemctl reload nginx
# 或 OpenResty：
sudo openresty -t && sudo openresty -s reload
```

访问：

```text
https://服务器IP或域名:18787
```
