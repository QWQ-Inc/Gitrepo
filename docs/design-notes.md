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
  - 关闭这个仓库的 push 闸门，等正在进行的 push 结束（见第四次评审一节）。
  - 执行 `git gc --prune=5.minutes.ago`：比 5 分钟更早的 pack 里的不可达对象直接丢弃，较新的会被解包后保留。
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

## 实例备份与恢复

- **为什么需要：** 直接复制数据目录不可靠。数据库是 WAL 模式，单独复制 `gitrepo.db` 会丢掉还在 `-wal` 里的写入；先后复制两个文件又可能得到不一致的组合。而且有的仓库只存在于 Hub 上。
- **数据库：** 用 `VACUUM INTO` 复制。它在读事务里完成，不阻塞服务端写入，得到的是某一时刻的一致副本。快照里的仓库列表也从这个副本读取，所以凡是数据库里登记了的仓库都会被备份。
- **仓库：** 用 `git bundle create --all` 打包，不复制文件。
  - bundle 先确定 ref，再打包这些 ref 可达的对象，期间的 push 不会让它自相矛盾。清单里的 ref 数取自 bundle 本身。
  - 期间的清除如果删掉了 bundle 已列出的 ref 所需的对象，打包会失败，会自动重试。
  - 代价是要重新打包一遍，大仓库会比较耗 CPU，GitLab 的备份也是这么做的。
  - `--all` 包括 `refs/backup/*`，被覆盖的历史在恢复后还在。
- **LFS 对象：** 在 bundle 之后复制。Hub 只有在 LFS 对象落盘后才接受引用它的 push，所以 bundle 里引用到的对象一定已经存在。对象写入后不会再修改，可以直接硬链接，跨文件系统时退回复制。
- **恢复：**
  - 只往没有数据库、`repos/` 为空的数据目录里恢复。
  - 先恢复仓库：`bundle verify`，然后 `fetch '+refs/*:refs/*'`，设置原来的 HEAD，再核对 ref 数和 LFS 对象数。
  - 最后把数据库放到位：先写临时文件，打开一次让旧版本的库完成迁移，再改名。中途失败时不会留下一个"看起来能启动"的实例。
  - 快照时已被删除的仓库（数据库里有、目录里没有）会恢复成空仓库，保持数据库和磁盘一致，之后可以用 `gr repo import` 从镜像补回。
- **清单里的路径不可信：** 恢复时用 `registry.ParseSlug` 校验每个仓库名，bundle 路径必须正好是 `repos/<slug>.bundle`，防止被篡改的快照把文件写到数据目录外面。
- **秘密：** 凭据在快照里仍是密文，主密钥不在快照里。快照目录为 `0700`，文件为 `0600`。

## 第四次独立评审（备份管理、审计、实例快照）后的修正

- **清除与 push 的竞争（数据丢失）：**
  - **问题：** 清除开始时读取 ref，随后执行 gc 和 LFS 清理。在这期间，push 可以让一个旧对象重新被引用，例如把改写前的历史推到新分支。
    - git 对象在这种情况下是 git 自身已知的 gc 竞争。
    - LFS 对象的问题更直接：batch API 回答"已存在"，pre-receive 也通过了，但清理接着把它删掉，新分支从此缺少 LFS 内容。
  - **修正：** 新增按仓库的 push 闸门 `gitserve.Gate`。
    - HTTP 的 receive-pack POST 和 SSH 的 git-receive-pack 进入时登记，结束时注销。
    - 清除拿到同步 lease 后关上闸门，等正在进行的 push 结束（最多 2 分钟），再执行 gc 和 LFS 清理。
    - 新的 push 直接被拒绝，提示稍后重试。HTTP 在 info/refs 阶段就拒绝，git 能把原因显示出来。
  - 有端到端测试：清除卡在 gc 时，push 被拒绝；清除完成后，push 恢复正常。
- **lease 不续期：** 清除和导入原来一次性拿 `push_timeout + 2 分钟` 的 lease，大仓库 gc 可能比这更久。现在和同步一样，拿短 lease 并定期续期。
- **快照能成功、却恢复不了：** ref 数原来在打 bundle 之前单独读取，恢复时要求完全相等。期间一次 push 或一次 Hub 备份就会让快照永远恢复失败，而备份时完全看不出来。
  - 现在 ref 数从 bundle 本身读取（`bundle list-heads`）。
  - 打包失败（例如期间的清除删掉了对象）会重试。仓库在快照期间被注销时，标记为 missing。
  - 有测试：用 git 包装脚本在打包前插入一个 ref，快照仍然可以恢复。
- **快照里的符号链接：** 恢复 LFS 对象时，原来会照搬符号链接，LFS 下载接口就能读出数据目录外的文件（例如主密钥文件）。现在只接受普通文件，逐个复制并校验 SHA-256 与文件名一致。
- **审计里的控制字符：** 被拒绝的请求也会记录，路径可以带换行、制表符和终端转义序列，在 `gr audit` 的表格里伪造出一行"admin"操作。
  - 现在所有字段入库前转义控制字符、格式字符（如 bidi 覆盖）和非法 UTF-8，并限制长度。
  - `gr` 输出时也转义，包括 `gr backup list` 里的提交说明。
- **标签备份无法恢复：** 被移动的附注标签，备份的是标签对象，不能直接建分支，而且所有失败都被报成"分支已存在"。现在先剥到它指向的提交，并返回真实的错误。
- **其它：**
  - 审计里失败请求的错误信息改为追加到说明之后，原来只在说明为空时才写。
  - 被强推覆盖的 ref 在备份失败时也会标出 `(forced)`；这种情况下也会让 bundle-uri 的 bundle 失效。
  - 恢复后清空旧实例遗留的 lease。
  - 名为 `admin` 的 token 不区分大小写地拒绝。
  - `gr audit` 的翻页提示保留筛选条件。

## Release 同步

- **以 Hub 为准：** 和代码一样，Release 在 Gitrepo 上创建，再发布到各镜像。另一种做法是以某个平台（例如 GitHub）为源，把它的 Release 复制到其他平台，但那样 Gitrepo 就不再是唯一入口，冲突和删除也说不清以谁为准。
- **先有标签：** 只有当镜像的 `ref_state` 里这个标签和 Hub 完全一致（同一个标签对象），才会在该镜像上创建 Release。
  - 否则各平台会从默认分支自己创建一个标签，和 Hub 的不一致，之后同步时还会被当成分叉。
  - 等待中的 Release 在每次同步结束（`syncer.Options.AfterRun`）和每次巡检时重新检查。
- **不覆盖别人的东西：** 判断依据是记录下来的平台 Release ID（`release_mirrors.remote_release`）。
  - 镜像上已有同名 Release、但 ID 不是我们记录的，标为 `conflict`，原样保留，除非这个 Release 设置了 adopt。
  - 接管之后只管理自己上传的附件（`release_mirrors.assets` 记录了名字、平台 ID 和 SHA-256），别人的附件保留。只有同名附件会被替换，因为平台不允许重名。
- **增量：** 用标签、标题、说明、预发布标记和附件哈希算出一个摘要，和上次发布的一样就不调用任何 API。附件按 SHA-256 判断是否需要重新上传。中途失败时，已经完成的部分（Release ID、已上传的附件）也会保存，重试不会产生重复。
- **附件存储：** 和 LFS 对象一样，按 SHA-256 存放在 Hub 仓库目录下的 `releases/objects/`。
  - 注销仓库时一起移进 trash，`gitrepo backup` 会一起备份。
  - 恢复时逐个校验哈希。
  - 同一个仓库里内容相同的附件只存一份，最后一个引用删除时文件才删除。
- **上传：**
  - 用独立的、没有总超时的 HTTP 客户端，由 context 控制时长（单个镜像单个 Release 30 分钟）。
  - multipart 请求体流式发送，但预先算出精确的 Content-Length，因为有些反向代理不接受分块上传。
  - GitHub 的 `upload_url` 来自 API 响应，发送 token 之前要校验它的主机：必须是 API 的主机，或者 github.com 对应的 uploads.github.com。
- **授权：** 让 CI 拿 admin token 发布 Release 风险太大，所以这组接口按仓库授权，复用 git 的访问规则：修改需要 write，读取与 clone 相同。修改操作照样记入审计日志。
- **平台差异：**
  - GitLab 的 Release ID 就是标签名。附件是"项目上传文件 + Release 链接"，删除附件只删链接，GitLab API 不提供删除已上传文件的通用方法。GitLab 没有预发布标记。
  - Gitee 的 token 放在查询参数里，错误信息里出现时会被替换掉。查询不存在的 Release 时，Gitee 返回 `{}` 而不是 404。创建时必须提供 `target_commitish`、名称和说明。

## GitHub App 凭据

- **为什么：** 个人令牌绑定在一个人身上，权限通常偏大，人一离职或令牌过期，镜像就断了。GitHub App 属于组织，权限可以细到 Contents/Administration，安装令牌只有一小时有效期，API 限额也更高。
- **存储：** App 的私钥作为 target 的密文保存（和其它凭据一样用 AES-GCM 加密，AAD 为 target 名）。App ID 和安装 ID 不是秘密，存放在明文列里，界面上可以显示。
- **取令牌：** `provider.ResolveSecret` 是唯一的入口，同步、Release、导入和凭据检查都经过它。
  - 对 token 等凭据，原样返回解密后的值。
  - 对 App：用私钥签 RS256 JWT（`iat` 往前拨 1 分钟以容忍时钟偏差，有效期 9 分钟），调用 `POST /app/installations/{id}/access_tokens` 换取安装令牌。
  - 令牌按（API 地址、App ID、安装 ID、私钥哈希）缓存。剩余有效期不足 20 分钟就换新，这个阈值大于一次推送的超时，推送不会用到中途过期的令牌。
  - 换私钥后缓存自然失效。每个缓存项有自己的锁，并发同步时只换一次。
- **下游不变：** 换来的令牌和个人令牌用法一样，API 请求带 `Authorization: Bearer`，git 用 `x-access-token`，所以 provider 的其它代码不用区分两种凭据。
  - 唯一的例外是 `Check`：App 没有 `/user`，改为调用 `/installation/repositories`，报告安装 ID 和可访问的仓库数。
  - 建仓时探测"个人还是组织"会因为没有 `/user` 而失败，于是走组织接口，这也正是安装令牌唯一能建仓的方式。
- **保存时就验证：** 没有给安装 ID 时，按命名空间依次查 `/orgs/{ns}/installation` 和 `/users/{ns}/installation`，然后实际换一次令牌。私钥不对、App 没装、App ID 错了，都会在保存 target 时报错，而不是等到第一次推送才失败。
- 私钥只接受 RSA 的 PKCS#1 或 PKCS#8 PEM，也就是 GitHub 下载的格式。它从不出现在 API 输出、审计日志或错误信息里，有测试覆盖。

## 第五次独立评审（Release 同步）后的修正

- **共享附件被误删（数据丢失）：**
  - **问题：** 上传时先把文件移进按哈希存放的目录，再等这个 Release 的锁，然后写数据库。而这个锁在发布期间会被占用很久。如果期间另一个 Release 被删除，它会检查数据库，看到"没有引用"，就把文件删了，随后写入的那行记录指向一个不存在的文件。
  - **修正：** 新增按仓库的文件锁，覆盖"放入文件 + 写记录"和"检查引用 + 删除文件"两段。上传不再等 Release 锁，也就不会被正在进行的发布卡住。
  - 有测试：让发布变慢，确定性地复现了这个问题，修正后通过。
- **草稿公开：** 读取 Release 原来和 clone 用同样的权限，公开仓库的草稿说明和附件（例如尚未公开的安全修复）任何人都能看到。现在草稿只对有 write 权限的人存在。各镜像的状态和错误信息里可能含有内部主机名，也只给 write 权限的人看。
- **接管与删除：**
  - 接管别人在平台上建的 Release，会用管理员配置的凭据改动不属于 Gitrepo 的内容，所以改为需要 admin token。
  - 删除被接管的 Release 时，只删 Gitrepo 上传的附件，不删 Release 本身。接管状态记在 `release_mirrors.adopted` 中。
- **身份判断：** 是不是"我们的" Release，原来只比较平台 ID。
  - GitLab 的 Release 以标签名为 ID，别人删掉后用同一标签重建的 Release 也会被当成我们的。现在 GitLab 用"标签 + 创建时间"作为标识。
  - 删除之前也会先取回平台上当前的 Release 核对标识，已被别人替换的就不动。
- **创建结果丢失：** 平台已经建好 Release，但响应丢了（超时、网关 5xx、关机）。下次重试会看到一个"不是 Gitrepo 建的" Release，于是永远停在 `conflict`。现在发出创建请求之前先记下 `creating`，重试时据此认领。请求被平台以 4xx 明确拒绝时清除这个标记。
- **平台改名：** GitHub 会改写附件名（空格等字符），按名字匹配会导致之后的每次更新都上传失败。现在 Gitrepo 自己上传的附件按平台 ID 识别，同时记录平台给的名字。
- **部分删除后重现：** 删除时有一部分镜像失败，已经删掉的那些原来会被重新标成"待发布"，下次同步又把 Release 建回来。现在部分失败时 Release 改为未发布，已删除的镜像标为 `deleted`。
- **退避和开销：**
  - 后台发布遵守失败退避：内容没变时，在 `next_retry_at` 之前不重试；`conflict` 状态等人处理，不再反复调用平台 API。
  - 每次同步只排队确实需要处理的 Release，例如从未发布过的镜像、等标签的、到期重试的。
  - 已经全部同步的 Release 在调用 git 之前就直接返回。
  - 新镜像即使排队时满了，也会在巡检时被补上。
- **平台细节：**
  - Gitea 和 Gitee 把标签当作一个路径段，含 `/` 的标签要编码成 `%2F`。
  - Gitee 的附件列表要分页读取。
  - GitHub 创建和更新时带 `make_latest: legacy`，发布旧版本的补丁不会把它标成 Latest。
  - GitHub 和 Gitea 创建时带上 `target_commitish`，万一镜像上的标签被删了，平台会在 Hub 的提交上重建它。
- **其它：**
  - 快照期间被删除的附件，会从快照的数据库中剔除，快照不会引用自己没有的文件；恢复时也会检查每个附件的文件都在。
  - 附件链接改用完整的路径转义。
  - 上传中断留下的临时文件，超过一天后清理。

## 只读边缘节点

- **复制什么：** 每个仓库的分支和标签（`refs/backup` 和 `refs/gitrepo` 不复制）、仓库列表和可见性、token 和 SSH 公钥的哈希与权限、admin token 的哈希。
  - 边缘因此可以完全在本地鉴权，Hub 短暂不可用也不影响读取。
  - 没有复制的东西：平台凭据、镜像配置、Release、审计日志。这些都只在 Hub 上，边缘也不提供对应的 API。
- **怎么跟上：**
  - Hub 维护一个状态版本：进程随机数加一个计数器，每个仓库各有自己的版本。push（在修正 HEAD 之后）和每个成功的管理操作都会推进版本，并唤醒正在长轮询 `GET /api/edge/state` 的边缘。
  - 边缘只 fetch 版本有变化的仓库（`fetch --prune --force` 分支和标签，并设置 HEAD），并发 4 个。
  - Hub 重启后随机数变了，边缘会把所有仓库检查一遍，没有变化的 fetch 不传数据。
  - fetch 失败时，5 秒后不带等待地重新拉取状态，不会一直等到下一次变更。
- **读到自己刚写的内容：** 边缘记录正在 fetch 的仓库。请求 ref 列表（`info/refs`）时，如果该仓库正在 fetch，最多等 15 秒。这样 push 到 Hub 之后立刻从边缘 fetch，通常能看到刚推的提交。测试中，从 Hub 上 push 到边缘可读大约 0.1 秒。
- **写：**
  - receive-pack 的请求，包括 `info/refs?service=git-receive-pack`，都返回 307 到 Hub 上的同一路径，git 默认会跟随初次请求的重定向。SSH 上的 push 直接拒绝，并给出 Hub 地址。
  - LFS 的 upload batch 带着客户端自己的凭据原样转给 Hub，返回的上传链接指向 Hub。
  - download batch 先在边缘补齐缺少的对象：用 edge token 向 Hub 发 batch，按返回的链接下载（链接必须在 Hub 的主机上），校验 SHA-256 后再放入本地。然后返回指向边缘的链接。
- **token 和公钥的复制按哈希或指纹做 upsert，不是整表重建。** 没变的行 ID 不变，这样 ControlMaster 复用的 SSH 连接不会因为任意一次状态更新而被当成公钥已吊销。SSH 连接同时绑定公钥的行 ID 和指纹，见下一节。
- **edge scope：** 读取所有仓库，并且可以访问复制状态，此外什么都不能写，也不能调用管理 API。它能拿到所有 token 的哈希，这些 token 都是 24 字节随机数，哈希无法反推出原文，但仍然只应发给自己运维的节点。

## 第六次独立评审（边缘节点）后的修正

- **SSH 行 ID 复用：**
  - **问题：** SQLite 会复用被删除的最大行 ID。边缘上公钥 B 被删、公钥 C 被加上之后，C 可能拿到 B 原来的 ID，而 B 还开着的连接按 ID 查权限，于是得到了 C 的权限。
  - **修正：** 连接现在同时绑定 ID 和指纹，每条命令都核对指纹。Hub 上删除公钥时本来就会关闭连接，边缘上靠指纹核对。
- **清除之后边缘仍能按 SHA 取到：** 边缘只做 `fetch --prune`，被覆盖和清除的对象会一直留着。
  - Hub 现在记录 `repos.purged_at`，复制状态里每个仓库带上身份（ID@创建时间）和清除时间。
  - 两者任一变化，边缘就丢弃副本、重新拉取，期间这个仓库返回 503。
  - 同一名字删除后重建，也按身份变化处理，旧的私有内容不会以新的可见性被提供。
- **可见性的先后：** 变严格的可见性立即生效，变宽松的要等副本拉到新版本之后才生效。
- **复制不再被拉取阻塞：** 原来的流程是"一次状态、全部拉完、再轮询"，大仓库会让吊销等上几分钟。
  - 现在每次状态都立即应用凭据、仓库列表和可见性，拉取交给独立的工作池。
  - 每个仓库在内存里记录想要的版本、是否忙、是否需要重跑或丢弃，拉取期间到达的新变化会在拉取结束后接着处理。
  - 测试中，3 秒的拉取期间，吊销在约 10 毫秒后就在边缘生效。
- **重启和可用性：**
  - 已加载过的状态和每个仓库已拉取的版本都持久化了，重启后即使 Hub 不可达，也直接用本地副本提供服务。
  - 就绪按仓库判断：没有完整副本的仓库返回 503，其它仓库照常服务。
  - `/healthz` 在边缘尚未加载、或被 Hub 拒绝（401/403，例如 edge token 被删）时返回 503。被拒绝后边缘停止提供服务，这个状态也会持久化。
- **通过边缘推送带 LFS 的新仓库：** LFS 的 upload batch 和 locks/verify 现在在本地查找仓库之前就转给 Hub，由 Hub 授权。Hub 在 push 建仓时也会立即推进状态版本。
- **admin token：**
  - Hub 的 admin token 是人定的，不是随机生成的，未加盐的哈希可以被离线暴力破解，所以不再复制。边缘的 metrics 用边缘自己的 admin token。
  - edge token 不能限定仓库范围：创建时拒绝，复制接口也拒绝。
- **关机：** Hub 的 Shutdown 会关闭一个 stopping 通道来结束长轮询，不再卡到超时。Shutdown 本身也改为可以重复调用。
- **LFS 拉取：** batch 立即返回指向边缘的链接，后台补齐对象，同一对象只从 Hub 拉一次（最多 8 个并发）。客户端下载时如果对象还没到，就等它补齐。
- **其它：**
  - 边缘跟随 Hub 的链接时，拒绝重定向到其它主机。
  - 等待副本、判断就绪都放在鉴权之后，不能借此探测私有仓库名。
  - SSH 读取也遵守就绪判断和等待。
  - 每 6 小时全量重新拉取一次，弥补可能漏掉的变化通知。
  - 复制状态里的仓库名先经过 `ParseSlug` 规范化。
