# HTTP API 与状态模型

所有业务 API 位于 `/api/v1`。单管理员会话由 `vault_session` HttpOnly cookie 承载；登录/初始化返回 `csrf`，所有已登录写请求必须附带 `X-CSRF-Token`。跨 Origin 写入被拒绝；前端与后端应由同一 Origin 提供。

JSON 错误：`{"error":"可读原因"}`。PikPak 错误另提供 `code`、`endpoint`、`upstream_status`、`verification_url`。不会把登录凭据返回给前端。耗时任务返回 HTTP 202 和任务记录；`GET /events` 提供 `event: change` 的 SSE 通知，前端收到后拉取当前状态，重连也重新拉取。

## 接口

| 方法与路径 | 输入 / 行为 |
| --- | --- |
| GET `/auth/status` | `{configured, authenticated, csrf?}` |
| POST `/auth/setup` | `{token, password}`，仅首次初始化 |
| POST `/auth/login` | `{password}` |
| POST `/auth/logout` | 注销当前会话 |
| GET `/summary` | 本地计数、缺失计数、任务数、活动账号、最近检查 |
| GET `/accounts` | `{accounts, active_id}`，不含凭据 |
| POST `/accounts` | `{name, username?, password?, refresh_token?, access_token?, device_id?, captcha_token?}`，创建后异步验证 |
| PATCH `/accounts/{id}` | 同上；凭据更新验证身份后提交，不能替换为另一个用户 |
| POST `/accounts/{id}/verify` | 验证账号、容量与当前账号根目录 |
| POST `/accounts/{id}/activate` | 切换活动账号，暂停旧账号任务，安排准备根目录 |
| GET `/files` | 查询参数 `parent=root`, `view`, `search`, `sort`, `direction`, `page=0`, `limit=100`（最大 500），返回 `{files,total,page,limit,breadcrumbs,transferring}`；`transfers=0` 排除尚未登记的传输项目 |
| POST `/files` | `{name,parent_id}` 新建逻辑文件夹及持久化远端任务 |
| GET `/files/{id}` | `{file,path,source}`，不包含分享提取码 |
| POST `/files/action` | `{ids,action,name?,parent_id?,favorite?,confirm?}`；action 为 rename/move/favorite/trash/restore/purge |
| PATCH `/files/{id}/position` | `{position}`，秒；同时更新最近使用时间 |
| POST `/imports/preview` | `{link,pass_code?}`，返回分享递归清单 |
| POST `/imports` | `{items:[{link,pass_code?,parent_id?,selected?:[顶层分享文件ID],preview?:[Entry]}]}`，每次最多 100 个来源；preview 仅用于显示已选分享项目，不作为恢复或核验依据 |
| PUT `/sources/{id}` | `{link,pass_code?,selected?}`，更新来源，保留原逐文件清单 |
| GET `/jobs` | 最近 500 条任务 |
| GET `/jobs/{id}` | 任务、逐文件问题、转存阶段及归属信息 |
| POST `/jobs/{id}/{action}` | pause/cancel/retry/associate；associate 输入 `{source_id?,remote_ids:[]}` |
| POST `/scans` | 创建完整检查任务 |
| POST `/recovery/preview` | `{ids:[]}`；空数组表示全部，返回容量估算、方法与冲突 |
| POST `/recovery` | `{ids:[],account_id:"预览返回的账号 ID",confirm:true}`，只对非回收站且未确认存在的条目创建任务 |
| GET/PATCH `/settings` | `{scan_minutes,proxy_default,folder_previews?,current_password?,new_password?}`；folder_previews 默认 false，省略保留原设置 |
| GET `/export` | 来源与目录 JSON 下载，不含密码、令牌、提取码 |
| POST `/backup` | 下载包含数据库与主密钥的 ZIP 快照 |
| GET `/files/{id}/media` | 新鲜的原文件/媒体选项及播放进度 |
| GET/HEAD `/files/{id}/content` | 默认重定向到 PikPak；`proxy=1` 开启中转，支持 `media`, `download=1`, `text=1` |
| GET `/files/{id}/thumbnail` | 鉴权后读取当前资源缩略图 |
| GET `/events` | SSE 更新通知，2 秒检查一次 |
| GET `/healthz` | 无需登录，数据库健康检查与版本 |

`view` 支持 files、favorites、recent、video、audio、image、documents、trash、missing、folders。文件记录使用稳定本地 ID，不接受云端路径作为服务器文件路径。

导入后前端保留当前页面。`files` 在目标目录及匹配的搜索/分类中优先列出当前账号的未完成导入，计入分页和 total。传输项目带 `transfer: {job_id,state,progress,message}`；尚未核验登记的条目使用临时显示 ID 和 `state=transferring`，不能用于文件操作。没有元信息时先显示来源名称，随后使用现有上游响应更新真实文件/文件夹名称。失败、暂停仍保留状态；取消移除占位，完成后仅保留正式文件。已经登记但任务尚未完成的节点也携带 transfer。`transferring` 为当前账号未完成导入任务数，用于 SSE 之外的本地状态刷新；显示过程不额外请求 PikPak。

开启 `folder_previews` 后，可用文件夹的列表记录带 `folder_previews: [{id,url}]`，最多三个视频。候选来自本地记录，优先直接子文件，再读取可用子目录中的视频；不遍历云端目录。回收站、缺失、未绑定文件与不可用子目录不会参与。网格按需加载缩略图，列表及关闭状态使用普通文件夹图标。缩略图请求绑定账号、远端对象及所属文件夹；优先复用已记录地址，地址缺失或过期时才重新获取文件元信息，浏览器私有缓存五分钟。不下载视频或在服务器截帧，没有上游缩略图时回退普通图标。

## 持久化与恢复

- `nodes` 保存本地期望目录、来源相对路径、大小、哈希、收藏、回收站、播放进度和版本号。
- `sources` 保存原始来源及逐文件清单，提取码单独加密。不会将临时播放地址当作恢复来源。
- `bindings` 按 `(account_id,node_id)` 保存远端 ID 和可用状态。账号身份不匹配时拒绝更新。
- `jobs` 保存工作阶段、暂存目录、上游任务 ID、已知结果、已完成条目和问题。目录操作与排队共用数据库事务。
- `events` 保存操作事件；`sessions` 保存散列会话令牌；`settings` 保存实例、管理员密码哈希和偏好。

文件状态：present、pending、missing、drift、conflict、unknown、unbound、trashed。扫描只在相关分页与缺失复核全部成功后提交缺失结论；失败时更新为 unknown。扫描不改变本地目录意图。

任务状态：queued → running → completed / partial / failed / attention；未完成上游任务进入 waiting，临时故障进入 retry，暂停/取消为 paused/cancelled。阶段保留，重启和继续先核对已提交结果。恢复任务固定绑定账号；当前账号切换后，旧账号写入受门控，已返回结果只进入原任务。

转存使用 `.vault-task-<job>-<source>` 专用暂存目录。阶段先落盘再请求；请求结果不明确时保留 dispatching，核对暂存目录或明确返回的结果 ID。不能证明关联时等待人工指定结果，不将同名无关文件当作转存结果。

文件夹创建使用 `.vault-folder-<nodeID>` 临时名称，提交绑定后恢复用户名称，从而能核对创建成功但响应丢失的情况。恢复前后均核对父目录、名称、类型、大小及双方可用的哈希，同名冲突不覆盖。

## 媒体中转

代理只接受已登录用户的本地资源 ID，HTTP 客户端不接收任意目标 URL。上游及重定向只允许 HTTPS、公网地址，DNS 解析后的实际连接目标也检查私有/回环/链路地址。HLS 清单中的分片、密钥及子清单转换为短时加密绑定的本站请求；绑定资源、账号和过期时间，切号后不能继续使用旧绑定。服务器只流式传输文件内容，文本预览限 1 MiB，清单限 2 MiB，缩略图限 5 MiB。


### 云端保存路径

`GET /settings` 返回 `root_path`（默认 `My Pack/PikPakVault`）、`root_path_applied`（当前账号已应用的规则）、`active_account` 和 `root_job`（该路径最近的迁移任务）。路径规则已应用不代表已删除的目录或文件被恢复。

`PATCH /settings` 可传 `root_path`。使用 `/` 分隔，最多 16 层、1024 字节，不允许空层级、`.`、`..`。省略该字段保留当前路径。配置和当前账号迁移任务在同一事务中保存，响应 `job` 返回任务或 null。迁移沿用已绑定的目录 ID，不收编目标同名目录；非活动账号在切换时应用。根目录已删除时保存规则，后续显式恢复在新位置重建。

## 完整导入与程序更新

- `POST /data/import/preview`：请求体为原始 ZIP，返回导入编号与账号、文件、来源、任务数量。限 512 MiB。
- `POST /data/import/commit`：`{id, confirm:true, current_password}`，事务替换所有业务表，保留导入前备份；要求重新登录。
- 初始化使用 `/auth/import/preview` 与 `/auth/import/commit`，附 `X-Setup-Token`，无需当前密码，仅在未初始化时有效。
- `POST /data/resume`：用户核对后解除导入维护标记，允许后台调度；暂停任务仍需手动继续。
- `GET /updates`：当前版本、更新仓库、上次检查、可用版本、安装能力及持久化更新状态。
- `POST /updates/check`：检查 GitHub 最新正式 Release；网络错误保留原因。
- `POST /updates/install`：`{tag, confirm:true}`，仅 systemd 安装可用，返回 202；后续轮询状态。
- 更新阶段：queued → downloading → stopping → installing → verifying → completed；失败为 failed / rolled_back，恢复失败保持 rolling_back 以便独立服务重试。
- `/healthz` 返回版本、PID、就绪状态和更新实例标记；更新检查期间 `ready:false`，用户写入与后台调度暂停。
## TelDrive 同步接口

以下接口均需管理员会话；写请求需 `X-CSRF-Token`。认证信息仅写入，不返回。

| 方法 | `/api/v1` 路径 | 用途 |
| --- | --- | --- |
| GET | `/teldrive` | 监控列表、目标路径、最近扫描任务、当前账号 |
| POST | `/teldrive/browse` | 使用 `base_url`、`token`、`folder_id`、`page` 浏览来源；已有监控可传 `id` 并留空 token |
| POST | `/teldrive` | 新建；字段 `name`、`base_url`、`token`、`folder_id`、`folder_path`、`parent_id`、`auto_minutes` |
| PATCH | `/teldrive/{id}` | 修改名称、认证与周期；来源和目标必须与原规则一致 |
| POST | `/teldrive/{id}/sync` | 扫描并排队上传，202 返回绑定当前账号的任务；重复提交返回已有活跃扫描 |

`auto_minutes=0` 仅手动，开启范围 5–10080。来源 `folder_id=""` 表示 TelDrive 根目录，目标 `parent_id="root"` 表示本资源库根目录。任务类型为 `teldrive_scan` 和 `teldrive_upload`，控制与进度复用现有 jobs、SSE 和文件传输状态接口。
