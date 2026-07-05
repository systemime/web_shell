# Web Shell

Web Shell 是一个可以放在服务器上的网页终端。它提供一个浏览器 UI，用来打开服务器 shell、保留 tmux 会话，并在指定目录内浏览、上传、下载文件。

当前版本是 Go 单二进制实现：Web UI 和后端服务都打包在一个 `webshell` 文件里。运行时仍需要系统已安装 `tmux`。

## 功能

- 浏览器终端，基于 xterm.js
- tmux 会话保留，刷新页面后可重新接回
- 多 shell 会话管理、重命名、关闭
- 指定根目录内的文件浏览、上传、下载
- 支持 OpenResty/Nginx 反向代理和 WebSocket 转发

## 直接使用

### 1. 安装 tmux

Debian/Ubuntu：

```bash
sudo apt update
sudo apt install -y tmux
```

### 2. 下载二进制

按服务器架构选择下载：

```bash
# x86_64 / amd64
curl -L -o webshell https://github.com/systemime/web_shell/releases/latest/download/webshell-linux-amd64

# arm64 / aarch64
curl -L -o webshell https://github.com/systemime/web_shell/releases/latest/download/webshell-linux-arm64

chmod +x webshell
```

### 3. 启动

```bash
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
| `WEB_WORKER_TMUX_SOCKET` | `web-worker-shell` | tmux socket 名称 |

## 使用 OpenResty/Nginx 代理

推荐公网只暴露 HTTPS 反代端口，让 Web Shell 继续监听本机 `127.0.0.1:8787`。

### 1. systemd 服务

把二进制放到：

```bash
sudo install -m 755 webshell /opt/project/web_worker/webshell
```

创建环境文件：

```bash
sudo install -m 600 /dev/null /etc/webshell.env
sudo tee /etc/webshell.env >/dev/null <<'EOF_ENV'
HOST=127.0.0.1
PORT=8787
WEB_WORKER_ROOT=/opt
WEB_WORKER_MAX_UPLOAD_MB=100
EOF_ENV
```

创建 systemd unit：

```bash
sudo tee /etc/systemd/system/webshell.service >/dev/null <<'EOF_SERVICE'
[Unit]
Description=Web Shell
After=network.target

[Service]
Type=simple
EnvironmentFile=/etc/webshell.env
ExecStart=/opt/project/web_worker/webshell
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF_SERVICE

sudo systemctl daemon-reload
sudo systemctl enable --now webshell
```

### 2. OpenResty/Nginx 配置

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

## 从源码构建

```bash
go build -o webshell .
```
