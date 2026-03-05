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

```bash
cd /root/project/ecdict-api
docker compose build
```

## 4. 导入词库

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

代码更新后：

```bash
cd /root/project/ecdict-api
git pull
docker compose build
docker compose up -d api
```

如果词库文件有变化，再执行一次：

```bash
docker compose --profile tools run --rm importer
docker compose up -d api
```

## 11. 常用排查

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
