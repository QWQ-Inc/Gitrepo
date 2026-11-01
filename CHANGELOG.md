# 更新记录

发布时，把 `## Unreleased` 改成版本号（例如 `## v0.1.0`），然后推送同名标签。发布流程会把这一节作为 GitHub Release 的说明。

## Unreleased

第一个版本。

### 核心

- 一个 Git 地址（HTTPS / SSH），push 后同步到所有镜像：GitHub（也支持 GitHub Enterprise Server）、GitLab、Gitea / Forgejo、Gitee，以及任意 Git 服务器。push 到新地址时自动建仓，镜像仓库也通过各平台 API 自动创建。
- 每个镜像的同步结果以 `remote:` 行直接显示在 `git push` 的输出里。失败的镜像按指数退避自动重试，巡检兜底。
- 平台凭据只填一次（target），用 binding 声明"哪些组织的仓库备份到哪里"，支持名称模板。
- 凭据类型：访问令牌、GitHub App（不绑定个人，令牌自动换新）、用户名加密码、SSH 私钥。凭据用 AES-256-GCM 加密存储。

### 安全

- 用 `--force-with-lease` 推送，不静默覆盖：镜像上出现别人的提交时标记为分叉并告警。确实要覆盖时，先把镜像上的内容备份下来。
- 被强推覆盖或删除的历史隐藏保存在 `refs/backup/` 下，可以列出、恢复成分支，或彻底清除（清除时暂停 push）。
- 单文件大小上限取所有镜像中最严的，在 Hub 入口就拒绝超限的 push。
- 访问令牌分 read / write / create / admin 四级，可以限定仓库范围；SSH 公钥也按同样的方式授权。
- 审计日志：记录所有修改类操作（包括被拒绝的）和每次 push（标出强推），不记录任何凭据。

### 功能

- Git LFS：Hub 自带 LFS 服务，并同步到各镜像。
- 从已有镜像导入或恢复仓库。
- Release：在 Gitrepo 上发布一次，说明和附件同步到所有支持 Release 的镜像。等标签同步到对应镜像后才创建；不覆盖别人手工建的 Release；CI 用仓库级的 write 令牌即可。
- 源码包下载；bundle-uri 加速首次克隆。
- 只读边缘节点：在用户附近部署只读副本，clone 和 fetch 就近完成，push 自动转到 Hub。Hub 上的变化约 0.1 秒同步到边缘，吊销令牌、清除内容也会同步。
- 通知：签名 webhook，以及 Slack、企业微信、飞书、钉钉机器人。

### 运维

- 单个二进制加 SQLite，提供 Docker 镜像。
- Web 控制台、`gr` 命令行、HTTP API、Prometheus 指标。
- `gitrepo backup` / `restore`：服务不停机做整实例快照，恢复前会校验快照内容。
- `gr doctor`：部署自检。

### 已知限制

- 各平台的写入操作（建仓、推送、发布）只在模拟 API 上测试过；读取接口已用真实平台的公开 API 核对过。
- Hub 按单实例运行。
- GitLab 没有"预发布"标记；GitLab 的 Release 附件以链接形式出现。
