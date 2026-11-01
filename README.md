# Gitrepo

一个 Git 地址，同时备份到 GitHub、GitLab 和自建 Git。

部署 Gitrepo，在里面填好各平台的凭据，然后把代码 `git push` 到 Gitrepo 给你的地址。Gitrepo 把代码存进自己的仓库（Hub），再用你配置的凭据把每个分支和标签推到所有镜像远端。哪个远端出问题只影响它自己；有人绕过 Gitrepo 直接改了镜像，Gitrepo 会告警，不会悄悄覆盖。

```
           git push                       force-with-lease
 开发者 ───────────────▶  Gitrepo (Hub)  ─────────────────▶  GitHub   acme/demo
 CI    ◀─────────────── git.example.com ─────────────────▶  GitLab   acme-group/demo
           git clone        │   ▲           ─────────────────▶  自建 Git backup/demo
                            ▼   │
                    pre-receive  post-receive
                    (策略检查)   (排队同步，回显结果)
```

- **一个地址**：`https://git.example.com/<org>/<repo>.git`，第一次 push 自动建仓。
- **凭据只填一次**：每个平台账号是一个 *target*；用 *binding* 声明"这个组织（或全部组织）的新仓库都备份到哪些 target"。GitHub 和 GitLab 上的镜像仓库由 Gitrepo 通过 API 自动创建。
- **push 时就看到结果**：每个镜像同步成功与否，直接以 `remote:` 行显示在 `git push` 的输出里。
- **不静默覆盖**：每次推送都带 `--force-with-lease`。Gitrepo 只覆盖自己推过的内容（或能快进的内容）；镜像上出现别人的提交时标记为"分叉"并告警，其他分支照常同步。
- **留有后悔药**：Hub 上被强推覆盖或删除的 ref、被覆盖前的镜像分叉内容，都保存在隐藏的 `refs/backup/<时间>/…` 下（默认保留 30 天），可以一键恢复成分支。不小心推上去的密钥也能彻底清除。
- **谁做了什么都有记录**：审计日志记下每一次凭据改动、token 签发、清除备份和每一次 push（含强推），包括操作者和来源地址。
- **在入口挡住注定失败的 push**：单文件大小上限取所有镜像中最严的（GitHub 是 100 MiB），超限的 push 在 Hub 就被拒绝，不会在某个镜像上一直失败。
- **出事会通知**：分叉、连续失败、恢复、凭据即将过期、同步完成（带 ref 和 SHA，可用来触发下游构建）都可以推送到带签名的 webhook，或直接推到 Slack、企业微信、飞书、钉钉。
- **Release 也只发一次**：说明和附件在 Gitrepo 上发布，自动同步到 GitHub、GitLab、Gitea/Forgejo、Gitee 的 Release，CI 用仓库级的 write token 即可。
- **常用的都支持**：HTTPS 和 SSH、Git LFS、Web 控制台、从已有镜像一键导入或恢复、整实例在线快照与恢复、源码包下载、bundle-uri 首次克隆加速。支持的平台有 GitHub、GitLab、Gitea/Forgejo、Gitee，以及任意 Git 服务器。

## 快速开始

### 1. 启动

```bash
export GITREPO_SECRET_KEY=$(gitrepo keygen)      # 加密平台凭据的主密钥，务必备份
export GITREPO_ADMIN_TOKEN=$(openssl rand -hex 24)
export GITREPO_PUBLIC_URL=https://git.example.com
gitrepo serve                                     # 默认监听 :8080，数据在 ./data
```

Docker（发布后可直接使用 `ghcr.io/qwq-inc/gitrepo:<版本>`，也可以本地构建）：

```bash
docker build -t gitrepo:local .
GITREPO_SECRET_KEY=... GITREPO_ADMIN_TOKEN=... docker compose -f deploy/docker-compose.yml up -d
```

镜像内置健康检查（`gitrepo healthcheck`）。

运行时依赖系统 `git`（≥ 2.31）；仓库用到 Git LFS 时还需要 `git-lfs`（Docker 镜像已自带）。生产环境请放在 TLS 反向代理后面。

打开 `https://git.example.com/` 进入 Web 控制台，用管理令牌登录，之后的第 2～5 步都可以在控制台里完成。下面以 CLI 为例。

### 2. 填入平台凭据（target）

```bash
export GITREPO_URL=https://git.example.com GITREPO_TOKEN=$GITREPO_ADMIN_TOKEN

# GitHub：fine-grained token，需要 Contents 写权限，自动建仓还需要 Administration 写权限
gr target add github --provider github --namespace acme --token-env GITHUB_TOKEN
# 或者用 GitHub App（推荐给组织，见下文）：不绑定个人，令牌每小时自动换新
gr target add github --provider github --namespace acme --app-id 123456 --private-key-file gitrepo.private-key.pem

# GitLab（gitlab.com 或自建）：access token，scope 为 api + write_repository
gr target add gitlab --provider gitlab --namespace acme-group --token-env GITLAB_TOKEN
gr target add corp-gitlab --provider gitlab --base-url https://gitlab.example.com --namespace infra --token-stdin < token.txt

# 自建 Gitea / Forgejo：access token，需要 repository 读写权限（在组织下建仓还需要 organization 写权限）
gr target add forgejo --provider gitea --base-url https://git.example.com --namespace backup --token-env FORGEJO_TOKEN

# Gitee：私人令牌勾选 projects 权限；git 用户名会自动从令牌查出
gr target add gitee --provider gitee --namespace acme --token-env GITEE_TOKEN   # 单文件上限随套餐不同，可加 --max-file-size

# 任意 Git 服务器（没有 API，仓库需事先存在）
gr target add backup --provider generic \
  --url-template 'ssh://git@git.example.com/{namespace}/{name}.git' --namespace mirrors \
  --ssh-key-file ~/.ssh/gitrepo_mirror --known-hosts-file ./known_hosts

gr target check github     # 用平台 API 验证凭据
```

凭据用 AES-256-GCM 加密后入库，不会出现在命令行参数、URL 或日志里。

凭据类型：访问令牌（各平台）、GitHub App（GitHub / GitHub Enterprise Server）、用户名加密码和 SSH 私钥（generic）。

#### 用 GitHub App 代替个人令牌

个人令牌属于某个人：人离职、令牌过期，镜像就断了，而且令牌的权限往往比需要的大。GitHub App 属于组织，Gitrepo 只保存 App 的私钥，每次需要时用它签一个 JWT，换取有效期一小时的安装令牌，缓存起来供 API 和 git 使用；剩余有效期不足 20 分钟时自动换新。

1. 在组织的 Settings → Developer settings → GitHub Apps 新建一个 App。
   - Webhook 不需要，可以关闭。
   - 仓库权限：Contents 读写、Metadata 只读。自动建仓还需要 Administration 读写。推送 `.github/workflows/` 下的文件需要 Workflows 读写。
2. 生成并下载私钥（`.pem`），记下 App ID。
3. 把 App 安装到组织上，选择 **All repositories**，之后新建的镜像才在它的范围内。
4. 添加 target：`gr target add github --provider github --namespace acme --app-id 123456 --private-key-file key.pem`。
   - 安装 ID 会按 `--namespace` 自动查找，也可以用 `--installation-id` 指定。
   - 保存时会实际换一次令牌，App ID、私钥或安装不对会当场报错。

限制：安装令牌不能在个人账号下建仓，所以 App 只能在组织下自动建仓；个人账号下的镜像仓库需要事先建好，并在 App 的安装范围内。

### 3. 声明备份规则（binding）

```bash
gr bind '*' github                    # 所有组织的新仓库都备份到 github
gr bind '*' gitlab
gr bind acme backup                   # 只有 acme 组织的仓库额外备份到 backup
gr bind '*' github --name-template '{org}-{name}'   # 多个组织共用一个命名空间时避免重名
```

一个镜像位置（平台 + 命名空间 + 仓库名）只能属于一个 Hub 仓库。如果规则算出的位置已被占用，就跳过这条规则，push 输出里会说明原因。

### 4. 拿到地址，push

```bash
gr token create laptop --scopes create     # 打印一次 token，作为 git 的 HTTPS 密码使用

git remote add hub https://git.example.com/acme/demo.git
git push hub main
```

```
remote: gitrepo: syncing to 3 mirror(s)…
remote: gitrepo:   [ok]       github:acme/demo        1 ref(s) updated
remote: gitrepo:   [ok]       gitlab:acme-group/demo  1 ref(s) updated
remote: gitrepo:   [ok]       backup:mirrors/demo     1 ref(s) updated
To https://git.example.com/acme/demo.git
 * [new branch]      main -> main
```

不想等就用 `git push -o gitrepo.wait=0`，同步会在后台继续。

### 5. 查看状态

```bash
gr status                 # 全部仓库
gr status acme/demo       # 单个仓库：每个镜像的状态、错误和分叉详情
gr runs acme/demo         # 同步历史
```

## 分叉怎么处理

有人绕过 Gitrepo 直接 push 到镜像（比如在 GitHub 上合并了 PR），Gitrepo 就不会覆盖那个分支：

```
remote: gitrepo:   [ok]       github:acme/demo  1 ref(s) updated
remote: gitrepo:   [diverged] gitlab:acme-group/demo  refs/heads/main
```

两种解决方式：

1. **把镜像上的提交合并回来**（推荐）：`git pull <镜像地址> main`，然后 push 到 Gitrepo。新提交是镜像内容的后代，同步时可以快进，分叉自动消除。
2. **以 Hub 为准覆盖**：`gr sync acme/demo --overwrite`。被覆盖的镜像内容先保存到 Hub 的 `refs/backup/<时间>/remotes/<id>/refs/heads/main`。

希望某个镜像永远以 Hub 为准，可以设置 `gr remote set acme/demo gitlab --on-divergence overwrite`（同样会先备份）。

同步规则逐个 ref 判断：

| 镜像上的值 | 动作 |
| --- | --- |
| 与 Hub 一致 | 无需操作 |
| 不存在 | 创建（lease 要求"不存在"） |
| 等于 Gitrepo 上次推送的值 | 更新或删除（必要时强推） |
| 是 Hub 值的祖先 | 快进（不丢任何东西；只对分支生效，别人打的标签永远不会被移动） |
| 其他情况 | 分叉：告警，或先备份再覆盖 |

每个推送操作都把刚观测到的远端值作为 lease。如果观测和推送之间远端又被改动，远端会拒绝这次推送，下一轮同步把它识别为分叉。

## 备份与清除

被覆盖的历史不会立刻消失。这包括 Hub 上的强推和删分支、带 `--force` 的导入、以及 `--overwrite` 覆盖掉的镜像提交，它们会以隐藏 ref 的形式在 Hub 上保留 `sync.backup_retention`（默认 30 天）。

```bash
gr backup list acme/demo                                       # 时间、来源、原分支、提交、到期
gr backup restore acme/demo refs/backup/<时间>/refs/heads/main  # 恢复成新分支 restore/main-<时间>，也会同步到镜像（标签的备份恢复成它指向的提交）
gr backup purge acme/demo --all --lfs --yes                     # 彻底清除
```

**推送了密钥怎么办**：先强推去掉它，再执行 `gr backup purge … --lfs`。清除会删除备份，并把只被备份引用的提交和 LFS 文件从 Hub 上彻底删掉（`git gc`），此后按 SHA 也取不回来。几点注意：
- 清除期间这个仓库的 push 会被拒绝，提示稍后重试：push 可能让一个正要被删除的旧对象重新被引用。清除会先等正在进行的 push 结束，大仓库的 gc 可能需要几分钟；
- 最近 5 分钟内写入的内容会被保留，例如为下一次 push 刚上传的 LFS 文件；
- 镜像平台上的副本要在各平台上自行处理；
- 无论如何都要轮换泄露的密钥。

控制台仓库页的"备份"一节也可以恢复和清除。

## 接管已有仓库

如果镜像仓库里已经有 Gitrepo 没写过的内容（比如以前直接在 GitHub 上维护的项目），Gitrepo 不会自动往里写，首次同步会失败并提示：

```
remote: gitrepo:   [failed]   github:acme/app  adoption required: the mirror repository already contains 3 ref(s) Gitrepo did not write …
```

确认它就是这个 Hub 仓库对应的镜像后再接管：`gr remote adopt acme/app github`（或在 `gr remote add` 时加 `--adopt`）。接管后，已有的分支只会快进或被报告为分叉，不会被覆盖。这样可以防止某条备份规则把一个新的 Hub 仓库指向别人已有的项目。

## 导入已有项目 / 从镜像恢复

已经在 GitHub、GitLab 等平台上的项目，一条命令就能让 Gitrepo 成为它的 Hub：

```bash
gr repo import acme/demo --from github        # 平台上的 acme/demo（命名空间、仓库名都可以另外指定）
```

Gitrepo 会拉取所有分支、标签以及 LFS 对象，作为 Hub 的内容（HEAD 跟随平台上的默认分支），并把这个来源记为已接管、已同步，然后同步到其他镜像。之后大家改为 push 到 Gitrepo 即可。

Hub 的数据丢失时，也用同一条命令从任意一个镜像恢复。如果 Hub 上已经有内容，必须加 `--force`；被替换的 ref 会先备份到 `refs/backup/`。Web 控制台的仓库页也提供这个功能。

## Git LFS

客户端照常使用：`git lfs track`、`git push`。Hub 本身就是 LFS 服务器（`<仓库地址>/info/lfs`），使用相同的令牌和权限。读权限可以下载，写权限可以上传，公开仓库允许匿名下载。

同步时，Gitrepo 先把本次新引用的 LFS 对象用 `git lfs push` 上传到各镜像，再推送 ref。只上传本次同步新增的对象，不会每次重传整个历史。如果某个指针对应的对象不在 Hub 上，这个镜像的同步会失败并提示原因，而不是把缺内容的指针推出去。

仓库里提交的 `.lfsconfig` 不会影响 Gitrepo 向镜像同步时使用的 LFS 地址：git-lfs 在一个看不到仓库内容的临时仓库里运行。镜像凭据也只会发给该镜像自己的地址。

- 镜像平台要开启 LFS：GitHub 默认开启，但有存储配额；GitLab 默认开启；Gitea/Forgejo 需要服务端启用 LFS。通过 SSH 访问的通用镜像，需要服务端支持 `git-lfs-authenticate`。
- 上传到 Hub 时，按该仓库所有镜像中最严的 LFS 大小上限拒绝过大的对象。GitHub 默认 2 GB（Team 和 Enterprise 套餐更高，可用 `gr target update github --max-lfs-object-size <字节>` 覆盖）；全局上限用 `policy.max_lfs_object_size` 设置。
- LFS 传输链接 15 分钟有效（git-lfs 会自动重新获取），所以撤销令牌后最多 15 分钟就会失效。
- 对象存放在 Hub 仓库目录下的 `lfs/objects/`，`gitrepo backup` 会一并备份（见[实例备份与恢复](#实例备份与恢复)）。
- 暂不支持 LFS 文件锁：`git lfs lock` 会返回明确的错误；推送时的锁检查会通过，不会产生警告。

## SSH

设置 `ssh.listen`（或环境变量 `GITREPO_SSH_LISTEN=:2222`）即可启用 SSH，之后用公钥代替令牌：

```bash
gr key add laptop --scopes create --file ~/.ssh/id_ed25519.pub
gr ssh                                  # 查看地址和主机指纹，用于核对 known_hosts
git remote add hub ssh://git@git.example.com:2222/acme/demo.git
git push hub main
```

- 公钥和令牌一样带权限（read/write/create）和仓库范围。push 自动建仓、pre/post-receive、镜像同步与 HTTPS 完全一致。
- 支持 Git 协议 v2，也支持通过 SSH 使用 git-lfs（走 `git-lfs-authenticate`，实际传输经由 HTTPS，所以 `public_url` 要配置成客户端能访问的地址）。
- 主机密钥在首次启动时生成，保存在 `<data_dir>/ssh/host_ed25519_key`，`gitrepo backup` 会一并备份，恢复后客户端不会看到主机密钥变化的警告。
- 不提供 shell：`ssh git@…` 只会回一句问候并断开。
- 只接受 ed25519、ECDSA，以及至少 2048 位的 RSA 公钥。

## 源码包

构建只需要代码、不需要历史时，可以直接下载源码包：

```bash
curl -fL -u x:$TOKEN https://git.example.com/archive/acme/demo/<sha>.tar.gz -o src.tar.gz
curl -fL -u x:$TOKEN https://git.example.com/archive/acme/demo/release/1.2.zip -o src.zip   # 分支或标签名也可以
```

- 鉴权与 clone 相同：公开仓库允许匿名下载，私有仓库需要 read 权限。
- 只提供分支和标签能到达的提交（`refs/backup/` 里的内容不会被下载）。
- 按完整 SHA 下载时内容不可变：公开仓库返回 `Cache-Control: public, immutable`，可以放在 CDN 上永久缓存；按分支或标签名下载不缓存。
- LFS 文件在源码包里是指针文件。

## Release

Release（说明和附件）在 Gitrepo 上发布一次，就会同步到这个仓库所有支持 Release 的镜像：GitHub、GitLab、Gitea/Forgejo、Gitee。

```bash
git push hub v1.2.0
gr release create acme/demo v1.2.0 --title "1.2.0" --notes-file CHANGELOG.md dist/*.tar.gz dist/SHA256SUMS
#   github:acme/demo        synced
#   gitlab:acme-group/demo  synced
#   gitee:acme/demo         waiting   ← 标签还没同步到这个镜像，同步完成后自动发布
```

- **先有标签，再有 Release**：标签同步到某个镜像（并且和 Hub 上的完全一致）之后，Release 才会在那里创建，平台不会自己从某个分支建出一个标签。在此之前状态为 `waiting`，每次同步结束后会自动重试。
- **改动会跟上**：改说明（`gr release edit`）、追加或替换附件（`gr release upload`）、删除附件（`gr release remove-file`）都会同步到各镜像。没有变化的附件不会重复上传；平台改了文件名（GitHub 会把空格等字符换掉）也认得出来。
- **删除**：`gr release rm` 删除各镜像上由 Gitrepo 创建的 Release，标签保留。镜像上的 Release 如果已被别人删掉重建，就不会再去动它。有镜像删除失败时，Hub 上的 Release 保留、改为未发布（不会在已经删掉的镜像上重新出现），重试即可。
- **不覆盖别人的 Release**：镜像上已有同名 Release、但不是 Gitrepo 创建的，显示为 `conflict`，原样保留。确认后可以用 admin token 执行 `gr release publish … --adopt` 接管：此时会更新它的标题和说明，并补上 Gitrepo 的附件，它原有的附件保留。删除被接管的 Release 时，只删除 Gitrepo 上传的附件，Release 本身留在平台上。
- **草稿**：`--draft` 只保存在 Hub 上，之后用 `gr release publish` 发布。草稿、以及各镜像的状态和错误信息，只有对仓库有 write 权限的人能看到。
- **CI 不需要 admin token**：Release 相关接口的权限按仓库计算，对仓库有 `write` 权限的 token 就能发布，例如 `gr token create ci --scopes write --repos 'acme/*'`。读取 Release 和下载附件与 clone 的权限相同。
- **附件**：附件也保存在 Hub 上（`gr release download`），单个文件默认最大 2 GiB（`releases.max_asset_size`），会随 `gitrepo backup` 一起备份。
- **各平台的差异**：GitLab 没有"预发布"标记；GitLab 的附件以项目上传文件加 Release 链接的形式出现，删除附件只删除链接。Gitee 要求 Release 有说明，没有填写时使用标题。纯 Git 服务器（generic）没有 Release，显示为 `unsupported`。
- 失败的镜像在后台按 30s、1m、2m… 退避重试，上限 1 小时；手动发布（`gr release publish`）会立即重试。已经同步到所有镜像的 Release 不会产生任何 API 调用。控制台的仓库页可以创建、发布和删除 Release。

## 首次克隆加速（bundle-uri）

大仓库的首次克隆（比如 CI 冷启动）可以先下载一个预先打好的 bundle 文件，再只拉取之后的增量：

```yaml
bundles:
  enabled: true
  min_size: 50MiB   # 小于这个大小的仓库不打 bundle
  interval: 24h     # bundle 最快多久重建一次（只在 ref 有变化时重建）
```

客户端需要开启：`git config --global transfer.bundleURI true`，然后照常 `git clone`（协议 v2，HTTPS 和 SSH 均可）。

- bundle 只包含分支和标签，不含 `refs/backup/`。仓库被注销时，它的 bundle 也会一并删除。
- 公开仓库的 bundle 链接不变、可以永久缓存，适合放在 CDN 后面。私有仓库的 bundle 链接里带有 15 分钟有效的签名令牌，git 下载时不需要额外的凭据。

## 通知

```bash
# 下游 CI：同步完成后按 SHA 触发构建，带 HMAC 签名
gr webhook add ci --url https://ci.example.com/hooks/gitrepo --events sync.completed --secret-env HOOK_SECRET

# 值班群：只推告警（URL 里带有机器人的 key，用环境变量传入，入库加密）
gr webhook add oncall --format wecom --url-env WECOM_BOT_URL --events 'remote.*,credential.expiring'

gr webhook test 2
```

| 事件 | 何时触发 |
| --- | --- |
| `push` | Hub 收到 push（各 ref 的新 SHA、推送者） |
| `sync.completed` | 一轮同步至少改动了一个镜像（变更的 ref、SHA 和每个镜像的结果）；没有变化的巡检不触发 |
| `remote.diverged` | 镜像进入分叉状态（只在状态变化时触发一次） |
| `remote.failed` | 镜像连续失败达到 `alerts.failure_threshold` 次（默认 3） |
| `remote.recovered` | 告警过的镜像恢复同步 |
| `credential.expiring` | target 凭据在 `alerts.credential_expiry_warning`（默认 14 天）内过期，每天最多一次；过期时间用 `gr target update NAME --expires 2027-01-31` 设置 |

`--format` 支持 `json`（默认，完整载荷）、`slack`、`wecom`、`feishu`、`dingtalk`。

JSON 投递带以下请求头：`X-Gitrepo-Event`、`X-Gitrepo-Delivery`；配置了 secret 时还带 `X-Gitrepo-Signature-256: sha256=<HMAC-SHA256(secret, body)>`，校验方式与 GitHub webhook 相同。失败的投递会在 5s、30s、2m 后重试。`--repos 'acme/*'` 可以只接收部分仓库的事件。飞书和钉钉机器人的"签名校验"安全设置暂不支持，请改用关键词（例如 `Gitrepo`）或 IP 白名单。

## 策略

在 Hub 的 pre-receive 阶段执行：

- 只允许推送 `refs/heads/*` 和 `refs/tags/*`（像 `refs/pull/*` 这样的 ref 推到别的平台会被拒绝）；
- 单个文件不能超过所有镜像中最严的上限：GitHub 为 100 MiB，可以用 `--max-file-size` 按 target 覆盖，也可以用 `policy.max_file_size` 设全局上限；
- 推送的 LFS 指针必须有对应的对象已上传到 Hub，否则拒绝，避免镜像上出现"有指针、没内容"的文件。

## 访问控制

| scope | 能做什么 |
| --- | --- |
| `read` | clone / fetch |
| `write` | 另外可以 push 已有仓库 |
| `create` | 另外可以通过 push 新地址来建仓 |
| `admin` | 另外可以使用管理 API 和 `gr` |
| `edge` | 给[边缘节点](#只读边缘节点)用：读取所有仓库和复制状态，不能写 |

例外：Release 接口按仓库授权，`write` 权限就能发布 Release，见 [Release](#release)。

`gr token create ci --scopes read --repos 'acme/*'` 可以把 token 限制在部分仓库。token 只存 SHA-256 哈希。`--public` 仓库允许匿名读。

名为 `admin` 的 token 不能创建：这个名字留给启动时配置的管理员 token，审计日志里才不会混淆。

注意：备份（`refs/backup/*`）不会出现在 clone、ls-remote、源码包和 bundle 里，但它们引用的对象仍在 Hub 仓库中。有读权限、又知道 SHA 的人可以通过协议 v2 按 SHA 取回这些对象，这和 GitHub 上悬空提交的情况一样。敏感内容请用 `gr backup purge` 彻底清除，见[备份与清除](#备份与清除)。

## 审计日志

Gitrepo 保管着各平台的凭据，所以"谁在什么时候改了什么"都有记录：

- **管理操作**：所有修改类 API 调用（新增、修改凭据、删除、签发 token、清除备份……），包括被拒绝的。记录内容为操作者、来源地址、路由和路径、HTTP 状态，以及一句说明，例如 `token ci (scopes read; repos acme/*)`、`changed credential`，被拒绝时是错误信息。**请求内容从不记录**，凭据、webhook URL 和 token 都不会出现在日志里。只读请求和未通过认证的请求不记录。
- **push**：Hub 接受的每次 push，包括推送者（token 名或 `ssh:公钥名`）、HTTPS 还是 SSH、来源地址，以及每个 ref 的变化。强推会标出 `(forced)`：

```
$ gr audit --repo acme/app
ID  TIME                 ACTOR  VIA   CLIENT       ACTION             STATUS  DETAIL
12  2026-11-01 12:30:02  dev    http  203.0.113.7  git push acme/app  -       main d3f63368bfa9 → b4ef06c5f7ef (forced)
11  2026-11-01 12:29:40  admin  api   203.0.113.9  POST /api/repos/acme/app/backups/purge  200  purged 1 backup(s), 0 LFS object(s)
```

`gr audit` 支持 `--actor`、`--repo`、`--action`（子串匹配，如 `push`、`DELETE`、`targets`）和 `--before`（翻页）。控制台的"审计"页面和每个仓库页面的"操作记录"显示同样的内容。记录默认保留 365 天（`audit.retention`，`0` 表示永久）。

Gitrepo 在反向代理后面时，把代理地址写进 `trusted_proxies`，来源地址才会取自 `X-Forwarded-For`；不在列表里的对端发来的这个头会被忽略，防止伪造。

## CLI 速查

```
gr target  add|update|list|rm|check      平台账号
gr bind    ORG|'*' TARGET                新仓库的默认镜像
gr binding list|rm
gr repo    add|list|rm|import            （push 新地址也会自动建仓；import 从已有镜像导入或恢复）
gr remote  add|list|adopt|pause|resume|set|rm  单个仓库的镜像
gr sync    ORG/NAME [--overwrite] [--remote ID]
gr status  [ORG/NAME]
gr runs    ORG/NAME
gr token   create|list|rm
gr key     add|list|rm                   SSH 公钥
gr ssh                                   SSH 地址与主机指纹
gr webhook add|list|test|enable|disable|rm
gr backup  list|restore|purge            被覆盖历史的隐藏备份
gr audit   [--repo] [--actor] [--action] 审计日志
gr release create|edit|upload|remove-file|publish|list|show|download|rm  发布到所有镜像的 Release
gr doctor                                部署自检：工具、public_url、磁盘、各平台凭据、镜像、边缘节点
```

所有命令都支持 `--json`。`gr` 读取 `GITREPO_URL` 和 `GITREPO_TOKEN`。

## HTTP API

`/api/*` 和 `/metrics` 都需要 admin token（`Authorization: Bearer …`），只有 `/api/repos/{org}/{name}/releases…` 例外：它按仓库授权，修改需要 write 权限，读取与 clone 相同。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET/POST | `/api/targets` | 列出 / 新增 target |
| GET/PATCH/DELETE | `/api/targets/{name}` | 查看 / 修改（含轮换凭据）/ 删除 |
| POST | `/api/targets/{name}/check` | 验证凭据 |
| GET/POST | `/api/bindings`，DELETE `/api/bindings/{id}` | 组织 → target 的默认规则 |
| GET/POST | `/api/repos` | 列出 / 登记仓库 |
| GET/DELETE | `/api/repos/{org}/{name}` | 状态详情 / 注销（数据移入 trash） |
| POST | `/api/repos/{org}/{name}/remotes` | 添加镜像 |
| PATCH/DELETE | `/api/repos/{org}/{name}/remotes/{id}` | 暂停、恢复、改分叉策略或 URL / 移除 |
| POST | `/api/repos/{org}/{name}/sync` | 立即同步，`{"wait":true,"overwrite":false}` |
| POST | `/api/repos/{org}/{name}/import` | 从已有镜像导入或恢复，`{"target":"github","force":false}` |
| GET | `/api/repos/{org}/{name}/backups` | 列出备份 |
| POST | `/api/repos/{org}/{name}/backups/restore` | 把备份恢复成新分支，`{"backup":"refs/backup/…","branch":"可选"}` |
| POST | `/api/repos/{org}/{name}/backups/purge` | 彻底清除，`{"backups":[…]}` 或 `{"all":true}`，`"lfs":true` 同时清理无引用的 LFS 对象 |
| GET | `/api/repos/{org}/{name}/runs` | 同步历史 |
| GET | `/api/doctor` | 部署自检，返回各项检查（ok / warn / fail） |
| GET | `/api/audit` | 审计日志，新的在前；`?repo=&actor=&action=&limit=&before=`，返回 `next` 用于翻页 |
| GET/POST | `/api/repos/{org}/{name}/releases` | 列出 / 新建 Release，`{"tag":"v1.0","name":"…","body":"…","prerelease":false,"publish":true,"wait":true}`（需仓库 write 权限，下同） |
| GET/PATCH/DELETE | `/api/repos/{org}/{name}/releases/{tag}` | 查看（含各镜像状态）/ 修改 / 删除（`?hub_only=true` 只删 Hub 上的）。标签含 `/` 时写成 `%2F` |
| POST | `/api/repos/{org}/{name}/releases/{tag}/publish` | 发布或重新发布，`{"wait":true,"adopt":false,"force":false}` |
| PUT/GET/DELETE | `/api/repos/{org}/{name}/releases/{tag}/assets/{file}` | 上传（请求体即文件内容）/ 下载 / 删除附件 |
| GET/POST | `/api/tokens`，DELETE `/api/tokens/{id}` | 客户端 token |
| GET/POST | `/api/webhooks`，PATCH/DELETE `/api/webhooks/{id}` | 出站通知 |
| GET/POST | `/api/ssh-keys`，DELETE `/api/ssh-keys/{id}` | SSH 公钥 |
| GET | `/api/ssh` | SSH 是否启用、地址、主机密钥与指纹 |
| POST | `/api/webhooks/{id}/test` | 发送测试投递 |
| GET | `/api/status` | 全部仓库概览 |
| GET | `/metrics` | Prometheus 指标 |
| GET | `/archive/{org}/{name}/{rev}.tar.gz`、`.zip` | 源码包（鉴权同 clone） |
| GET | `/healthz` | 健康检查 |
| GET | `/ui/` | Web 控制台（`/` 会重定向到这里） |

Git 地址：`/{org}/{name}.git`（smart HTTP，协议 v2）；启用 SSH 后还有 `ssh://git@<host>:<port>/{org}/{name}.git`。

## 只读边缘节点

Hub 在一个地区、开发者和 CI 在另一个地区（例如 Hub 在海外、用户在国内）时，可以在用户附近部署边缘节点，让 clone 和 fetch 走近路：

```bash
gr token create edge-shanghai --scopes edge          # 在 Hub 上签发
docker run -d -p 8080:8080 -v gitrepo-edge:/data \
  -e GITREPO_EDGE_UPSTREAM=https://git.example.com \
  -e GITREPO_EDGE_TOKEN=grp_… \
  -e GITREPO_SECRET_KEY="$(docker run --rm gitrepo:local keygen)" \
  -e GITREPO_PUBLIC_URL=https://git-sh.example.com \
  gitrepo:local serve
```

- **能做什么**：clone、fetch、源码包、LFS 下载都由边缘节点用本地副本处理（SSH 也可以读）。
- **鉴权**：和 Hub 一致。仓库可见性、token 和 SSH 公钥都复制到了边缘（只复制哈希）。Hub 的 admin token 不会复制。
- **Hub 不可达时**：读取照常可用，边缘重启后也能直接用本地副本提供服务。
- **写去哪里**：
  - push 会被 307 重定向到 Hub，git 会自动跟随。也可以把 push 地址直接设成 Hub：`git remote set-url --push origin https://git.example.com/acme/demo.git`。
  - LFS 上传会被转给 Hub。
  - 管理 API、控制台和 Release 都只在 Hub 上。
- **多快能看到新内容**：
  - 边缘对 Hub 做长轮询，Hub 上每次 push 或管理变更都会立即唤醒它，只 fetch 有变化的仓库。测试中，Hub 上 push 到边缘可读大约 0.1 秒。
  - 刚 push 完就从边缘 fetch，如果边缘正在拉这个仓库，会先等它拉完（最多 15 秒）。
  - token 被吊销、仓库被注销，也会在几秒内同步到边缘，即使边缘正在拉取大仓库也不受影响。
  - Hub 上清除了备份，或者删除仓库后用同一个名字重建，边缘会丢弃旧副本重新拉取，被清除或已删除的内容不会再从边缘取到，按 SHA 也不行。重新拉取完成之前，这个仓库在边缘上返回 503。
- **LFS**：边缘第一次被请求某个对象时，从 Hub 下载并按 SHA-256 校验，之后都从本地提供。
- **按区域路由**：给同一个域名配 GeoDNS，或者给不同地区各配一个域名，让读取落到最近的边缘节点，push 地址指向 Hub。
- **监控**：边缘的 `/healthz` 在尚未加载、或被 Hub 拒绝时返回 503。边缘的 `/metrics`（用边缘自己的 `GITREPO_ADMIN_TOKEN`）提供 `gitrepo_edge_ready`、`gitrepo_edge_last_update_timestamp_seconds`、`gitrepo_edge_pending_fetches`、`gitrepo_edge_upstream_ok`。
- **注意**：
  - edge token 能读所有仓库，也能拿到所有 token 的哈希，应只发给自己运维的节点。它不能限定仓库范围。
  - 在 Hub 上删除这个 token，边缘会停止服务。
  - 边缘和 Hub 之间请用 HTTPS。用明文 http 连接非本机的 Hub 时，边缘会在启动时警告。
  - 边缘自己的 `GITREPO_SECRET_KEY` 只用来签 LFS 下载链接，可以和 Hub 不同。
  - Hub 重启后，边缘会把每个仓库都检查一遍；没有变化的仓库 fetch 不会传任何数据。

## 运维

- **自检**：部署完、或者改了配置之后，运行 `gr doctor`。它让服务端从自己的角度检查一遍，有失败项时退出码为 1，可以放进监控脚本。检查内容：
  - git 和 git-lfs 是否可用；
  - `public_url` 能否访问到本机，以及是不是明文 http；
  - 数据目录的剩余空间；
  - 每个平台凭据能否通过平台 API 验证（GitHub App 会实际换一次令牌），以及是否已过期或即将过期；
  - 是否有失败或分叉的镜像、投递失败的通知；
  - 各边缘节点最近一次同步的时间。
- **审计**：见[审计日志](#审计日志)。
- **数据目录**：`repos/`（bare 仓库，含 LFS 对象）、`gitrepo.db`（SQLite，WAL 模式）、`ssh/`（SSH 主机密钥）、`bundles/`（可随时重建）、`trash/`（注销的仓库）。服务运行时不要直接复制 `gitrepo.db`，用 `gitrepo backup`，见下文。
- **实例备份**：见[实例备份与恢复](#实例备份与恢复)。即使没有快照，代码也可以用 `gr repo import` 从任一镜像恢复，但凭据、token 和只在 Hub 上的仓库会丢失。
- **重试**：失败的镜像按 30s、1m、2m… 指数退避自动重试，上限 1h；巡检每 10 分钟全量检查一次，兜底 webhook 丢失和服务重启。
- **告警**：见[通知](#通知)。
- **监控**：`gitrepo_remote_state`、`gitrepo_remote_last_success_timestamp_seconds`、`gitrepo_remote_consecutive_failures`、`gitrepo_target_credential_expiry_timestamp_seconds`。建议对"上次成功同步距今多久"和 `state="diverged"` 设告警。
- **单实例**：Hub 是唯一权威源，按单实例运行。Gitrepo 自己的部署源请直接指向 GitHub，不要经过 Gitrepo。

### 实例备份与恢复

Hub 是唯一权威源，有些仓库可能没有镜像，所以要定期给整个实例做快照：

```bash
docker compose exec gitrepo gitrepo backup /data/snapshots/$(date +%F)
```

- 服务**不用停**。数据库用 SQLite 的 `VACUUM INTO` 复制，得到一致的副本。
- 每个仓库打成一个 `git bundle --all`，包括被覆盖历史的隐藏备份。bundle 先读 ref 再打包，期间的 push 要么完整包含，要么完全不包含。
- LFS 对象（能硬链接就硬链接）和 SSH 主机密钥也在快照里。
- `manifest.json` 最后写入：有它才是完整的快照。
- 快照目录权限为 `0700`。里面的凭据仍用 `GITREPO_SECRET_KEY` 加密，**快照不含这个密钥**，请把密钥单独保存。

快照目录可以直接复制到别处，例如 `rsync` 或对象存储。恢复到一个**全新的**数据目录：

```bash
docker run --rm -v gitrepo-data-new:/data -v /backups/2026-11-01:/snapshot:ro gitrepo:local restore /snapshot
```

恢复前会先校验每个 bundle，并核对 ref 和 LFS 对象的数量，然后用**原来的** `GITREPO_SECRET_KEY` 启动服务。目标数据目录里已有数据库或仓库时，恢复会拒绝执行，不会覆盖现有实例。

配置文件示例见 [`deploy/gitrepo.example.yaml`](deploy/gitrepo.example.yaml)，实现上与原设计的差异见 [`docs/design-notes.md`](docs/design-notes.md)。

## 路线图

- **M1（已完成）**：target / binding / token，push 自动建仓，pre/post-receive，force-with-lease 同步，分叉检测与备份，巡检与重试，`gr status`。
- **M2**：~~出站 webhook 与告警~~、~~Web 控制台~~（已完成）。
- **M3**：~~源码包~~、~~bundle-uri~~、~~只读边缘节点~~、~~按区域读取路由~~（已完成，路由用 GeoDNS 或分区域域名，见[只读边缘节点](#只读边缘节点)）。
- **M4**：~~Gitea / Forgejo 适配器~~、~~Git LFS~~、~~Gitee 适配器~~、~~同步 Release~~（已完成）。

## 发布

推送 `v*` 标签（例如 `git tag v0.1.0 && git push origin v0.1.0`）会触发 `.github/workflows/release.yml`：

- 构建 Linux、macOS 的 amd64、arm64 二进制，连同 `SHA256SUMS` 发布到 GitHub Release；
- 构建多架构 Docker 镜像，推送到 `ghcr.io/qwq-inc/gitrepo:<标签>` 和 `:latest`。

## 开发

```bash
make test     # go test -race ./...（端到端测试会启动真实的 git http-backend）
make build    # bin/gitrepo, bin/gr
```

各平台在测试里用的是模拟 API。另有一组只读的契约测试，用真实平台的公开 API 核对这些模拟是否和实际一致，包括 GitLab.com、Codeberg（Forgejo）、Gitee，GitHub 可选。它们需要联网，默认跳过：

```bash
GITREPO_CONTRACT=1 go test ./internal/provider -run Contract -v
GITREPO_CONTRACT=1 GITREPO_CONTRACT_GITHUB=owner/repo GITHUB_TOKEN=… go test ./internal/provider -run Contract -v
```

许可证：Apache-2.0。
