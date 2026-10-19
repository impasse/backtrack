# Backtrack DVR

### 把每一场直播，变成随时可回看的时间轴。

Backtrack 是一套可以自己部署的个人直播录像系统。OBS 或浏览器开始推流，录像就自动写入；直播中可以回看刚刚错过的内容，结束后可以按天查找历史、剪下片段并收藏或下载。

```text
OBS / Chrome ──推流──▶ Backtrack ──自动录制──▶ 直播时间轴
                         │                         ├─ 实时观看与时移
                         │                         ├─ 按天浏览直播历史
                         └──────▶ 收藏片段 ◀───────┘
```

Backtrack 面向个人和小型自托管场景：一台机器即可运行，媒体文件由你自己保存和管理。

## 你可以用它做什么

- **直播自动存档**：频道一旦开播，系统就持续录制，不需要提前创建录像任务。
- **直播中回看**：观看直播时可以拖回已经录下的内容，也可以随时跳回直播位置。
- **按天找历史**：在频道的直播历史中按日期浏览录像，打开某场直播即可回放。
- **精细定位长录像**：整场总览和动态 10 分钟精细时间轴联动，拖动时窗口会跟随播放位置移动；全屏控件在鼠标悬停时保持显示。
- **控制回放速度**：历史录像和时移回看支持 0.5× 到 2× 倍速；播放器诊断可查看录像时间、缓冲、掉帧和直播延迟等信息。
- **边看边剪片段**：在自定义播放器上设定起点和终点，然后导出 MP4 或收藏片段。
- **保护重要录像**：保护时间轴上的录像、收藏需要留存的片段；自动清理会遵守保护状态。
- **管理多个频道**：在频道列表进入指定频道，在设置中管理频道、存储和保留策略。
- **本地掌握数据**：媒体和索引存放在挂载的数据目录中，服务重启后会扫描并恢复已提交的录像。

## 推流方式与格式

| 来源 | 视频 | 音频 | 说明 |
| --- | --- | --- | --- |
| OBS / RTMP | H.264、HEVC（H.265）、AV1 | AAC-LC、Opus | 每个频道同时接受一路推流 |
| Chrome 浏览器直推 | H.264 | Opus | 选择共享屏幕、窗口、标签页或摄像头 |

Chrome 浏览器推流可以设置 **24 / 30 / 60 FPS** 和 **1080p / 720p / 原始分辨率上限**。实际采集帧率受浏览器、设备和共享来源影响。浏览器采集需要 HTTPS；在 `localhost` 上开发时不需要 HTTPS。

HEVC 和 AV1 的浏览器播放能力取决于浏览器、操作系统和硬件解码器。系统会保留来源编码；跨编码配置剪辑或填补录像空档时会调用 FFmpeg 重新编码。

## 快速启动

### Docker Compose（推荐）

需要已安装 Docker 和 Docker Compose。首次启动前，复制配置并修改管理员令牌：

```sh
cp configs/dvr.docker.example.yaml configs/dvr.yaml
```

编辑 `configs/dvr.yaml`，将 `server.admin_token` 改为至少 24 个字符的唯一随机密钥，然后启动：

```sh
docker compose up --build -d
```

打开 [http://localhost:8080](http://localhost:8080)，使用管理员令牌登录。令牌默认保存在当前浏览器，后续访问无需重复输入；退出登录会清除本地令牌。

停止服务：

```sh
docker compose down
```

录像、数据库和片段保存在项目目录的 `data/` 中。重建镜像不会删除该目录。

### 本地运行

本地构建需要 Go 1.27.2+、Node.js 24+、FFmpeg 和 ffprobe：

```sh
cp configs/dvr.example.yaml configs/dvr.yaml
```

将 `configs/dvr.yaml` 中的 `server.admin_token` 替换为至少 24 个字符的唯一随机密钥，再构建前端并启动服务：

```sh
cd web
npm ci
npm run build
cd ..
go run ./cmd/dvr-server -config configs/dvr.yaml
```

在 `web/` 目录中运行 `npm run format` 格式化 Svelte 和 TypeScript 源码，运行 `npm run format:check` 检查格式；运行 `npm run check` 检查前端类型和 Svelte 组件。VS Code 安装推荐的 Prettier 扩展后，保存文件时会自动格式化。

## 第一次开播

1. 登录 Backtrack，进入 **设置 → 频道管理**，创建一个频道。
2. 复制该频道的推流地址和推流码。推流码可以在管理界面中再次显示。
3. 在 OBS 中选择自定义服务：

   ```text
   服务器：rtmp://<服务器地址>:1935/live
   推流码：<频道 ID>?key=<推流码>
   ```

4. 或者在 Chrome 的频道页面点击 **推流**，选择浏览器推流并授权屏幕或摄像头采集。
5. 回到频道页面观看直播；直播结束后，在 **直播历史** 中按天打开录像。

OBS 建议使用约 2 秒的关键帧间隔，以便更快切换直播片段和定位回放。单个频道同一时间只允许一个推流者。

## 片段与存储

系统将直播写成不可变的 fragmented MP4（fMP4）媒体片段，并用 SQLite 索引时间轴。HLS 用于实时、时移和历史播放；导出任务将所选区间生成 MP4。

- **快速导出**尽量直接复制媒体流，速度快且保留来源编码。
- **精确导出**可跨越不同编码配置，统一输出为 720p、30 FPS、立体声 AAC。
- 时间轴上的录像保护和片段收藏可以避免相关媒体被常规清理移除。
- 录制媒体按最长保留时间和总容量清理；收藏片段库和临时下载文件也可以分别设置容量上限。
- 默认配置保留录像 14 天或最多 500 GiB，具体以 `configs/dvr.yaml` 为准。

## 网络端口

| 端口 | 协议 | 用途 |
| --- | --- | --- |
| `8080` | TCP | Web 界面、HTTP API、HLS 播放 |
| `1935` | TCP | OBS RTMP 推流 |
| `50000–50100` | UDP | 浏览器 WebRTC 媒体 |

在局域网或公网访问时，请配置 TLS（反向代理可使用 Caddy 等工具）。浏览器 WebRTC 推流需要把 UDP 端口范围开放到主机。当前内置 STUN 可帮助发现网络候选地址；需要 TURN 中继的网络暂不支持在 Backtrack 内配置 TURN。

## 登录与访问控制

- 管理员令牌用于登录和管理频道、推流码、保留策略及片段。
- 可选配置独立的只读令牌，供观看、浏览时间轴和下载已完成片段使用。
- 登录后使用 12 小时会话 Cookie；浏览器可记住令牌，用于会话过期后的自动恢复。
- 远程访问前请设置强随机令牌并启用 HTTPS。不要将默认示例令牌用于实际部署。

## 配置

完整配置参考：

- Docker：[`configs/dvr.docker.example.yaml`](configs/dvr.docker.example.yaml)
- 本地运行：[`configs/dvr.example.yaml`](configs/dvr.example.yaml)

常用选项包括 RTMP/HTTP 监听地址、推流重连宽限时间、片段时长、直播延迟目标、默认回看窗口、磁盘预留空间、录像保留策略和剪辑任务并发数。所有字段均可在上述示例配置中查看。

## API 概览

服务提供健康状态、频道管理、时间轴查询、HLS 播放、剪辑任务和存储管理 API。健康检查：

```text
GET /api/system/health
```

脚本访问可使用 `Authorization: Bearer <管理员令牌>`。只读令牌（若已配置）只能用于读取、播放和下载权限范围内的接口。主要端点如下：

<details>
<summary>展开查看主要 API</summary>

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/api/system/health` | 服务健康状态 |
| `GET`, `POST` | `/api/streams` | 查询或创建频道 |
| `GET` | `/api/streams/{id}/status` | 推流状态 |
| `GET` | `/api/streams/{id}/timeline?start=…&end=…` | 查询录像时间轴，可按小时或天汇总 |
| `GET` | `/api/streams/{id}/sessions` | 查询直播场次 |
| `GET` | `/api/playback/live/{id}.m3u8` | 直播播放列表 |
| `GET` | `/api/playback/dvr/{id}.m3u8?start=…&end=…` | 时移与区间回放 |
| `GET` | `/api/playback/vod/{sessionId}.m3u8` | 单场直播回放 |
| `POST` | `/api/clips` | 创建片段导出任务 |
| `GET` | `/api/clips` | 查询片段任务和收藏 |
| `GET` | `/api/clips/{id}/download` | 下载已完成的 MP4 |
| `GET`, `PUT` | `/api/retention` | 查询或更新保留策略 |
| `GET` | `/metrics` | 管理员 Prometheus 指标 |

</details>

## 技术组成

- **媒体服务**：Go，单进程服务，负责 RTMP / WebRTC 接入、分片、时间轴和 HTTP API。
- **存储**：fragmented MP4 + SQLite WAL。
- **播放**：HLS / fMP4，Web UI 使用 SvelteKit 和 hls.js。
- **片段导出**：FFmpeg / ffprobe。
- **部署**：Docker 镜像包含 Go 服务、静态 Web UI 和 FFmpeg。

## 项目设计

Backtrack 以单一媒体时间轴为中心：推流写入的片段同时服务直播、时移、历史回放和剪辑。片段提交后保持不可变；元数据由数据库索引，文件保存在本地磁盘。这样在服务器重启后，系统可以扫描和校验已经提交的媒体数据。

系统使用统一的媒体时间轴连接推流、实时播放、时移、历史回放和片段导出。每个已提交媒体片段保持不可变；SQLite 保存索引和管理信息，服务启动时可扫描磁盘并恢复有效录像。

## 许可证

本项目采用 GNU Lesser General Public License v3.0，详见 [`LICENSE`](LICENSE)。
