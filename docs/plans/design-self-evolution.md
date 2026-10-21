# 设计稿 — 自进化 × 签名更新通道（Self-Evolution）

> 状态：Phase 1–4 主体已实现（2026-10-21），默认关闭。代码以
> `internal/self/`、`internal/updater/`、`internal/core/selfmerge.go`、
> `cmd/panda/selfcmd.go` 为准；与代码冲突时以代码为准。
> 本稿替代内部工作稿 `docs/author/2026-10-21-design-self-evolution.md`
> 的架构部分；内部工作清单不随仓库发布。

## 0. 一句话

让节点用它**自己的源码工作树**产出一个签名的补丁集，把它作为审批任务
停在人工合并门前；批准后在一棵专用 worktree 里应用、跑完整发版门禁、
构建、走**与官方 release 完全相同的 updater swap 通道**换装，并且永远
保留最近一份签名 release 二进制作为回滚目标。

## 1. 为什么这是一道架构题

官方更新通道的信任模型是 **fail-closed 验签**：二进制必须由 release
私钥签名，updater 才允许换装。自进化产物不可能有 release 签名——它是
节点自己的 key 签的源码构建。二者不能共用一条验签链：

- 让自构建冒充 release = 把验签门变成摆设；
- 让自构建绕过 updater = 两套换装路径，回滚语义分叉。

因此自进化是**第三条 provenance**（`release` / `self` / `unsigned`），
复用 updater 的 swap 机制，但走 `StageLocal` 入口，不经过下载验签。

## 2. 数据模型

### 2.1 补丁集（patch set）

一次自修改是一个目录：

```
<data>/self/queue/<id>/
  manifest.json      # openpanda-patchset/1
  0001-worktree.patch
```

`manifest.json` 字段：

| 字段 | 含义 |
|---|---|
| `kind` | 固定 `openpanda-patchset/1` |
| `id` | 目录名，安全路径元素 |
| `base` | 产物所对的 **commit SHA**（不是 `HEAD` 符号引用） |
| `ver` | 目标版本戳，如 `0.0.12-preview+self.3` |
| `prod` | `k:<ed25519-pub>`，产出节点稳定身份 |
| `files` | 按序应用的补丁文件名 |
| `digests` | 每个补丁的 sha256，**进签名域** |
| `gate` | 产出端 `make gate` 摘要（证据，不是权威） |
| `critical` | 产出端自称是否触及关键路径（合并端重算） |
| `sig` | 对去掉 `sig` 的 manifest 的 Ed25519 签名 |

签名绑定了补丁字节：签名后改 patch 会因 digest 不匹配而拒绝加载。
`base` 钉成 SHA，rebase 扫描才能判断集合是否过期。

### 2.2 关键路径

以下前缀下的改动必须被标记，供审查界面高亮（合并端从 diff 头重算，
不信任 manifest 的自称）：

```
internal/updater/ internal/security/ internal/trust/ internal/defense/
internal/bus/ internal/storage/ internal/self/
Makefile go.mod go.sum .github/ scripts/package.sh scripts/sign
```

## 3. 生命周期

```
panda self mint --title T          # 产出：worktree diff → 签名 → queue/
panda self propose <id>            # 打包成 StateReview 审批任务
panda approve <task-id>            # 人工批准
  → 拿 worktree 锁
  → git reset --hard <base>; git clean -fd; git apply
  → make gate（fmt-check/vet/staticcheck/build/adapter-test/test/race-focused）
  → go build -tags lite（lite 节点）
  → updater.StageLocal
  → updater.Apply（换装 binary + adapters）
  → archive(merged) + task Complete
  → 服务重启（在任务落定之后）
panda reject <task-id>             # 归档为 rejected（或就地标记）
```

失败语义：

- apply / gate / build / stage 失败 → 任务 failed，补丁集 `conflicted`；
- 服务重启被延后到任务落定，避免“二进制换了、任务永远 RUNNING”；
- 同一 worktree 的 apply/rebase/mint/reset 由跨进程锁文件串行化。

## 4. 信任与准入

- `self_evolution.producers`：
  - `self`（默认）：只接受本节点 key 签名的集合；
  - `mesh`：接受任意签名者的集合（为 Phase 5 跨节点流转预留）。
- 目录 gossip 的签名只证明“某 key 声称某 id”，**不能**为设备名 id
  首次钉 key；只有经过验签 hello 才能建立 pin。`k:<pub>` 是自证明 id，
  必须由它声明的 key 签名。
- 合并端在 propose 与 approve 两端都检查 base 是否仍是 repo HEAD；
  过期集合必须显式 `panda self rebase`，否则会静默回滚后续提交。
- 合并时重新分配 `+self.N`，避免多个排队集合都构建成同一个版本戳。

## 5. 配置

```yaml
self_evolution:
  enabled: false
  repo: ""                 # 启用时必填：本节点的 OpenPanda checkout
  producers: "self"        # self | mesh
  rebase_on_update: true   # 启动时把 pending 集合 rebase 到 repo HEAD
```

依赖：`go`、`git`、`make`、`python3`（adapter 合同测试）、C 工具链
（race-focused 的 CGO 腿）。`panda self doctor` 预检。

## 6. 逃生口

- 官方 release 每次成功 apply 后，把新二进制复制为 `panda.release`；
  `panda self rollback` 原路换回，`panda self reset` 先回滚再清状态。
- 回滚前探测 `panda.release` 支持的 schema；数据目录已升级到更高版本
  时拒绝回滚。
- self build 的 provenance 是 `self`，不是 `release`；trust 层可以据此
  区分。macOS ad-hoc 签名每次换装都会变 cdhash，TCC 授权需重新确认。

## 7. 不做 / 未做

- **Phase 5**：补丁集跨节点传输、逐跳 provenance 签名链、`producers:
  mesh` 的实际准入策略。
- **auto 模式**：只做 propose，不做无人审批自动合并。
- **产出侧 gate 摘要**：目前只在合并端跑真 gate，manifest 的 `gate`
  字段仍为空。
- 可配置的 `protected_paths`（当前硬编码关键前缀）、`bench` 腿。
- macOS 稳定签名身份（自换装不掉 TCC）。
- `panda update --no-rebase` 开关；rebase 扫描目前每次启动运行。
