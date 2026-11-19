# Deplyt

Deplyt 接收 PhotoLib GitHub Actions 打包后的 JAR，将校验通过的文件上传到阿里云 OSS，再通过 SSH 让生产机从加速域名下载。生产机重新校验 SHA-256，切换 JAR，启动服务并检查 systemd 状态。控制台通过 PhotoLib 管理员单点登录，提供部署及回滚记录、提交、PR、Actions 和安全告警。

## 安装到 Linux 中转机

使用 Go 1.26 和 Node 22.12 或更新的 Node 22 构建。Svelte 的 HTML、CSS 和 JS 已嵌入 Go 可执行文件。GitHub Actions 的 Build 工作流也提供 `deplyt-linux-amd64` 产物。

```sh
npm --prefix frontend ci
npm --prefix frontend run build
CGO_ENABLED=0 go build -trimpath -o deplyt .
```

在中转机安装二进制、配置文件和 systemd 服务。

```sh
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
| `OSS_BUCKET` | PhotoLib 当前使用的私有 Bucket。 |
| `OSS_REGION` | Bucket 所在地域，例如 `cn-hangzhou`。 |
| `OSS_ACCESS_KEY_ID`、`OSS_ACCESS_KEY_SECRET` | Deplyt 专用 RAM 凭据，仅需访问下述 JAR 前缀。 |
| `DEPLYT_PUBLIC_URL`、`PHOTOLIB_PUBLIC_URL` | Deplyt 和 PhotoLib 的 HTTPS 源地址，例如 `https://deploy.example.com`，带非默认端口时需填写端口。 |
| `DEPLYT_SSO_SECRET` | SSO 客户端密钥。通过 `openssl rand -hex 32` 生成独立于上传令牌的密钥，并在 PhotoLib 和 Deplyt 填入相同值。 |
| `GITHUB_TOKEN` | 仅在 Deplyt 后端使用，限定到 `tobby888/Photolib` 的细粒度 PAT。 |
| `DEPLOY_STATE_DIR` | 部署记录目录。systemd 默认使用 `/var/lib/deplyt`，手动运行默认使用当前目录的 `data/`。升级时保留该目录。 |

## OSS 归档与回滚

1. 为共享 Bucket 开启[传输加速](https://www.alibabacloud.com/help/en/oss/user-guide/transfer-acceleration)。Deplyt 使用 `<bucket>.oss-accelerate.aliyuncs.com` 上传与签名，两个服务器均需能访问该地址。PhotoLib 继续使用原有 OSS 配置。
2. 仅授予 Deplyt 凭据对 `deplyt/photolib/*` 的 `oss:PutObject` 和 `oss:GetObject`。确保 Bucket 生命周期规则对该前缀永久保留，且不会转到需要解冻的存储类型。
3. 对象键为 `deplyt/photolib/photolib-backend-<run-id>-<部署记录ID>-<完整sha256>.jar`。每次 CI 上传分配独立 ID，重跑也会保留自己的归档。对象按私有 ACL 写入。部署失败或回滚都保留已上传的对象。
4. 生产机需提供 `curl`、`bash`、`sha256sum`、`flock`、现有的 `maint-on`、`maint-off` 和 `photolib.service`。目录 `/opt/Photolib` 应仅有一个匹配 `photolib-backend-*.jar` 的运行包。切换过程中临时把旧包移出匹配范围，新服务启动失败时尝试恢复旧包。
5. 控制台可选择曾部署成功的归档，确认后从原对象下载并校验，再执行同一套切换流程。回滚记录保留原 SHA-256、来源提交、运行编号和操作管理员。控制台显示最后确认版本，超时或进程中断后可通过日志核实生产状态。首次上线前的部署没有 Deplyt 记录。

## PhotoLib 单点登录与 GitHub

将配套变更部署到 PhotoLib，并在 PhotoLib 后端 `.env` 加入相同的 `DEPLYT_PUBLIC_URL` 与 `DEPLYT_SSO_SECRET`。Deplyt 回调为 `/auth/callback`。浏览器会先进入 PhotoLib 的授权页面，未登录时在 PhotoLib 完成现有登录流程。仅 PhotoLib 的 `ADMIN` 权限组可以授权。授权码有效期为 60 秒且只能使用一次，Deplyt 会话有效期为 30 分钟。用户可从控制台退出。

为 `GITHUB_TOKEN` 授予 Contents、Pull requests、Actions、Dependabot alerts、Code scanning alerts、Secret scanning alerts 的 Read 权限。GitHub 仓库需启用对应扫描功能，控制台会在相应板块显示权限或功能错误。凭据扫描页只展示告警元数据。[GitHub PAT 权限文档](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)

## 连接 PhotoLib 的 GitHub Actions

在 PhotoLib 仓库配置以下 Actions 变量和 Secret，再将更新的工作流合并到 `master`。

| 类型 | 名称 | 值 |
| --- | --- | --- |
| Variable | `PHOTOLIB_DEPLOY_URL` | 中转机的 HTTPS 基地址，例如 `https://deploy.example.com`。 |
| Secret | `PHOTOLIB_DEPLOY_TOKEN` | 中转机 `.env` 中的 `DEPLOY_TOKEN`。 |
| Variable（可选） | `PHOTOLIB_DEPLOY_RUNNER` | 中转机仅在内网可达时，填能访问它的 Linux self-hosted runner 专用标签。留空时使用 `ubuntu-latest`。 |

在中转机的 HTTPS 反向代理中配置部署入口。使用内网 CA 时，将其根证书加入部署 runner 的信任库。以下 Nginx 片段放在 Deplyt 域名的 HTTPS `server` 块内。控制台与上传接口共用域名，默认 JAR 上限是 512 MiB，CI 和回滚最多等待一小时。

```nginx
location / {
    proxy_read_timeout 60m;
    proxy_pass http://127.0.0.1:8080;
}

location = /auth/callback {
    access_log off;
    proxy_pass http://127.0.0.1:8080;
}

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

部署接口是 `POST /v1/deploy/photolib`，请求体为原始 JAR 字节。请求头包括 `Authorization: Bearer <token>`、`X-Artifact-Name`、`X-Artifact-SHA256` 和 `X-Deploy-Run-ID`。工作流也可传入 `X-Deploy-Commit`，将上传关联到提交 SHA。中转服务单实例串行处理部署，正在处理时的新请求返回 `409`。通过校验的请求会继续执行，即使调用方断开。网络中断时可在 `journalctl -u deplyt` 中用 `run_id` 核实部署结果，再决定是否重跑工作流。

## 本地验证

```sh
npm --prefix frontend ci
npm --prefix frontend run build
go test -race ./...
go vet ./...
bash -n restart.sh
```

测试中的 OSS、SSH、PhotoLib 和 GitHub 使用本地替身。
