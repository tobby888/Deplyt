# Deplyt

Deplyt 是 PhotoLib 的 Go 部署中转服务。PhotoLib 的 GitHub Actions 完成现有 CI 和 Maven 打包后，通过 HTTPS 将现成的 JAR 发给中转机。中转机校验令牌、文件大小、SHA-256 和 PhotoLib JAR 的必需条目，再通过 SSH 传给生产机。生产机校验收到的文件后，执行 PhotoLib 原有的维护模式、切换 JAR 和重启流程。只有重启完成且 `photolib` 服务通过状态检查，HTTP 请求才会返回 `200`。

## 安装到 Linux 中转机

用 Go 1.26 或更新版本编译，并在中转机上执行以下安装命令。

```sh
go build -o deplyt .
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin deplyt
sudo install -d -m 0755 /opt/deplyt
sudo install -m 0755 deplyt /opt/deplyt/deplyt
sudo install -o root -g deplyt -m 0640 .env.example /opt/deplyt/.env
sudoedit /opt/deplyt/.env
sudo install -m 0644 deplyt.service /etc/systemd/system/deplyt.service
sudo systemctl daemon-reload
sudo systemctl enable --now deplyt
curl -fsS http://127.0.0.1:8080/healthz
```

`/opt/deplyt/.env` 在启动时读取，修改后执行 `sudo systemctl restart deplyt`。其中 `.env` 的初始化命令仅首次安装时执行。

| 配置 | 用途 |
| --- | --- |
| `DEPLOY_TOKEN` | 上传令牌。可用 `openssl rand -hex 32` 生成，和 Actions Secret 使用同一个值。 |
| `DEPLOY_LISTEN_ADDR` | 监听地址，默认 `127.0.0.1:8080`，供同机 HTTPS 反向代理使用。 |
| `PRODUCTION_SSH_HOST` | 生产机 IP。填中转机可达的地址，可以使用内网 IP。 |
| `PRODUCTION_SSH_PORT` | 生产机 SSH 端口，默认 `22`。 |
| `PRODUCTION_SSH_USER` | 现有生产部署用户，默认 `root`。该用户需要能写入 `/opt/Photolib`、执行维护命令、切换 JAR 的属主权限及管理 `photolib` 服务。 |
| `PRODUCTION_SSH_PASSWORD` | 生产机 SSH 密码。用单引号保留 `$`、`#` 等字符。 |
| `PRODUCTION_SSH_HOST_KEY` | 从可信渠道取得的生产机 SSH host 公钥整行，例如读取生产机的 `/etc/ssh/ssh_host_ed25519_key.pub` 后用单引号包住内容。SSH 握手会固定校验该公钥。 |

生产机需继续提供现有的 `maint-on`、`maint-off` 和 `photolib.service`，以及 `bash` 和 `sha256sum`。中转机要能连到生产机的 SSH 端口。

## 连接 PhotoLib 的 GitHub Actions

在 PhotoLib 仓库配置以下 Actions 变量和 Secret，再将更新的工作流合并到 `master`。

| 类型 | 名称 | 值 |
| --- | --- | --- |
| Variable | `PHOTOLIB_DEPLOY_URL` | 中转机的 HTTPS 基地址，例如 `https://deploy.example.com`。 |
| Secret | `PHOTOLIB_DEPLOY_TOKEN` | 中转机 `.env` 中的 `DEPLOY_TOKEN`。 |
| Variable（可选） | `PHOTOLIB_DEPLOY_RUNNER` | 中转机仅在内网可达时，填能访问它的 Linux self-hosted runner 专用标签。留空时使用 `ubuntu-latest`。 |

在中转机的 HTTPS 反向代理中配置部署入口。使用内网 CA 时，将其根证书加入部署 runner 的信任库。以下 Nginx 片段放在现有 HTTPS `server` 块内。默认 JAR 上限是 512 MiB，CI 最多等待一小时。

```nginx
location = /v1/deploy/photolib {
    client_max_body_size 512m;
    client_body_timeout 12m;
    proxy_http_version 1.1;
    proxy_request_buffering off;
    proxy_send_timeout 12m;
    proxy_read_timeout 60m;
    proxy_pass http://127.0.0.1:8080;
}
```

部署接口是 `POST /v1/deploy/photolib`，请求体为原始 JAR 字节。请求头包括 `Authorization: Bearer <token>`、`X-Artifact-Name`、`X-Artifact-SHA256` 和 `X-Deploy-Run-ID`。中转服务单实例串行处理部署，正在处理时的新请求返回 `409`。通过校验的请求会继续执行，即使调用方断开。网络中断时可在 `journalctl -u deplyt` 中用 `run_id` 核实部署结果，再决定是否重跑工作流。
