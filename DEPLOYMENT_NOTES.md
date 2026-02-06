# Deployment Notes: ECDICT Go Service + Caddy Proxy

Date: 2026-02-06

Goal:
- Run the ECDICT Go dictionary service
- Expose via HTTPS at `ecdict.gogoga.top`

---

## 1. Install Go

```bash
sudo apt-get update && sudo apt-get install -y golang-go
```

Verify:

```bash
go version
```

---

## 2. Import Dataset

Project: `/root/project/ecdict-api`  
Dataset: `/root/project/ecdict-api/datasets/ecdict.csv`

The environment restricts write access under `/root`, so use writable caches:

```bash
mkdir -p /tmp/go-build /tmp/gopath
GOCACHE=/tmp/go-build GOPATH=/tmp/gopath make import
```

---

## 3. Run the API

```bash
mkdir -p /tmp/go-build /tmp/gopath
GOCACHE=/tmp/go-build GOPATH=/tmp/gopath make run-api
```

Service listens on `127.0.0.1:8080`.

Health check:

```bash
curl http://127.0.0.1:8080/v1/health
```

---

## 4. Caddy Reverse Proxy (Flowlet Docker)

Caddy runs in Docker and must access the host Go service via
`host.docker.internal`.

### Update `docker/docker-compose.full.yml`

```yaml
caddy:
  extra_hosts:
    - "host.docker.internal:host-gateway"
```

### Update `docker/Caddyfile`

```caddyfile
ecdict.gogoga.top {
  reverse_proxy host.docker.internal:8080
}
```

### Restart Caddy

```bash
docker compose -f /root/project/Flowlet/docker/docker-compose.full.yml \
  --env-file /root/project/Flowlet/docker/.env.flowlet \
  up -d caddy
```

---

## 5. DNS

A record:

```
ecdict.gogoga.top -> 148.135.6.189
```

Verify:

```bash
dig +short ecdict.gogoga.top
```

---

## 6. HTTPS Certificate

Caddy automatically requests a certificate from Let’s Encrypt. If DNS is not
propagated, issuance will fail. After DNS resolves, restart Caddy to trigger
issuance.

---

## 7. Verify Access

```bash
curl https://ecdict.gogoga.top/v1/word/apple
```

---

## 8. Run as systemd Service (Optional)

Create service file:

`/etc/systemd/system/ecdict-api.service`

```ini
[Unit]
Description=ECDICT API
After=network.target

[Service]
Type=simple
WorkingDirectory=/root/project/ecdict-api
ExecStart=/usr/bin/make run-api
Restart=always
RestartSec=2

# Writable cache paths for restricted environments
Environment=GOCACHE=/var/cache/go-build
Environment=GOMODCACHE=/var/cache/go-mod

# Optional overrides
# Environment=HTTP_ADDR=:8080
# Environment=DB_PATH=/root/project/ecdict-api/data/dict.db

[Install]
WantedBy=multi-user.target
```

Prepare cache directories:

```bash
sudo mkdir -p /var/cache/go-build /var/cache/go-mod
sudo chmod -R 777 /var/cache/go-build /var/cache/go-mod
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable ecdict-api
sudo systemctl start ecdict-api
```

Check status and logs:

```bash
sudo systemctl status ecdict-api --no-pager
sudo journalctl -u ecdict-api -f
```

---

## Notes

- Default Go cache paths under `/root` were not writable; use `GOCACHE` and
  `GOPATH` (or `GOMODCACHE`) pointing to writable locations.
- Caddy in Docker cannot reach host services without `host.docker.internal`
  mapping.
