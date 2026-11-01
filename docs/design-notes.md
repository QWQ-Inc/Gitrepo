# 实现说明：与原设计文档的差异

原设计文档（2026-10-31，"远端仓库管理器设计文档"）把"主源放在哪里"列为待定。M1 定为 **Hub 自己接收 push，再单向同步到所有镜像**。下面记录这个决定带来的改动，以及实现中发现的问题。

## 架构决定

| 原设计 | 实现 | 原因 |
| --- | --- | --- |
| 主源可以在外部（GitHub），Hub 拉取后再分发 | Hub 是唯一权威源，客户端直接 push 到 Hub | 对应"填凭据 → 拿到一个 git 地址"的用法；Hub 永远最新，M1 不需要 fetch 主源，也不需要入站 webhook |
| 凭据挂在每个远端上（`remotes.credential_id`） | 凭据挂在 **target**（平台账号）上，**binding** 把组织映射到 target | 凭据只填一次，新仓库自动获得镜像 |
| 用 `gr repo add` 登记 | 也可以第一次 push 时自动建仓（需要 token 有 `create` scope） | 拿到地址直接 push |
| provider 都假设平台有 API | 新增 `generic` provider，通过能力位（`CreateRepo`、`BranchProtection`、`DefaultBranch`）声明支持什么 | 自建 Git 往往只有 URL 和凭据 |
| `consumers` 只用于读 | token 分 `read`、`write`、`create`、`admin` 四级，可限定仓库 | Hub 要接收写入 |
| API 用 `{id}` | API 用 `{org}/{name}` | 更符合人的使用习惯 |
| M1 不含 smart HTTP | smart HTTP 进入 M1 | 接收 push 依赖它 |

## 同步规则的补充

原设计只把 lease 期望值设为 `pushed_state`。实现中补充了两条不会丢数据的规则：

- **镜像值是 Hub 值的祖先时允许快进**，即使 Gitrepo 从没推过这个 ref。这样可以直接接管已有内容的镜像，`pushed_state` 丢失后也能恢复，而且"把镜像的提交合并回来再 push"会自动消除分叉。
- **镜像上被删除的 ref 直接重建**，因为重建不会丢失任何东西。

镜像上 Gitrepo 没写过的 ref（比如有人直接在 GitHub 上建的分支）会报告为分叉，不会被删除。每个推送操作都把刚观测到的远端值作为 lease，所以观测和推送之间发生的变化总会被远端拒绝。

`--atomic` 失败时，引擎把有独立失败原因的 ref（lease 过期、平台拒绝）剔除，其余 ref 重推。远端不支持 atomic 时自动降级为非原子推送，并在该远端上记住这一点。

标签更新始终按强推处理：git 要求更新已有标签必须强推，即使是快进。

## push 的反馈

- **pre-receive**（在 receive-pack 的隔离区里运行）：只允许 `refs/heads/*` 和 `refs/tags/*`；新 blob 不能超过最严镜像的大小上限。能在 Hub 拒绝的，就不放进来。
- **post-receive**：通过 unix socket 通知服务端。服务端先备份被强推覆盖或删除的 ref，再修正 HEAD，然后排队同步，并把每个镜像的结果以 `remote:` 行流式回显，最多等待 `post_receive_wait`。`git push -o gitrepo.wait=…` 可以调整等待时间。
- 不在 pre-receive 里"先推镜像再决定收不收"：多个远端之间做不到原子，而且会让 Hub 能否写入取决于 GitHub 是否可用。

## 实现中发现的问题

- **`net/http/cgi` 不能用来跑 `git http-backend`**。它直接拒绝 chunked 请求体，而 git push 超过 `http.postBuffer`（1 MiB）时就会用 chunked。另外，http-backend 在读完请求体之前就会输出响应头；Go 的 HTTP/1 服务器一旦开始写响应，就可能丢弃未读的请求体。因此实现了一个小型 CGI 运行器（`gitserve.RunBackend`），开启 `EnableFullDuplex`，并流式转发请求和响应。端到端测试用 3 MiB 的 push 覆盖了这条路径。
- **服务端 git 配置通过 `GIT_CONFIG_COUNT` 注入**（因此要求 git ≥ 2.31），所有仓库共享同一套设置：`core.hooksPath`、`transfer.hideRefs=refs/backup`（备份 ref 不对客户端广告，也禁止客户端推送）、`receive.advertisePushOptions`、`receive.fsckObjects`。
- **对 git 的每次调用都固定 `-c` 覆盖项**：`credential.helper=`、`push.negotiate=false`、`push.followTags=false` 等，避免宿主机的 git 配置改变推送语义或泄露凭据。HOME 指向数据目录内的空目录。
- **凭据注入**：HTTPS 走 `GIT_ASKPASS`。askpass 是一个 shim 脚本，用一次性 ticket 通过 unix socket 向服务端换取凭据，凭据只在内存中存在于这次推送期间。SSH 用 0600 权限的临时密钥文件，用完即删；配置了 known_hosts 时严格校验主机密钥，否则使用 `accept-new`。
- **新的 bare 仓库，HEAD 可能指向一个永远不会被推送的分支**。post-receive 会把 HEAD 修正到实际存在的分支（优先 main / master），并同步到 `repos.default_branch`。GitHub 和 GitLab 上新建的镜像在第一次推送后也会设置默认分支。

## 依赖

GitHub 和 GitLab 的 API 调用直接用 `net/http` 实现（每个平台只用到四五个接口），没有引入 go-github 和 GitLab 客户端。指标用 Prometheus 文本格式手写输出，没有引入 client_golang。最终依赖只有 `modernc.org/sqlite`、`gopkg.in/yaml.v3` 和 `spf13/cobra`。

## M1 有意不做的事

- 多实例：Hub 按单实例运行。SQLite 中的 lease 只防止两个进程误用同一个数据目录。
- Git LFS：已在 M1 之后实现，见下文。
- 读取路由、熔断、镜像加速：Git 入口和 Hub 在同一个进程里，在有独立部署的边缘节点之前，路由带不来可用性收益。

## M1 之后补充的功能

- **Gitea / Forgejo 适配器**（`--provider gitea`）：自动建仓、设置默认分支；对需要强推或删除的分支，开启保护规则的 `enable_force_push`（Gitea ≥ 1.22 / Forgejo ≥ 7），旧版本则移除对应规则。
- **出站通知**：`push`、`sync.completed`、`remote.diverged`、`remote.failed`、`remote.recovered`、`credential.expiring`。告警只在状态变化时触发（分叉只在进入分叉时触发一次；失败只在连续失败次数恰好达到阈值时触发；恢复只针对告警过的镜像），所以每 10 分钟一次的巡检不会重复刷屏。`sync.completed` 只在确实改动了镜像时发出，载荷里带变更的 ref 和 SHA，下游可以据此按 SHA 拉取并校验，避免"镜像还没同步完就开始构建"的竞态。webhook URL 加密存储，因为聊天机器人的 URL 里就带着 access key。


## 独立评审后的修正

M1 完成后做了一次独立的代码评审，修正了以下问题，每一项都有回归测试：

- **合并请求时的覆盖范围**：只针对某个远端的 `--overwrite`，与 push 或巡检合并后会扩散到所有远端。现在"是否覆盖"按远端单独记录。
- **镜像位置冲突**：两个 Hub 仓库可以通过规则被映射到同一个镜像仓库。现在镜像位置（目标平台 + 命名空间 + 仓库名，或显式 URL）全局唯一；规则支持 `{org}`、`{name}` 名称模板；镜像仓库里已有 Gitrepo 没写过的 ref 时，必须经管理员接管（`adopt`）才会写入。
- **标签**：别人打的标签，即使能快进，也不再被移动。
- **原子推送被服务端拒绝**：receive-pack 的 `atomic push failure` 现在会被识别为连带失败。服务端 hook 整体拒绝推送时（GitLab、Gitea 的保护分支），逐个 ref 重推，一个受保护的分支不再挡住其他分支。
- **Hub 侧备份不再依赖 post-receive**：引擎在强推或删除前，会确认被替换的旧值仍被 Hub 的某个 ref 引用，否则先写入 `refs/backup/`。post-receive 里的备份改为第一步执行，并且不受客户端断开影响。
- **pre-receive 卡死**：路径超长时行读取会出错，而进程没有被清理，导致卡死。现在不限行长，出错时先结束子进程再等待。
- **推送状态**：推送成功的 ref 立即记入 `pushed_state`，之后的校验失败不会让下次同步误判为分叉。
- **其他**：
  - 传给 http-backend 的 `service` 参数与鉴权使用的保持一致。
  - lease 改为短期并自动续约，被占用时延后重试，不再丢弃请求。
  - 所有 Hub 仓库命令显式传入 `--git-dir`。
  - Hub 没有任何 ref 时，拒绝清空镜像。
  - 备份抓取后校验值是否与 lease 一致。
  - GitLab、Gitea 不再为了删除分支而删除通配保护规则。
- **记录在案但不修改**：备份对象可以按 SHA 通过协议 v2 取回。保留协议 v2 的性能收益，风险与 GitHub 的悬空提交相同，已写入 README。

## Gitee

`--provider gitee` 使用 API v5。access token 按 Gitee 的要求放在 `access_token` 参数里；provider 层的错误信息会去掉 URL 查询串，所以 token 不会出现在日志或 `last_error` 里。通过 HTTPS 推送时，Gitee 要求用户名必须是令牌所属账号的登录名，因此创建 target 时，如果没填 `--username`，会通过 `/user` 自动查出并填入（`provider.LoginUsername` 接口）。分支受保护时，强推或删除之前会先取消保护。单文件大小上限取决于套餐，不内置默认值，请用 `--max-file-size` 设置。

## Git LFS

Hub 自己实现了 LFS 服务端（`internal/lfs`）：

- 协议：batch API，basic 传输，verify，以及一个空的锁 API（`locks/verify` 返回无锁，避免客户端每次推送都告警；创建锁返回 501）。
- 鉴权：batch 请求和 git 使用同一套令牌与权限。上传、下载、verify 的 href 带有短期（1 小时）HMAC 签名令牌，绑定"仓库 + 对象 + 操作"，密钥由主密钥派生，服务重启后仍然有效。
- 存储：对象放在 bare 仓库的 `lfs/objects/<aa>/<bb>/<oid>`，与 git-lfs 自己的布局一致。上传时边写临时文件边计算 SHA-256，校验通过才 rename 到最终位置。
- 入口检查：pre-receive 读取新增的、可能是指针的小 blob（100～1024 字节），对象不在 Hub 上就拒绝推送。
- 同步：推送 ref 之前，先用 `rev-list --objects --ignore-missing --stdin` 算出"新值可达、而镜像现有值不可达"的指针，再用 `git lfs push --object-id --stdin` 上传。镜像的地址通过 `-c remote.gitrepo-mirror.url=` 临时注入，凭据沿用 askpass 或 SSH 的同一套机制，并关闭 `lfs.locksverify`。LFS 上传失败时，这一轮不推送 ref。

## SSH

内置 SSH 服务端（`golang.org/x/crypto/ssh`），默认关闭。

- **鉴权**：SSH 公钥和令牌一样带权限与仓库范围，按 SHA256 指纹查找，并逐字节比对完整公钥。
- **与 HTTPS 共用一套逻辑**：`gitserve.Server.Access`（鉴权、push 自动建仓）和 `gitserve.Server.GitEnv`（hooks、隐藏 ref、推送策略、单文件上限）由 HTTP 和 SSH 两条路径共用，两边的行为不会出现差异。服务端 git 配置必须在一次 `GIT_CONFIG_COUNT` 中给出，因为重复的环境变量只有最后一个生效。
- **会话**：只接受 `env`（仅 `GIT_PROTOCOL`，用于协议 v2）和 `exec`。执行期间继续应答其他请求；stdin 手动拷贝，避免客户端在等退出码时 `Wait()` 一直阻塞。
- **git-lfs-authenticate**：返回 HTTPS 的 LFS 地址，以及一个仓库级、15 分钟有效的签名令牌。这个令牌只能用于 batch 请求，不能用于单个对象的传输。

## 导入与恢复

`POST /api/repos/{org}/{name}/import`（`gr repo import`）让 Hub 成为某个镜像的副本，用于把已有项目迁到 Gitrepo，也用于 Hub 数据丢失后从镜像恢复。

- **加锁**：导入期间持有该仓库的同步 lease，不会和同步交错。
- **已有内容**：Hub 已有 ref 时必须带 `force`；带 `force` 时先把这些 ref 备份到 `refs/backup/`，再执行带 `--prune` 的 fetch。
- **拉取内容**：拉取所有分支和标签；如果历史里有 LFS 指针，还会执行 `git lfs fetch --all`。HEAD 跟随来源的 `ls-remote --symref HEAD`。
- **记录来源**：来源镜像标记为已接管，`pushed_state` 设为导入时的 ref，所以下一次同步时它就是"已同步"状态，其他镜像则照常推送。

## 第二次独立评审（LFS、Gitee）后的修正

- **高危：提交的 `.lfsconfig` 可以把镜像凭据引向任意主机。** git-lfs 会读取 bare 仓库 HEAD 中 `.lfsconfig` 的 `lfs.url` 和 `lfs.pushurl`，而 askpass 对任何主机都会给出凭据。修复分两层，每层单独都能挡住攻击：
  - git-lfs 改在一个临时 bare 仓库里运行（通过 alternates 借用对象，HEAD 未出生，`lfs.storage` 指向 Hub 的 LFS 目录），根本读不到仓库内容。`lfs.url` 和 `lfs.pushurl` 也被固定为镜像自己的地址。
  - askpass ticket 绑定到远端的 origin（scheme://host:port），对其他主机的凭据提示一律拒绝。这对普通 git 操作同样生效。
- **LFS 对象缺失时不再推送指针。** 每次同步都会扫描新引入的指针，对象不在 Hub 上就让该镜像的同步失败。pre-receive 改为只把分支和标签视为"已存在"（`--not --branches --tags`），所以只能从 `refs/backup/*` 到达的内容也会被检查。
- **LFS 服务端：**
  - batch 请求最多 1000 个对象，请求体最多 1 MiB。
  - 只有在已有对象大小正确时才跳过上传，否则替换；写入时先 fsync 临时文件，rename 后再 fsync 目录。
  - 传输链接使用配置的 `public_url`。
  - 令牌绑定"仓库名 + 数据库 ID"，仓库删除重建后旧令牌失效。
  - 指针解析与 git-lfs 一致：支持 CRLF 和旧版本行，键必须按序。
  - 未认证时不暴露私有仓库是否存在。
  - 锁接口接受 `git-lfs-authenticate` 签发的仓库级令牌。
- **扫描改为流式：** 指针扫描（同步端和 pre-receive）通过管道流式处理，不再把整段输出读进内存。
- **Gitee：**
  - URL 解析错误和 API 回显中的 token 会被抹掉；`base_url`、`api_url` 在保存时校验。
  - 更新凭据时没带用户名就保留原值。
  - 轮换 token 时重新查询登录名。

## 源码包与 bundle-uri

- **源码包** `/archive/{org}/{name}/{rev}.tar.gz|.zip`：
  - 鉴权与 clone 相同；`rev` 通过 `--end-of-options` 解析，杜绝参数注入。
  - 只提供能从分支或标签到达的提交（`for-each-ref --contains`），隐藏的备份内容不会外泄。
  - 按完整 SHA 请求时内容不可变，公开仓库可被 CDN 永久缓存。
- **bundle-uri**（默认关闭）：
  - 巡检时为超过 `min_size` 的仓库重建 bundle，条件是 ref 有变化且距上次超过 `interval`。只打包分支和标签，保留最新的两个 bundle。
  - 通过 upload-pack 的 `uploadpack.advertiseBundleURIs` 和 `bundle.*` 配置广播，HTTP 和 SSH 都生效。使用 `creationToken` 启发式，客户端可以据此做增量。
  - 每个 bundle 记录所属仓库的数据库 ID，只广播和提供属于当前仓库的 bundle。
  - 私有仓库的链接带 15 分钟签名令牌（与 LFS 共用签名密钥）。

## 第三次独立评审（SSH、导入、源码包、bundle）后的修正

- **隐藏内容泄露：**
  - SSH 不再提供 `git-upload-archive`，因为它能解析任何 ref，包括 `refs/backup/*`。
  - HTTP 源码包改在只含分支和标签的临时视图里生成（`gitexec.PublishedView`），备份 ref 既解析不到，也不会通过 `export-subst` 的 `%D` 泄露出名字。
- **bundle：**
  - 推送中出现覆盖或删除（即产生 Hub 备份的情况）、带 `force` 的导入，都会立即删除该仓库的 bundle。
  - 每次刷新时检查 bundle 的每个 head 是否仍被当前分支或标签包含，不满足就删除并立即重建，这样可以兜住绕过 hook 的改写。
  - bundle 的重建改到后台进行，不再拖慢巡检。
- **SSH 公钥吊销：** 每个会话都会重新查询公钥及其权限。删除公钥时，还会关闭它所有已打开的连接（包括 ControlMaster 复用的连接）。
- **导入与并发 push：**
  - 导入先把内容 fetch 到隐藏的 `refs/gitrepo/import/*`（开启 `fetch.fsckObjects`），然后下载 LFS 对象，最后用一次带期望旧值的 `update-ref` 事务发布。
  - 如果期间有 push 改动了 Hub，整个导入放弃并返回 409，push 的内容保持不变。
- **SSH 资源限制：**
  - 连接数：全局 512，单个地址 32。每个连接最多 8 个并发会话。
  - 超时：握手必须在 30 秒内完成；之后空闲 10 分钟断开；upload-pack 加 `--timeout=600`。
  - accept 出错（例如文件描述符耗尽）时退避重试，不会让 SSH 服务停掉；进程被信号杀死时，退出码报为 1。
- **源码包：**
  - `git archive` 中途失败时中断响应，不会把截断的文件当作完整、可永久缓存的内容发出去。
  - 并发构建最多 8 个，超出时返回 503 和 `Retry-After`。
- **仍然存在的限制：** 协议 v2 允许按 SHA 取得仓库里的任何对象，包括只被备份 ref 引用的对象。这一点通过"清除"解决，见下一节。

## 备份管理：列出、恢复、清除

把备份移到独立仓库可以让"按 SHA 取回"在 30 天内都不可能发生，但代价是每次强推都要复制对象并对 Hub 做一次 gc。GitHub 对悬空提交也接受同样的风险。所以实现上选择让管理员能够**在需要时彻底清除**：

- **列出**（`for-each-ref --format=… %(contents:subject)` 一次取完）：来源（Hub 或某个镜像）、原 ref、提交、创建与到期时间。
- **恢复**：用 `update-ref create` 建一个新分支（分支已存在时失败），然后排队同步，镜像也会收到。
- **清除**：
  - 持有仓库的同步 lease，删除选中的备份 ref。
  - 执行 `git gc --prune=5.minutes.ago`：比 5 分钟更早的 pack 里的不可达对象直接丢弃，较新的会被解包后保留，以免误删正在被接受的 push。
  - 加上 `lfs` 时，再删除不再被任何 ref（包括剩余的备份）引用、且早于 5 分钟的 LFS 对象。
- 清除之后，被清除的提交按 SHA 也取不回来（有端到端测试覆盖）。镜像平台上的副本不受影响。

## 审计日志

- **记录范围：** 认证通过的修改类 API 请求（GET、HEAD 以外的方法），无论成功、失败还是因权限不足被拒；以及 Hub 接受的每次 push。
  - 只读请求不记，否则日志会被控制台的轮询淹没。
  - 未认证的请求也不记：扫描器可以无限制造这类记录，而且这些请求什么都改不了。
- **记录方式：** 在 `requireAdmin` 里统一包一层，而不是逐个 handler 记录，因此新加的 API 也不会漏掉。
  - 动作取自路由模式（`r.Pattern`，例如 `DELETE /api/targets/{name}`），旁边存实际路径。用户可以按路由筛选，不需要维护一份动作名表。
  - handler 通过 `noteAudit` 补一句不含秘密的说明，例如 target 的修改只列出改了哪些字段，不记录值。
  - 请求失败时，说明为 API 返回的错误信息。错误信息只含字段名和校验结果，不会回显请求里的值。
  - 请求体从不记录。端到端测试会断言凭据、token 和密码都不出现在日志里。
- **push 记录：** 写在 post-receive 里，紧跟 Hub 备份之后。
  - `BackupHubUpdates` 现在返回被覆盖的 ref，所以日志能标出 `(forced)`。
  - 推送者、传输方式和来源地址由服务端通过环境变量传给 hook（`REMOTE_USER`、`GITREPO_TRANSPORT`、`GITREPO_CLIENT_ADDR`），客户端无法伪造。
  - 被 pre-receive 拒绝的 push 不记录：没有发生任何改动，推送者也已在终端里看到了原因。
- **来源地址：** 只有当直接对端在 `trusted_proxies` 里时，才从右往左读 `X-Forwarded-For`，取第一个不是可信代理的地址。更左边的部分可能是客户端伪造的，一律忽略。
- **名称冲突：** token 不能命名为 `admin`，否则在日志里无法和启动时配置的管理员 token 区分。SSH 公钥本来就带 `ssh:` 前缀。
- **保留期：** 默认 365 天，巡检时清理。单条说明最长 4 KiB（按字符边界截断），push 的 ref 明细最多列 50 条。
