# gogoga_dictionary

[English](./README.md) | 简体中文

一个基于 ECDICT 的 Go 词典 Web 服务，适合 iOS/APP 单词查询场景。

## 功能

- `GET /v1/word/{word}`：精确查词
- `GET /v1/search?q=...&mode=prefix|fuzzy&page=1&page_size=20`：搜索（前缀/模糊）
- `GET /v1/suggest?q=...&limit=10`：联想建议
- `GET /v1/health`：存活检查
- `GET /v1/ready`：就绪检查（1 秒超时的数据库 ping），并返回各可选功能是否启用
- `POST /v1/analytics/events`、`POST /v1/feedback`、`POST /v1/feedback/upload-image`：埋点 / 反馈
- `POST /v1/llm/ark`：火山方舟 Responses API 代理（JSON 或 SSE）
- `POST /v1/tts`：豆包语音合成（mp3）

所有 JSON 响应使用 `{data, error, code, request_id, server_time}` 结构；`code` 为可选的稳定错误码
（`invalid_request`、`invalid_body`、`body_too_large`、`not_found`、`method_not_allowed`、`rate_limited`、
`service_unavailable`、`busy`、`budget_exceeded`、`upstream_error`、`internal_error`）。未知路由返回 JSON 404，
方法不对返回 JSON 405。内部错误不会把细节返回给客户端（按 `request_id` 记录在日志中）。

## 项目结构

- `cmd/api`：API 服务入口
- `cmd/importer`：ECDICT CSV 导入器入口
- `internal/db`：SQLite 初始化与 schema 应用
- `internal/repo`：词典数据访问层
- `internal/config`：环境变量读取与校验
- `internal/httpapi`：路由、中间件（request id、访问日志、panic 恢复）与处理器
- `internal/ratelimit`、`internal/llmproxy`、`internal/tts`、`internal/upload`、`internal/feedback`、`internal/analytics`：各功能服务
- `migrations/schema.sql`（已嵌入二进制）：数据库结构与索引
- `datasets/`：词库文件目录（建议放 `ecdict.csv`）

## 数据文件放置

建议路径：

```bash
./datasets/ecdict.csv
```

仓库已配置为允许提交该文件，方便其他人开箱即用。

## 本地开发

1. 安装依赖

```bash
go mod tidy
```

2. 导入 ECDICT CSV（默认读取 `./datasets/ecdict.csv`）

```bash
make import
```

如果使用自定义路径：

```bash
make import CSV=/absolute/path/to/ecdict.csv
```

3. 启动 API 服务

```bash
make run-api
```

默认值：

- 监听地址：`:8080`
- 数据库文件：`./data/dict.db`
- Makefile 构建标签：`sqlite_fts5`（模糊搜索依赖 FTS）

其他目标：`make test`（带 -race 的测试）、`make vet`、`make build`（输出 `./bin/api`）、`make run-local`（加载 `./.env`）。

## Docker 部署

1. 准备目录和数据文件

```bash
mkdir -p datasets data
# 将 ecdict.csv 放到 ./datasets/ecdict.csv
```

2. 构建镜像

```bash
docker compose build
```

3. 导入词库（首次或词库更新后执行）

```bash
docker compose --profile tools run --rm importer
```

4. 启动 API 服务

```bash
# 容器以 uid 10001 运行，挂载的 data 目录必须对其可写
sudo chown -R 10001:10001 ./data
docker compose up -d api
```

compose 只在 `127.0.0.1:8080` 上发布端口，需由反向代理（Caddy）对外提供服务。镜像自带 `HEALTHCHECK`（`api -healthcheck`，本地请求 `/v1/health`）。

5. 验证

```bash
curl 'http://127.0.0.1:8080/v1/health'
```

## 环境变量

启动时会校验配置：数字格式错误、CIDR 非法、某个功能只配置了一部分（例如只设置了部分 `QINIU_*`）都会直接退出。日志中不会输出任何密钥。

基础：

- `HTTP_ADDR`（默认：`:8080`）
- `DB_PATH`（默认：`./data/dict.db`；容器内默认：`/app/data/dict.db`）
- `SCHEMA_PATH`（可选；schema 已嵌入二进制，仅在需要覆盖时设置）
- `TRUSTED_PROXIES`：逗号分隔的 CIDR/IP，只有直连对端在此列表中时才信任 `X-Forwarded-For`（默认：回环、RFC1918、`fc00::/7`）。
  受信任时，从右向左取第一个不受信任的地址作为客户端 IP；否则使用直连对端地址。限流按此 IP 计算。

按客户端 IP 的限流（每分钟请求数，`0` 表示关闭；超限返回 `429` 和 `Retry-After`）：

- `LLM_RATE_PER_MINUTE`（默认 `60`）、`TTS_RATE_PER_MINUTE`（`120`）、`ANALYTICS_RATE_PER_MINUTE`（`60`）、
  `FEEDBACK_RATE_PER_MINUTE`（`10`）、`UPLOAD_RATE_PER_MINUTE`（`5`）、`SEARCH_RATE_PER_MINUTE`（`120`，`/v1/search` 与 `/v1/suggest` 共用）

LLM 代理（`POST /v1/llm/ark`）：

- `LLM_ARK_API_KEY`：配置后启用代理，密钥只在服务端注入。
- `LLM_ARK_MODELS`：逗号分隔的模型白名单（默认值与 iOS 端一致），其他模型返回 `400`。
- 请求体**不再原样转发**：只保留 `model`、`input`、`stream`、`thinking`、`text`、`temperature`、`max_output_tokens`
  （`tools`、`previous_response_id` 等其他字段一律丢弃），`input` 必填，`max_output_tokens` 上限 4500（缺省为 4500），
  并强制发送 `"store": false`，避免方舟保存响应。JSON/SSE 响应仍原样流式返回。
- `LLM_MAX_BODY_MB`（默认 `6`）、`LLM_TIMEOUT_SECONDS`（默认 `180`）
- `LLM_MAX_CONCURRENT`（默认 `32`）：全局并发上限，满时返回 `503` + `Retry-After`。
- `LLM_DAILY_REQUEST_BUDGET`（默认 `0` = 不限）：每个 UTC 日的全局请求预算，用尽返回 `503` + `Retry-After`（到 UTC 零点的秒数）。内存计数，重启清零。

语音合成（`POST /v1/tts`）：

- `VOLC_SPEECH_API_KEY`：配置后启用（豆包语音合成 `seed-tts-2.0`）。请求体 `{"text","speaker","speech_rate"}`，返回 `audio/mpeg`。
- `VOLC_TTS_SPEAKERS`（音色白名单，默认值与 iOS 端一致）、`VOLC_TTS_RESOURCE_ID`（默认 `seed-tts-2.0`）
- `TTS_CACHE_DIR`（默认 `<DB_PATH 所在目录>/tts-cache`）：100 字以内的文本会缓存，更长的文本不落盘。
- `TTS_CACHE_MAX_MB`（默认 `2048`，`0` 表示不限）：每小时清理一次，超出上限时删除最久未使用的文件。
- 上游调用与客户端连接解绑（30 秒超时），客户端断开不会浪费已付费的合成；相同的并发请求只调用一次上游。

反馈 / 上传（同组变量要么全配，要么全不配）：

- `FEISHU_APP_ID`、`FEISHU_APP_SECRET`、`FEISHU_BITABLE_APP_TOKEN`、`FEISHU_BITABLE_TABLE_ID`：启用 `POST /v1/feedback`。
  按 `client_feedback_id` 去重（重复提交直接返回成功，不会在飞书重复建记录）。字段长度上限（字符）：content 4000、category 64、
  user_id 128、device 128、ios_version/app_version/locale 32、client_feedback_id 128、screenshot_url 512。
  `screenshot_url` 必须以 `QINIU_PUBLIC_BASE_URL + "/feedback/"` 开头（未配置七牛时必须是 `https://`）。
- `QINIU_ACCESS_KEY`、`QINIU_SECRET_KEY`、`QINIU_BUCKET`、`QINIU_UPLOAD_URL`、`QINIU_PUBLIC_BASE_URL`：启用
  `POST /v1/feedback/upload-image`（最大 10 MB）。图片类型由文件头识别（JPEG、PNG、WebP、HEIC/HEIF，其他返回 `400`），
  忽略客户端的 `Content-Type`。`key_prefix` 必须匹配 `^feedback/[a-z0-9_-]{1,32}$`，否则使用 `feedback/other`。
  上传凭证有效期 300 秒，并带有 `fsizeLimit`/`mimeLimit`/`detectMime`。

埋点：

- `ANALYTICS_RETENTION_DAYS`（默认 `395`，`0` 表示关闭）：每日任务删除更早的事件。
- 批次中不合法的事件（缺字段、字段过长、`event_time_ms` 不在 `[now-30d, now+1d]` 内）会被跳过，响应为 `{"accepted": N, "rejected": M}`。

请求体大小限制：埋点 256 KB、反馈 32 KB、TTS 16 KB、LLM 为 `LLM_MAX_BODY_MB`；超限返回 `413`。

## API 示例

```bash
curl 'http://127.0.0.1:8080/v1/word/apple'
curl 'http://127.0.0.1:8080/v1/suggest?q=app&limit=5'
curl 'http://127.0.0.1:8080/v1/search?q=apple&mode=prefix&page=1&page_size=20'
curl 'http://127.0.0.1:8080/v1/search?q=network security&mode=fuzzy&page=1&page_size=20'
curl -N -X POST 'http://127.0.0.1:8080/v1/llm/ark' -H 'Content-Type: application/json' \
  -d '{"model":"doubao-seed-2-1-lite-260915","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}'
curl -X POST 'http://127.0.0.1:8080/v1/tts' -H 'Content-Type: application/json' \
  -d '{"text":"serendipity","speaker":"en_female_dacey_uranus_bigtts","speech_rate":0}' -o out.mp3
```

## ECDICT CSV 字段要求

导入器按列名读取（大小写不敏感），建议包含：

- `word`
- `phonetic`
- `definition`
- `translation`
- `pos`
- `collins`
- `oxford`
- `tag`
- `bnc`
- `frq`
- `exchange`
- `detail`
- `audio`

`word` 为空的行会被跳过；缺失字段会按空值处理。
