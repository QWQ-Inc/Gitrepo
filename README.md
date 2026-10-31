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
- **留有后悔药**：Hub 上被强推覆盖或删除的 ref、被覆盖前的镜像分叉内容，都保存在隐藏的 `refs/backup/<时间>/…` 下（默认保留 30 天）。
- **在入口挡住注定失败的 push**：单文件大小上限取所有镜像中最严的（GitHub 是 100 MiB），超限的 push 在 Hub 就被拒绝，不会在某个镜像上一直失败。
- **出事会通知**：分叉、连续失败、恢复、凭据即将过期、同步完成（带 ref 和 SHA，可用来触发下游构建）都可以推送到带签名的 webhook，或直接推到 Slack、企业微信、飞书、钉钉。

> 当前处于 M1 阶段，并已包含 M2（通知、Web 控制台）以及 M4 的 Gitea/Forgejo、Gitee 适配器和 Git LFS。读取路由和镜像加速见 [路线图](#路线图)。

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
- 主机密钥在首次启动时生成，保存在 `<data_dir>/ssh/host_ed25519_key`，随数据目录一起备份。
- 不提供 shell：`ssh git@…` 只会回一句问候并断开。
- 只接受 ed25519、ECDSA，以及至少 2048 位的 RSA 公钥。

## 导入已有项目 / 从镜像恢复

已经在 GitHub、GitLab 等平台上的项目，一条命令就能让 Gitrepo 成为它的 Hub：

```bash
gr repo import acme/demo --from github        # 平台上的 acme/demo（命名空间、仓库名都可以另外指定）
```

Gitrepo 会拉取所有分支、标签以及 LFS 对象，作为 Hub 的内容（HEAD 跟随平台上的默认分支），并把这个来源记为已接管、已同步，然后同步到其他镜像。之后大家改为 push 到 Gitrepo 即可。

Hub 的数据丢失时，也用同一条命令从任意一个镜像恢复。如果 Hub 上已经有内容，必须加 `--force`；被替换的 ref 会先备份到 `refs/backup/`。Web 控制台的仓库页也提供这个功能。

## 接管已有仓库

如果镜像仓库里已经有 Gitrepo 没写过的内容（比如以前直接在 GitHub 上维护的项目），Gitrepo 不会自动往里写，首次同步会失败并提示：

```
remote: gitrepo:   [failed]   github:acme/app  adoption required: the mirror repository already contains 3 ref(s) gitrepo did not write …
```

确认它就是这个 Hub 仓库对应的镜像后再接管：`gr remote adopt acme/app github`（或在 `gr remote add` 时加 `--adopt`）。接管后，已有的分支只会快进或被报告为分叉，不会被覆盖。这样可以防止某条备份规则把一个新的 Hub 仓库指向别人已有的项目。

## Git LFS

客户端照常使用：`git lfs track`、`git push`。Hub 本身就是 LFS 服务器（`<仓库地址>/info/lfs`），使用相同的令牌和权限。读权限可以下载，写权限可以上传，公开仓库允许匿名下载。

同步时，Gitrepo 先把本次新引用的 LFS 对象用 `git lfs push` 上传到各镜像，再推送 ref。只上传本次同步新增的对象，不会每次重传整个历史。如果某个指针对应的对象不在 Hub 上，这个镜像的同步会失败并提示原因，而不是把缺内容的指针推出去。

仓库里提交的 `.lfsconfig` 不会影响 Gitrepo 向镜像同步时使用的 LFS 地址：git-lfs 在一个看不到仓库内容的临时仓库里运行。镜像凭据也只会发给该镜像自己的地址。

- 镜像平台要开启 LFS：GitHub 默认开启，但有存储配额；GitLab 默认开启；Gitea/Forgejo 需要服务端启用 LFS。通过 SSH 访问的通用镜像，需要服务端支持 `git-lfs-authenticate`。
- 单个对象大小可以用 `policy.max_lfs_object_size` 限制。对象存放在 Hub 仓库目录下的 `lfs/objects/`，备份数据目录时会一并备份。
- 暂不支持 LFS 文件锁：`git lfs lock` 会返回明确的错误；推送时的锁检查会通过，不会产生警告。

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

`gr token create ci --scopes read --repos 'acme/*'` 可以把 token 限制在部分仓库。token 只存 SHA-256 哈希。`--public` 仓库允许匿名读。

注意：`refs/backup/*` 不会出现在 clone 和 ls-remote 的结果里，但它们引用的对象仍在 Hub 仓库中。有读权限、又知道 SHA 的人可以通过协议 v2 按 SHA 取回这些对象，保留期默认 30 天。这和 GitHub 上悬空提交的情况一样，所以被强推删掉的密钥依然要轮换。

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
```

所有命令都支持 `--json`。`gr` 读取 `GITREPO_URL` 和 `GITREPO_TOKEN`。

## HTTP API

所有 `/api/*` 和 `/metrics` 都需要 admin token（`Authorization: Bearer …`）。

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
| GET | `/api/repos/{org}/{name}/runs` | 同步历史 |
| GET/POST | `/api/tokens`，DELETE `/api/tokens/{id}` | 客户端 token |
| GET/POST | `/api/webhooks`，PATCH/DELETE `/api/webhooks/{id}` | 出站通知 |
| GET/POST | `/api/ssh-keys`，DELETE `/api/ssh-keys/{id}` | SSH 公钥 |
| GET | `/api/ssh` | SSH 是否启用、地址、主机密钥与指纹 |
| POST | `/api/webhooks/{id}/test` | 发送测试投递 |
| GET | `/api/status` | 全部仓库概览 |
| GET | `/metrics` | Prometheus 指标 |
| GET | `/healthz` | 健康检查 |
| GET | `/ui/` | Web 控制台（`/` 会重定向到这里） |

Git 地址：`/{org}/{name}.git`（smart HTTP，协议 v2）；启用 SSH 后还有 `ssh://git@<host>:<port>/{org}/{name}.git`。

## 运维

- **数据目录**：`repos/`（bare 仓库）、`gitrepo.db`（SQLite）、`trash/`（注销的仓库）。备份这个目录和 `GITREPO_SECRET_KEY` 就够了。
- **重试**：失败的镜像按 30s、1m、2m… 指数退避自动重试，上限 1h；巡检每 10 分钟全量检查一次，兜底 webhook 丢失和服务重启。
- **告警**：见[通知](#通知)。
- **监控**：`gitrepo_remote_state`、`gitrepo_remote_last_success_timestamp_seconds`、`gitrepo_remote_consecutive_failures`、`gitrepo_target_credential_expiry_timestamp_seconds`。建议对"上次成功同步距今多久"和 `state="diverged"` 设告警。
- **单实例**：Hub 是唯一权威源，按单实例运行。Gitrepo 自己的部署源请直接指向 GitHub，不要经过 Gitrepo。

配置文件示例见 [`deploy/gitrepo.example.yaml`](deploy/Gitrepo.example.yaml)，实现上与原设计的差异见 [`docs/design-notes.md`](docs/design-notes.md)。

## 路线图

- **M1（已完成）**：target / binding / token，push 自动建仓，pre/post-receive，force-with-lease 同步，分叉检测与备份，巡检与重试，`gr status`。
- **M2**：~~出站 webhook 与告警~~、~~Web 控制台~~（已完成）。
- **M3**：只读边缘节点、bundle-uri / 源码包 CDN、按区域读取路由。
- **M4**：~~Gitea / Forgejo 适配器~~、~~Git LFS~~、~~Gitee 适配器~~（已完成）、按需同步 Release。

## 发布

推送 `v*` 标签（例如 `git tag v0.1.0 && git push origin v0.1.0`）会触发 `.github/workflows/release.yml`：

- 构建 Linux、macOS 的 amd64、arm64 二进制，连同 `SHA256SUMS` 发布到 GitHub Release；
- 构建多架构 Docker 镜像，推送到 `ghcr.io/qwq-inc/gitrepo:<标签>` 和 `:latest`。

## 开发

```bash
make test     # go test -race ./...（端到端测试会启动真实的 git http-backend）
make build    # bin/gitrepo, bin/gr
```

许可证：Apache-2.0。
