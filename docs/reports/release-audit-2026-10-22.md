# OpenPanda v0.1.0 发布审计报告

> 日期：2026-10-22
> 版本：`v0.1.0` "Constellation"（第一个发布级版本）
> 范围：静态审计（安全 / 任务执行 / 存储与界面三路并行）+ 真实跨网双设备与三节点多跳实测
> 结论：**可发布**。审计中发现的发布阻塞项已全部修复并回归；真实跨网多跳任务（含 3 MiB 项目树）端到端通过。

---

## 1. 门禁与静态检查

| 检查 | 结果 |
|---|---|
| `make gate-all` 非 race 腿（fmt / vet / staticcheck / govulncheck / build / adapter-test / test） | 通过 |
| 竞态检测（core / commander / bus / storage / askengine / scheduler / panel） | 通过；无 `DATA RACE`（core 560s，panel 139s） |
| Web 控制台（`make web-gate`：typecheck + 227 个单测 + 生产构建） | 通过 |
| TUI 真实 PTY 契约（mouse + exit） | 通过 |
| `govulncheck` | No vulnerabilities found |
| 适配器契约（68 例） | 68 passed |

> 说明：首次 `make gate-all` 的 race 阶段与本机**另一个并发 Claude 会话**的同一套测试抢用固定端口（`bind: address already in use: 127.0.0.1:17947`、`:17837` bad handshake）。该套件退出后单独复跑 `make race-focused` 全绿；上述 race 结果是清场后的结果。

---

## 2. 真实跨网测试

### 2.1 拓扑

```
Mac（e2e-mac，物理机，127.0.0.1:17836）
   │  WS + per-session AEAD
   ▼
e2e-relay（本机第二实例，127.0.0.1:17846）
   │  WS over SSH tunnel（公网 WAN）
   ▼
云服务器（VM-0-6-ubuntu，81.70.135.110:7836，独立 HOME/DB）
```

三节点、两跳；A 只知道 B，B 通过 SSH 隧道连接 C。云服务器 UFW 默认不放行 7836/7837，测试**没有**修改防火墙，而是用 `ssh -L` 隧道把云端的 WS 端口映射到本机回环，真实字节仍走公网 WAN。

### 2.2 结果

| 场景 | 证据 |
|---|---|
| 双设备跨网任务（A → C，经隧道） | `01a15fa0-3bfd-79a1-859a-6113bd304360` → `done`；云上执行结果含 `FAKE-CODEX node=VM-0-6-ubuntu` |
| 三节点多跳任务（A → B → C） | `01a15fa4-ee90-7355-8295-05f80ec68fbe` → `done`；trace 为 `e2e-mac → VM-0-6-ubuntu`，结果从 C 经 B 的 transit 回传 |
| 多跳 + 3 MiB 项目输入 | `01a15fb2-3aea-7ae8-8cc6-9f0fa7238df8` → `done`；云端落盘 `3,148,458` 字节，SHA-256 前缀 `c3ed4ae11fe36f9b` 与本机一致 |
| 最终 0.1.0 二进制复测 | `01a15fbe-ecf3-7653-b82f-114c51fc027d` → `done`；3 MiB 输入哈希一致 |
| 审批路径 | 首轮任务因 supervisor 存根不可解析停在 `review`，`panda approve` 后镜像为 `done` |
| 控制台侧车 | `/api/sessions`、`/api/skills`、`/api/todos`、`/api/projects` 均 200；无 token 401；`/` 返回 200 HTML |
| 节点自检 | `panda doctor` 能识别三节点链路（`linked as e2e-relay — session encrypted`） |

> 测试环境使用存根模型端点与假 `codex` CLI，目的是验证传输、路由、审批、产物与进程链路，不消耗真实模型额度；被测代码路径与生产一致。

### 2.3 实测发现并修复的多跳缺陷

三节点测试第一次运行即暴露：执行节点与产物持有者无直接连接时，`artifact_fetch` 被明确排除在 transit 之外，导致 `project inputs: ... peer not connected: e2e-mac`。修复后：

- `artifact_fetch` / `artifact_chunk` 允许走盲中继；内层 `From/To` 保留在密封信封中，因此回复能回到请求方。
- `transitMaxInner` 从 1 MiB 提升到 2 MiB，容纳 1 MiB 分片的两次 base64 膨胀（外层仍低于 4 MiB 读上限）。
- `fetchStageInputs` 在生产者无直连时**优先询问链上前驱**，避免 transit 授权失败时把一次快速失败拖成整个传输预算的超时。
- 不改变 `artifact_push` 的排除：推送是主动的，没有可绑定的拉取凭据。

---

## 3. 已修复的发布阻塞项

| # | 级别 | 问题 | 修复 |
|---|---|---|---|
| 1 | P0（功能） | 模型自报 `status:"failed"` 被适配器正常退出掩盖，任务记为 `done` | `applyStructuredFailure` 将失败写回 `res.OK=false`，原因进入重试/委派/时间线 |
| 2 | P0（多跳） | 中继后的产物拉取失败（见 §2.3） | transit 数据面 + 前驱优先 |
| 3 | P1（安全） | 已知节点可省略 Ed25519 字段被冒充，继承其信任档 | 节点 ID 有公钥记录时，hello 必须出示该公钥；换钥需运维操作 |
| 4 | P1（安全） | 委派项目包在信任准入**之前**解包，且无接收端上限 | 仅 mesh 来源可提前落包；其余在准入后落包；2 MiB 压缩上限、64 MiB 解压上限、256 MiB 磁盘余量 |
| 5 | P1（安全） | 数据报 `hello_reject` 解引用 nil conn，杀死整个 UDP 平面 | 数据报路径丢弃 hello 帧；处理函数容忍 nil conn |
| 6 | P1（安全） | `sandbox.mode: strict` 只禁写、不禁读节点机密 | 配置文件/能力卡、SQLite 与 WAL/SHM（节点私钥）、OAuth token 存储按文件加入读拒绝；父目录保持可读以保住工作树 |
| 7 | P1（功能） | 对端拒绝后，有能力的发起节点误报失败且同步 Submit 永久等待 | 拒绝重路由到本地时 `SetQueueMeta + ClaimLocal` 并驱动 `runScheduled` |
| 8 | P1（界面） | 独立 `panda-webui` 侧车缺少 Sessions/Worktrees/SkillStore，线程与技能页 404 | 侧车补齐依赖并传入 CardPath |
| 9 | P1（安全/数据） | 会话合并把 `.openpanda/uploads` 等运行态提交进用户分支 | `.git/info/exclude` 排除整个 `.openpanda/` |
| 10 | P1（数据） | 卸载备份 0644（含 mesh secret / provider key / 节点私钥），且读取失败被吞后仍删除原文件 | 备份 0600；任一文件不可读即返回错误；备份失败时 `ExecuteUninstall` 在删除前中止，CLI exit 1 |
| 11 | P1（崩溃） | `panda model add custom <url>` 在空位置参数上 index panic | 空列表返回 usage 错误，补回归测试 |
| 12 | P2（安全） | `OPENPANDA_UPDATE_UNSIGNED=0` 反而启用无签名更新 | 用 `strconv.ParseBool` 解析，0/false 关闭 |
| 13 | P2（界面） | 实时 plan 轮次渲染成空气泡；线程加载失败保留上一线程内容与错误 | 采纳 `kind:plan + plan_id`；切换线程时清空、成功/失败都重置错误 |
| 14 | P2（i18n） | `sessions.castPlan` 在五个语言包中缺失 | 五语补齐 |
| 15 | P2（脚本） | `agents test` / `config model test` / `session rm` 的 `--json` 失败路径 exit 0 | JSON 分支按失败设置非零退出码；`session rm` 只输出 JSON |
| 16 | P2（恢复） | 重启会把未准入的远程 submitted 行直接 requeue | `Recover` 加 `remote = 0`；未准入行由 `CancelStaleRemoteSubmits` 处理 |
| 17 | P2（权限） | 上传目录 0755、文件 0644 | 目录 0700、文件 0600 |
| 18 | 发布 | 版本仍为 `0.0.12-preview` | `0.1.0` / `2026-10-22`；web package、package.sh、Homebrew 回退、五语 CHANGELOG 与 README 同步 |

---

## 4. 已知但未阻塞发布的问题

以下问题不构成 P0，未在本次改动中处理，建议 v0.1.1 跟踪：

1. **链中继死亡时的重复执行**（P1 韧性）：A→B→C 中 B 死亡时，A 的租约到期会改派，而 C 可能仍在新执行；C 的结果回到 B 后因 attempt 过期被丢弃。需要“中继租约”或结果短路机制，改动面较大。
2. **远程工作树回传失败静默**（P2）：任务已 `done` 后 `adoptWorktreeOutput` 失败只记日志，没有任务事件或失败标记。
3. **重路由重建 payload 会使 origin 的 consent digest 失效**（P2）：仅影响已授权的项目/文件任务在拒绝后改派。
4. **准入 review 的拒绝/取消不上溯**（P2）：发起方要等租约超时才发现。
5. **`awaitSettled` 可能读到瞬态 `failed`**（P2）：队列重试成功后最终行仍正确，但用户先看到失败。
6. **跨进程会话写入 / finalizer 双写**（P2）：CLI 与面板同时写同一 session 目录时可能丢一轮。
7. **`panda doctor --json` 仍输出人类文本**；`model test|switch|remove|fetch` 的失败退出码未统一。
8. **迁移无预备份 / 完整性恢复路径**；配置空路径与 env 覆盖顺序问题。
9. **`panda update` 覆盖 Homebrew keg、重启失败不报错**；建议在 v0.1.1 加 brew guard 与重启结果上报。

---

## 5. 发布检查清单

- [x] 门禁全部通过：fmt / vet / staticcheck / govulncheck / build / adapter / test / race（清场复跑）/ web（227 例）/ TUI PTY
- [x] 真实双设备跨网与三节点多跳任务通过（含 3 MiB 输入哈希一致）
- [x] CLI `panda version` → `v0.1.0 Constellation`
- [x] 控制台侧车 API 与静态页面可访问
- [x] 五语 CHANGELOG / README 与版本元数据同步
- [x] `make package` 产出 9 个 `panda-0.1.0-*` 归档（含 lite Linux 三架构）并通过 `scripts/verify-release.sh 0.1.0`；解包后的 darwin-arm64 二进制报告 `v0.1.0 Constellation`，adapters/voice/drivers 齐全
- [x] 受影响的最终包再跑一遍：`internal/core`（含新恢复测试）、`cmd/panda`、`internal/config`、`internal/install` 全部通过；新增恢复用例在 `-race` 下通过
- [ ] 打标签 `v0.1.0` 并推送（由发布者执行；CI release 使用 `OPENPANDA_RELEASE_KEY` 签名校验和）
- [ ] 正式发布前确认 release secret `OPENPANDA_RELEASE_KEY` 可用；否则产物无 `checksums.txt.sig`，更新器会按设计拒绝更新

> 发布命令（在确认 secret 与 CI 绿灯后）：
>
> ```sh
> git tag -a v0.1.0 -m "v0.1.0 — Constellation"
> git push origin v0.1.0
> ```
