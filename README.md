# Deplyt

Deplyt 是 PhotoLib 的自动化部署平台。控制台跟随 PhotoLib 当前生效的自定义或定时图标，使用 PhotoLib 账号登录。获得授权的成员可以触发 GitHub Actions、部署指定 OSS 制品和查看发布记录。管理员可以管理访问权限、回滚 OSS 归档并查看仓库活动和安全告警。

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
| `OSS_PRODUCTION_ENDPOINT` | 生产机下载归档时使用的 HTTPS 地域 Endpoint，例如 `https://oss-cn-hangzhou-internal.aliyuncs.com`。填不含 Bucket 名的地址。留空时使用 `OSS_REGION` 对应的普通公网 Endpoint。 |
| `OSS_ACCESS_KEY_ID`、`OSS_ACCESS_KEY_SECRET` | Deplyt 专用 RAM 凭据，仅需访问下述 JAR 前缀。 |
| `OSS_ECS_RAM_ROLE` | 在阿里云 ECS 上部署时，可填实例绑定的 RAM 角色名。SDK 自动取得并更新临时凭据。此模式无需填写 AccessKey。 |
| `DEPLYT_PUBLIC_URL`、`PHOTOLIB_PUBLIC_URL` | Deplyt 和 PhotoLib 的 HTTPS 源地址，例如 `https://deploy.example.com`，带非默认端口时需填写端口。 |
| `DEPLOY_ADMIN_PHOTOLIB_ID` | 指定可通过 PhotoLib 登录取得 Deplyt 管理权限的用户数字 ID。 |
| `DEPLYT_SSO_SECRET` | 可选的 PhotoLib 管理员单点登录密钥。通过 `openssl rand -hex 32` 生成，并在 PhotoLib 和 Deplyt 填入相同值。 |
| `DEPLOY_EMERGENCY_PASSWORD_HASH` | 可选的应急管理员密码哈希。使用 Deplyt 的密码生成命令配置。 |
| `GITHUB_TOKEN` | 仅在 Deplyt 后端使用，限定到 `tobby888/Photolib` 的细粒度 PAT。 |
| `GITHUB_ACTIONS_TOKEN` | 限定到 PhotoLib 仓库、具有 `Actions: write` 权限的细粒度 PAT。启用控制台的手动工作流。 |
| `DEPLOY_STATE_DIR` | 部署记录与访问权限目录。systemd 默认使用 `/var/lib/deplyt`，手动运行默认使用当前目录的 `data/`。升级时保留该目录。 |

## OSS 归档与回滚

配置归档凭据后，CI 和手动 OSS 部署都会将校验通过的 JAR 归档到私有 OSS，随后通过 SSH 让生产机使用签名 URL 下载。生产机会重新校验 SHA-256，再切换 JAR、重启服务并检查 systemd 状态。

1. 为共享 Bucket 开启[传输加速](https://www.alibabacloud.com/help/en/oss/user-guide/transfer-acceleration)。中转机使用 `<bucket>.oss-accelerate.aliyuncs.com` 上传。部署和回滚的签名下载链接使用 `OSS_PRODUCTION_ENDPOINT` 对应的地址，由生产机访问。同地域的阿里云 ECS 可配置 [OSS 内网 Endpoint](https://help.aliyun.com/en/oss/user-guide/access-oss-via-bucket-domain-name)。
2. 仅授予 Deplyt 凭据对 `deplyt/photolib/*` 的 `oss:PutObject` 和 `oss:GetObject`。确保 Bucket 生命周期规则对该前缀永久保留，且不会转到需要解冻的存储类型。
3. 对象键为 `deplyt/photolib/photolib-backend-<run-id>-<部署记录ID>-<完整sha256>.jar`。每次 CI 上传分配独立 ID，重跑也会保留自己的归档。对象按私有 ACL 写入。部署失败或回滚都保留已上传的对象。
4. 生产机需提供 `curl`、`bash`、`sha256sum`、`flock`、现有的 `maint-on`、`maint-off` 和 `photolib.service`。目录 `/opt/Photolib` 应仅有一个匹配 `photolib-backend-*.jar` 的运行包。切换过程中临时把旧包移出匹配范围，新服务启动失败时尝试恢复旧包。
5. 控制台可选择曾部署成功的归档，确认后从原对象下载并校验，再执行同一套切换流程。回滚记录保留原 SHA-256、来源提交、运行编号和操作管理员。控制台显示最后确认版本，超时或进程中断后可通过日志核实生产状态。首次上线前的部署没有 Deplyt 记录。

## 使用令牌部署 OSS 制品

| 配置 | 用途 |
| --- | --- |
| `OSS_ARTIFACT_URL` | HTTPS 对象下载 URL。私有 OSS 使用完整的 GetObject 预签名 URL，并保留签名参数。 |
| `OSS_ARTIFACT_NAME` | 可选，默认为 URL 中的文件名。下载端点的路径缺少文件名时，填实际的 `photolib-backend-*.jar` 文件名。 |
| `OSS_ARTIFACT_SHA256` | 可信构建产物的 SHA-256。更新对象时一并更新。 |
| `OSS_ACCESS_TOKEN` | 下载端点支持 Bearer 认证时填写。Deplyt 会发送 `Authorization: Bearer`。使用预签名 URL 时留空。 |

配置后，已授权成员可点「部署 OSS 制品」。Deplyt 会校验文件大小、SHA-256 和 JAR 必需条目。仅配置制品下载时，手动 OSS 部署和 CI 上传都通过 SSH 直接中转校验后的 JAR。配置归档凭据后可回滚到已成功部署并归档的版本。标准 OSS 的 STS SecurityToken 需与临时 AccessKey 配合签名，可以由凭证持有方提供预签名 URL。

## PhotoLib 账号授权与 GitHub

成员在 Deplyt 点「使用 PhotoLib 登录」，到 PhotoLib 批准配对，再回到 Deplyt 检查结果。首次验证后，账号会显示在管理员的「访问权限」页。管理员可在该页授予或撤销权限，成员刷新即可看到结果。回滚和仓库活动仍由 Deplyt 管理员操作。

如果使用原有的 PhotoLib 管理员单点登录，请在 PhotoLib 部署配套变更并配置相同的 `DEPLYT_PUBLIC_URL` 与 `DEPLYT_SSO_SECRET`。回调路径为 `/auth/callback`。未指定 `DEPLOY_ADMIN_PHOTOLIB_ID` 时，通过该单点登录验证的 PhotoLib 管理员可以管理 Deplyt。

为 `GITHUB_TOKEN` 授予 Contents、Pull requests、Actions、Dependabot alerts、Code scanning alerts、Secret scanning alerts 的 Read 权限。GitHub 仓库需启用对应扫描功能，控制台会在相应板块显示权限或功能错误。凭据扫描页只展示告警元数据。[GitHub PAT 权限文档](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)

配置 `GITHUB_ACTIONS_TOKEN` 后，已授权成员可手动触发 PhotoLib `master` 上的 `build.yml`，并在 GitHub 查看运行进度。

配置 `GITHUB_TOKEN` 后，Deplyt 在后台同步默认分支的提交、全部 PR、工作流运行和三类安全告警。同步会读取 GitHub 提供的全部分页，也会同步提交详情、PR 统计和工作流作业。首次启动会立即同步，之后每 3 小时同步一次。管理员可以在 GitHub 数据页面点「手动同步全部」，页面会自动更新进度。手动同步启动后重新计算下次自动同步时间。

GitHub 数据保存在状态目录下的 `github/` 中，重启后可继续浏览。某一列表同步失败时保留该列表最近一次成功的数据。首次同步需要补齐历史详情，之后会复用未变动的详情，页面翻页从本地读取。

## 应急管理员登录

在中转机安装新版 Deplyt 后运行：

```sh
/opt/deplyt/deplyt generate-emergency-password
```

命令会输出随机密码和一行形如 `DEPLOY_EMERGENCY_PASSWORD_HASH='...'` 的配置。将密码保存到密码管理器，再把完整的配置行连同单引号写入中转机的 `/opt/deplyt/.env`，最后执行 `sudo systemctl restart deplyt`。应急密码只在 Deplyt 上配置。

配置后，登录页显示「使用应急密码登录」。此入口可在 PhotoLib 不可用时直接登录 Deplyt，授予 30 分钟的管理员会话，可查看部署记录、部署和回滚。操作记录使用「应急管理员」标识，登录事件也会写入 Deplyt 服务日志。更换或停用时，更新或清空哈希后重启 Deplyt。

## 连接 PhotoLib 的 GitHub Actions

在 PhotoLib 仓库配置以下 Actions 变量和 Secret，确保 `master` 上的 `build.yml` 包含中转部署任务。

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
