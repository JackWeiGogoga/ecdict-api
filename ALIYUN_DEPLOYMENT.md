# 阿里云部署说明

本文档记录当前项目部署到阿里云新服务器的步骤，不替换已有的部署说明。

适用场景：

- 服务器：阿里云 ECS
- 系统：Ubuntu 22.04
- 服务方式：Docker Compose
- 反向代理：宿主机 Caddy
- 域名：`ecdict.gogoga.top`

## 1. 安装 Docker

建议使用 Docker 官方 APT 仓库：

```bash
apt update
apt install -y ca-certificates curl gnupg
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list
apt update
apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

验证：

```bash
docker version
docker compose version
```

## 2. 拉取代码

```bash
mkdir -p /root/project
cd /root/project
git clone <仓库地址> ecdict-api
cd /root/project/ecdict-api
```

如果目录名不是 `ecdict-api`，后续命令按实际目录替换。

## 3. 构建镜像

国内服务器拉取 Go 模块和 Debian 软件包较慢，在 `.env` 中设置构建镜像源（`docker-compose.yml` 会作为 build args 传入）：

```bash
BUILD_GOPROXY=https://goproxy.cn,direct
BUILD_GOSUMDB=sum.golang.google.cn
BUILD_APT_MIRROR=mirrors.aliyun.com
```

不要直接修改服务器上的 `Dockerfile`，否则 `git pull` 会冲突。

```bash
cd /root/project/ecdict-api
docker compose build
```

## 4. 导入词库

容器（api 与 importer）以非 root 用户 uid 10001 运行。首次部署或从旧版本（root 运行）升级时，先让挂载的 `data` 目录对其可写：

```bash
mkdir -p data
sudo chown -R 10001:10001 data
```

首次部署或词库更新后执行：

```bash
docker compose --profile tools run --rm importer
```

说明：

- 正确命令是 `docker compose --profile tools run --rm importer`
- 不要写成 `docker compose run --rm --profile tools importer`

## 5. 启动 API

```bash
docker compose up -d api
```

`docker-compose.yml` 现在只把端口发布到 `127.0.0.1:8080`（不再是 `0.0.0.0:8080`），公网无法直接访问 API，必须经由 Caddy。

本机验证：

```bash
curl 'http://127.0.0.1:8080/v1/health'
curl 'http://127.0.0.1:8080/v1/word/apple'
```

## 6. 配置 Caddy

如果宿主机已经安装 Caddy，编辑：

```bash
nano /etc/caddy/Caddyfile
```

追加站点块：

```caddyfile
ecdict.gogoga.top {
    encode gzip
    reverse_proxy 127.0.0.1:8080
}
```

注意：

- 宿主机 Caddy 必须通过 `127.0.0.1:8080` 访问 API（compose 只绑定回环地址）。
- 如果 Caddy 也运行在 Docker 中，`127.0.0.1` 指向的是 Caddy 容器自身：要么让 Caddy 与 API 加入同一个 Docker 网络并
  `reverse_proxy api:8080`，要么给 Caddy 容器配置 `extra_hosts: ["host.docker.internal:host-gateway"]`，并让 API
  端口绑定在 Caddy 能访问到的宿主机地址上。
- API 只在直连对端属于 `TRUSTED_PROXIES` 时才信任 `X-Forwarded-For`（默认：回环、RFC1918、`fc00::/7`）。宿主机 Caddy
  经 Docker 端口映射访问时，容器看到的对端是 Docker 网桥网关（如 `172.17.0.1`/`172.18.0.1`），已在默认范围内；如果 Caddy
  所在网络不在默认范围，请在 `.env` 中设置 `TRUSTED_PROXIES`（逗号分隔 CIDR）。不要把公网地址段加入该列表，否则客户端可伪造 IP 绕过限流。

校验并重载：

```bash
caddy fmt --overwrite /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy
```

## 7. 阿里云安全组

入方向至少放开：

- `22/TCP`
- `80/TCP`
- `443/TCP`

`8080` 不需要对公网开放，因为只由本机 Caddy 访问。

## 8. DNS 解析

在阿里云 DNS 中新增：

- 主机记录：`ecdict`
- 记录类型：`A`
- 记录值：服务器公网 IP

验证：

```bash
dig +short ecdict.gogoga.top
```

## 9. 上线验证

```bash
curl -I http://ecdict.gogoga.top/v1/health
curl -I https://ecdict.gogoga.top/v1/health
curl 'https://ecdict.gogoga.top/v1/word/apple'
```

预期：

- HTTP 跳转到 HTTPS
- HTTPS 返回 `200`

## 10. 后续发布

### 推荐：本地编译镜像，服务器只导入（服务器内存小，编译 CGO 会拖垮线上）

在开发机（Apple Silicon 或 x86 均可，需要 Docker）上：

```bash
BUILD_APT_MIRROR=mirrors.aliyun.com BUILD_GOPROXY=https://goproxy.cn,direct ./scripts/build-image.sh
scp dist/gogoga-dictionary-<sha>.tar.gz ali:/root/
```

服务器上：

```bash
cd /root/project/ecdict-api
git pull                                   # 同步 docker-compose.yml 等配置
gunzip -c /root/gogoga-dictionary-<sha>.tar.gz | docker load
docker tag gogoga-dictionary:latest gogoga-dictionary:rollback-$(date +%Y%m%d)   # 先给当前版本留回滚
docker tag gogoga-dictionary:<sha> gogoga-dictionary:latest
docker compose up -d --no-build api
curl -s http://127.0.0.1:8080/v1/ready
```

回滚：`docker tag gogoga-dictionary:rollback-<日期> gogoga-dictionary:latest && docker compose up -d --no-build api`。

### 备选：在服务器上编译

只在内存充足、业务低峰时使用。代码更新后：

```bash
cd /root/project/ecdict-api
git pull
docker compose build
sudo chown -R 10001:10001 data   # 从 root 运行的旧版本升级时执行一次
docker compose up -d api
```

如果词库文件有变化，再执行一次：

```bash
docker compose --profile tools run --rm importer
docker compose up -d api
```

## 11. 安全相关配置

- 限流（按客户端 IP，每分钟）：`LLM_RATE_PER_MINUTE`、`TTS_RATE_PER_MINUTE`、`ANALYTICS_RATE_PER_MINUTE`、
  `FEEDBACK_RATE_PER_MINUTE`、`UPLOAD_RATE_PER_MINUTE`、`SEARCH_RATE_PER_MINUTE`，默认值见 README。
- LLM：`LLM_MAX_CONCURRENT`（全局并发，默认 32）、`LLM_DAILY_REQUEST_BUDGET`（每 UTC 日全局预算，默认不限）。
- 埋点保留：`ANALYTICS_RETENTION_DAYS`（默认 395 天）。TTS 缓存上限：`TTS_CACHE_MAX_MB`（默认 2048）。
- 就绪检查：`curl http://127.0.0.1:8080/v1/ready`。
- 配置不完整（例如只配置了部分 `QINIU_*`）时 API 会拒绝启动，查看 `docker compose logs api`。

## 12. 常用排查

查看容器状态：

```bash
docker compose ps
```

查看 API 日志：

```bash
docker compose logs -f api
```

查看 Caddy 状态：

```bash
systemctl status caddy --no-pager
journalctl -u caddy -n 100 --no-pager
```
