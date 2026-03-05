# gogoga_dictionary

[English](./README.md) | 简体中文

一个基于 ECDICT 的 Go 词典 Web 服务，适合 iOS/APP 单词查询场景。

## 功能

- `GET /v1/word/{word}`：精确查词
- `GET /v1/search?q=...&mode=prefix|fuzzy&page=1&page_size=20`：搜索（前缀/模糊）
- `GET /v1/suggest?q=...&limit=10`：联想建议
- `GET /v1/health`：健康检查

## 项目结构

- `cmd/api`：API 服务入口
- `cmd/importer`：ECDICT CSV 导入器入口
- `internal/db`：SQLite 初始化与 schema 应用
- `internal/repo`：词典数据访问层
- `internal/http`：HTTP 处理器
- `migrations/schema.sql`：数据库结构与索引
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
docker compose up -d api
```

5. 验证

```bash
curl 'http://127.0.0.1:8080/v1/health'
```

## 环境变量

- `HTTP_ADDR`（默认：`:8080`）
- `DB_PATH`（默认：`./data/dict.db`；容器内默认：`/app/data/dict.db`）
- `SCHEMA_PATH`（默认：`./migrations/schema.sql`；容器内默认：`/app/migrations/schema.sql`）

## API 示例

```bash
curl 'http://127.0.0.1:8080/v1/word/apple'
curl 'http://127.0.0.1:8080/v1/suggest?q=app&limit=5'
curl 'http://127.0.0.1:8080/v1/search?q=apple&mode=prefix&page=1&page_size=20'
curl 'http://127.0.0.1:8080/v1/search?q=network security&mode=fuzzy&page=1&page_size=20'
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
