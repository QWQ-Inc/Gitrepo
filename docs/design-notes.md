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
- **git-lfs-authenticate**：返回 HTTPS 的 LFS 地址，以及一个仓库级、1 小时有效的签名令牌。这个令牌只能用于 batch 请求，不能用于单个对象的传输。

## 导入与恢复

`POST /api/repos/{org}/{name}/import`（`gr repo import`）让 Hub 成为某个镜像的副本，用于把已有项目迁到 Gitrepo，也用于 Hub 数据丢失后从镜像恢复。

- **加锁**：导入期间持有该仓库的同步 lease，不会和同步交错。
- **已有内容**：Hub 已有 ref 时必须带 `force`；带 `force` 时先把这些 ref 备份到 `refs/backup/`，再执行带 `--prune` 的 fetch。
- **拉取内容**：拉取所有分支和标签；如果历史里有 LFS 指针，还会执行 `git lfs fetch --all`。HEAD 跟随来源的 `ls-remote --symref HEAD`。
- **记录来源**：来源镜像标记为已接管，`pushed_state` 设为导入时的 ref，所以下一次同步时它就是"已同步"状态，其他镜像则照常推送。
