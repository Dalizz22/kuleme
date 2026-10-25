# 库了么

单户私有实物库管理系统：**NAS 自托管**的服务端 + Web 管理后台 + Android 扫码 App。

适合「家里一台 NAS、几个人用、几千件东西」的场景：给每件实物一个编码，记位置、照片、状态和流水，能扫码、能 AI 识别入库、能表格批量导入。

- 单实例、单租户，不做多组织与计费；
- 数据全部留在自己机器上，不依赖任何云服务（AI 识别可选，接自建或自选 OpenAI 兼容服务）；
- 服务端是**单个 Go 二进制**，前端产物内嵌在里面，不需要另外部署静态站点。

## 功能

| 模块 | 说明 |
| --- | --- |
| 目录 | 库（space）、位置（树形货架）、分类、自定义字段模板 |
| 物品 | 建档、编辑、状态机（在库 / 待入库 / 预留 / 维修中 / 已售 / 已报废 / 已遗失）、列表多状态筛选 |
| 库存 | 入库、移动、报废、遗失、纠正；余额与流水可追溯 |
| 照片 | 多图关联、排序、设主图、解除；上传未关联的文件仅上传者可见 |
| 转库 | 跨库转移（带预览与版本校验）、库内移位 |
| 表格 | 导入导出，二选一：**只建档案**（catalog）或 **直接入库**（stock） |
| AI 识别 | 上传照片 → 生成候选 → 人工确认入库；可控制是否关联来源照片 |
| 扫码 | `KLM1:…` 编码解析，物品码进详情、位置码给提示 |
| 运维 | 生产初始化一次性凭据、功能开关（出售 / 维修 / 盘点 / 价格）、审计日志、消息、回收站 |

## 技术栈

| 层 | 技术 |
| --- | --- |
| 服务端 | Go 1.25+、Gin、GORM、PostgreSQL 16；`//go:embed` 内嵌前端产物 |
| Web | Vue 3 + TypeScript + Element Plus + Vite |
| Android | Vue 3 + Vant + Capacitor 8（`com.klm.app`），源码与安装包在独立仓库 [kuleme-apk](https://github.com/Dalizz22/kuleme-apk) |
| 契约 | `api/openapi.yaml`（机器契约）+ [docs/接口契约约定.md](docs/接口契约约定.md)（行为约定与错误码） |

## 目录结构

```
server/                  Go 服务端
  cmd/klm/               进程入口
  internal/              authz/auth/catalog/item/inventory/ai/media/transfer/...
  migrations/            SQL 迁移（启动自动 apply，只增不减）
  internal/web/dist/     go:embed 的前端产物（仓库里只放占位页，见下）
apps/web/                Web 管理后台（Vue 3 + Element Plus）
api/openapi.yaml         HTTP 契约
docs/                    行为契约与说明
```

## 本地运行

### 1. 数据库

任意 PostgreSQL 16 实例即可（本地容器、NAS 上的实例都行），连接串形如：

```
postgres://klm:klm@127.0.0.1:5432/klm?sslmode=disable
```

迁移在服务启动时自动执行，不需要手工灌 SQL。

### 2. 服务端

```bash
cd server
export KLM_DATABASE_URL='postgres://klm:klm@127.0.0.1:5432/klm?sslmode=disable'
export KLM_ADDR=:18081
export KLM_DATA_DIR=../data
go run ./cmd/klm
```

首次启动日志里会打印一次性初始化凭据：

```json
{"msg":"首次启动：请用一次性凭据完成初始化","setup_secret":"XXXXXXXXXXXX"}
```

浏览器打开 `http://127.0.0.1:18081/setup` 创建管理员账号（`KLM_ENV=production` 时凭据必填且只能用一次）。初始化完成后 Web 与 App 都用这个账号登录。

### 3. Web 开发模式

```bash
npm install
npm --workspace @klm/web run dev
```

Vite 默认把 `/api/v1` 代理到 `http://127.0.0.1:18081`，所以**服务端要跑在 18081**。改端口请同时改 `apps/web/vite.config.ts` 的 proxy 目标；`changeOrigin` 必须保持 `false`——服务端有 Origin/Host 校验，改成 `true` 会让写请求 403「跨站请求被拒绝」。

### 4. 打成单二进制（前端内嵌）

```bash
npm --workspace @klm/web run build
rm -rf server/internal/web/dist/* && cp -R apps/web/dist/. server/internal/web/dist/
cd server && go build -o ../bin/klm ./cmd/klm
```

`server/internal/web/dist/` 在仓库里只有一个占位页，作用是让 `go:embed all:dist` 能在干净仓库里编译通过。**没构建前端时，浏览器打开看到的会是「管理后台尚未构建」提示页，这不是 bug。**

## Android

Android 客户端是**独立仓库** [kuleme-apk](https://github.com/Dalizz22/kuleme-apk)：源码工程、构建说明、内网 HTTP 配置与成品 APK 都在那里，本仓库不包含它的源码，也不构建它。

直接装：下 `releases/kuleme-android-1.0-debug.apk`，装完进 App「设置」页填服务端地址——**只填主机根地址**（例如 `http://192.168.1.10:8080`，不要带 `/api/v1`）。首次没有地址时 App 会自己落到设置页，这是设计行为。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `KLM_ADDR` | `:8080` | 监听地址 |
| `KLM_DATABASE_URL` | 本机 postgres | 连接串 |
| `KLM_DATA_DIR` | `./data` | 实例 ID、主密钥、AI 设置、上传文件目录 |
| `KLM_PUBLIC_BASE_URL` | 空 | 对外基础 URL，用于拼下载链接 |
| `KLM_ENV` | `development` | 填 `production` 时启用生产初始化校验与更严的 CORS |
| `KLM_CORS_ORIGINS` | `capacitor://localhost,http://localhost,https://localhost` | 允许的来源，逗号分隔 |

AI 提供者配置不在环境变量里，登录后在「设置 → AI」填（OpenAI 兼容：`base_url` + `model` + 可选密钥；密钥用实例主密钥加密存盘）。

## 数据、备份与升级

- **迁移**：`server/migrations/`，启动自动 apply，只增不改；升级只换二进制。
- **实例数据**：`KLM_DATA_DIR` 下有 `instance_id`、`master.key`、`ai-settings.json`、`media/`（原图与缩略图）。
- **备份**：数据库 dump + 数据目录，两者要一起备；`master.key` 丢了，里面加密保存的 AI API Key 解不开（其余数据不受影响）。
- **升级 / 回滚**：换二进制即可，迁移不需要回退；但**别用旧二进制写新库**。

## 硬件与规模边界

- 单户 NAS / 小规模：**≤10 个用户、<1000 件物品**；
- 图片单文件 ≤10MB、JSON 请求体 ≤1MB、表格导入 ≤1000 行 / 10MB；
- 性能基线（1000 件物品 + 5000 张照片、单机）：列表 p95 ≈ 19ms、详情 ≈ 3ms。这是一台机器一次压测的结果，不代表 NAS 机械盘上的表现。

## 已知限制

- **Android 只有模拟器级验证**：无真机、无相机链路（拍照、扫码动作）实测。
- **AI 依赖外部服务**：识别质量、限流、超时取决于所接的 provider，本仓库不内置模型。
- 列表接口不做按库过滤（单户场景下成员本就可见全部库）；未知 JSON 字段静默忽略。
- 大文件上传接口不做幂等（上传成功 ≠ 已关联；未关联文件只有上传者可见，可删除清理）。

## 测试

本仓库只包含运行所需的源码，不随附测试用例、运维脚本与部署配置。

改动后建议至少自查：

```bash
cd server && go build ./... && go vet ./...
npm --workspace @klm/web run build
```

## 文档

| 想了解 | 看这份 |
| --- | --- |
| 接口行为、错误码、状态码约定 | [docs/接口契约约定.md](docs/接口契约约定.md) |
| 机器可读契约 | [api/openapi.yaml](api/openapi.yaml) |
| 预编译 APK | [kuleme-apk](https://github.com/Dalizz22/kuleme-apk)（安装包 + 校验值 + 安装说明） |
