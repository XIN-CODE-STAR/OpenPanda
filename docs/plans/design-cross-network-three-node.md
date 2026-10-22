# 三节点跨网部署设计：Mac · 云服务器 · Windows

> 日期：2026-10-22
> 目标：三台设备各自安装 OpenPanda，分处三个不同网络，直接互相访问并协同完成任务
> 状态：设计已对照代码逐条核实；实施前需先完成「前置条件」一节

---

## 0. 结论速览

| 问题 | 结论 |
|---|---|
| 三台设备能互联吗？ | **能**，但必须把云服务器当作会合点（rendezvous）+ 中继（relay） |
| 现在能直接这么干吗？ | **不能**。Windows 上是 v0.0.11，**完全没有**跨网机制（无 transit 中继、无目录 gossip、无打洞） |
| 打洞代码本身有 bug 吗？ | **有 3 个**，已修（见 §3）。不修的话打洞必然失败 |
| 打洞在这台 Mac 上物理可行吗？ | **可行**。实测 NAT 为 EIM 映射（外部端口跨目标稳定），一次 STUN 结果对任意对端有效 |
| 需要动云服务器防火墙吗？ | **需要**。7836/tcp（WS）与 7837/udp（打洞）必须对 Mac 与 Windows 的出口 IP 放行 |
| Clash 代理会不会干扰打洞？ | **会**。Mac 上 Clash Verge 开着 TUN 且 `auto-route`，默认把流量劫走；打洞前必须让 panda 流量绕过代理，或关掉 TUN |

---

## 1. 现状核实（实测，不是推测）

### 1.1 三台设备的网络位置

| 设备 | 内网地址 | 公网地址 | NAT 形态 |
|---|---|---|---|
| Mac | `10.194.233.82`（网关 `10.194.233.69`） | `50.7.250.107` | 家用路由器，**EIM**（见 §1.3） |
| 云服务器 | `10.2.0.6` | `81.70.135.110` | 无 NAT，公网直出 |
| Windows | `192.168.0.107` | 未知（另一局域网） | 未知 |

### 1.2 云服务器防火墙（当前）

```
ufw: 22/tcp LIMIT · 80/tcp ALLOW · 443/tcp ALLOW
     7836 / 7837 —— 无任何规则（被默认 DROP 拦截）
```

今天的三节点实测是靠 `ssh -L 17839:127.0.0.1:7836` 隧道绕过的，**没有改防火墙**。真机部署必须放行。

### 1.3 Mac 的 NAT 形态（决定打洞是否可行）

用 STUN 从同一个 socket 向 5 个不同目标查询：

```
stun.cloudflare.com       -> 50.7.250.107:8577
stun.nextcloud.com        -> 50.7.250.107:8577
stun.miwifi.com            -> 50.7.250.107:8577
stun.voip.blackberry.com   -> 50.7.250.107:8577
stun.qq.com                -> 50.7.250.107:8577
→ 5 个不同目标，外部端口完全相同
```

**结论：EIM（Endpoint-Independent Mapping）。** 外部端口不随目标变化，所以一次 STUN 结果对「打洞到任意对端」都成立。这是打洞能work的物理前提——若是对端相关的映射（symmetric NAT），一次 STUN 结果只对 STUN 服务器有效，打洞必然失败。

本地端口 → 公网端口的映射是**改写**的（`17837 → 8690`，`45001 → 8656`），这一点直接暴露了 §3.1 的 bug。

### 1.4 STUN 服务器可用性（Mac 上实测）

| 服务器 | 状态 |
|---|---|
| `stun.cloudflare.com:3478` | ✅ 可达 |
| `stun.nextcloud.com:443` | ✅ 可达 |
| `stun.miwifi.com:3478` | ✅ 可达 |
| `stun.voip.blackberry.com:3478` | ✅ 可达 |
| `stun.l.google.com:19302` | ❌ 超时（**config.example.yaml 里的默认值不可用**） |
| `stun.qq.com:3478` | ❌ 超时 |

> `config.example.yaml` 默认示例是 `stun.l.google.com:19302`，在这台 Mac 上不通。必须换成上面可达的。

### 1.5 Clash Verge 的干扰

Mac 上 Clash Verge 开着 **TUN + auto-route**，网关 `28.0.0.1`，把 `192.168.0.0/16` 全部劫进代理：

```
route -n get 192.168.0.107 → gateway 28.0.0.1, interface utun1024
```

后果：
1. **Windows 从 Mac 不可达** —— TCP 能「连上」（被 Clash 接管）但收不到任何 banner，SSH 直接 `Connection closed`。
2. **打洞会打错地方** —— 若 panda 的 UDP 也走 TUN，喷出去的数据包从代理出口出去，NAT 映射开在代理上而不是你的路由器上，打洞必然失败。

**打洞前必须让 panda 进程绕过 Clash**（见 §5.3）。

---

## 2. 架构：云服务器做会合点与中继

```
        Mac (NAT)                          Windows (NAT)
   50.7.250.107:8577              <public>:<nat-port>
            │                                    │
            │  ①WS 会话（含 sessaead）            │
            │  ②UDP 打洞 → 直连                   │
            ▼                                    ▼
        ┌───────────────────────────────────────────┐
        │  云服务器 81.70.135.110                    │
        │  · rendezvous: always（会合点）            │
        │  · transit: true（盲中继，端到端密封）      │
        │  · 7836/tcp  7837/udp 对两台设备放行        │
        └───────────────────────────────────────────┘
```

**为什么云服务器是会合点而不是可选**：目录 gossip（`dir_rows`）只沿**直接相连的边**传播。云服务器同时与 Mac 和 Windows 有 WS 会话，所以它是唯一能把「Windows 的公网地址」传给 Mac 的通道。Mac 与 Windows 之间没有边，也就永远学不到对方。

**数据面有两条路**（`internal/core/pathtry.go` 的可达性阶梯）：

| 顺序 | 手段 | 说明 |
|---|---|---|
| 1 | 直连 WS | 两台 NAT 设备之间不可能 |
| 2 | 通告地址直拨 | 同上 |
| 3 | **UDP 打洞** | 目标：Mac ↔ Windows 直连，走公网，不经云服务器 |
| 4 | **盲中继 transit** | 经云服务器转发，帧端到端密封（X25519），中继只看得到 `for k:<pub>, TTL, N bytes` |
| 5 | DTN outbox | 离线托管，最后兜底 |

打洞成功 → 数据不经云服务器（低延迟、带宽不占中继）。打洞失败 → 自动落到中继，**功能不断**。这是设计的意图：**打洞是优化，中继是保底**。

---

## 3. 打洞路径的 3 个 bug（已修）

这三个都在 `internal/core/udp.go` / `pathtry.go`，**不修则打洞必然失败**——这也是为什么之前没测出真机跨网效果。

### 3.1 候选地址丢掉了 NAT 改写后的端口（P0）

`udpCandidates()` 原本把 STUN 答应的**公网 IP** 和**本地监听端口**重新拼在一起：

```go
if reflexive != "" {
    if h, _, err := net.SplitHostPort(reflexive); err == nil {
        add(h)          // ← 只取 host，端口被丢掉
    }
}
```

端口是地址的一半。NAT 把 `17837` 改写成 `8690`，于是候选变成 `50.7.250.107:17837` —— 这个端口上根本没有映射，对端朝它喷数据等于打在空气上。

**修复**：STUN 应答原样广播（`public-host:public-port`）。网卡地址与对端观察到的 IP 仍用本地端口——那两种情况是同网段可达，端口本就相同。

回归测试：`internal/core/udp_candidates_test.go`

### 3.2 `punch_want` 跨中继丢失请求方（P0）

中继转发的信封会被重新封装（`env.From = 中继自己`），因为接收方按 conn 绑定的对端 id 验签。原实现的 `PunchWantPayload` 只有 TTL，**请求方身份无处可存**。

结果：Mac 请求 Windows 打洞，帧经云服务器转发后，到达 Windows 时 `env.From` 是**云服务器**。Windows 会对**云服务器**喷打洞包——那台本来就直连可达的机器——而不是请求它的 Mac。

**修复**：`PunchWantPayload` 增加 `Src`，与 `PunchOfferPayload.Src` 同理。

回归测试：`TestRelayedPunchWantKeepsItsRequester`

### 3.3 打洞结果在「发起瞬间」就判负（P1）

`notePunchAttempt` 在 `PunchPeer()` 返回后立刻调用——而喷包窗口是 **4 秒**。在喷包有机会打通之前就记了一次失败，于是：

- 每一次成功前的打洞都被记为失败；
- 两次普通尝试就把对端标记为 `nat_suspect`（10 分钟不再打洞）；
- 成功分支实际不可达。

**修复**：判定延后一个窗口；已确认的路由立即清除怀疑标记（不必等窗口）。

---

## 4. 前置条件（必须先做完）

### 4.1 Windows 必须升级到 v0.1.0（硬性）

实测 `git cat-file`：

| 文件 | v0.0.11 | v0.0.12-preview | v0.1.0 |
|---|---|---|---|
| `internal/core/transit.go`（盲中继） | ❌ 缺失 | ❌ 缺失 | ✅ |
| `internal/core/dirsync.go`（目录 gossip） | ❌ 缺失 | ❌ 缺失 | ✅ |
| `internal/core/pathtry.go`（可达性阶梯） | ❌ 缺失 | ❌ 缺失 | ✅ |

跨网 P1–P5 是 v0.0.12-preview 之后才合入的（`a32c583` / `8071763` / `06d66f4` / `927fc05`）。**v0.0.11 的 Windows 节点连中继都不会用**，更别提打洞。

而且 Windows 的 v0.0.11 也有 §3.1 的同一个 bug（`git show v0.0.11:internal/core/udp.go` 第 364-370 行是同样的代码）。

**升级方式**（Windows 上）：
```powershell
# 用 v0.1.0 的 windows-amd64 归档替换 C:\panda\panda.exe
# 或在 Windows 机器上直接：
panda update apply --force
```
> 注意：`panda update` 需要 `OPENPANDA_RELEASE_KEY` 签名校验和；本地打包的归档没有签名，需要用 `OPENPANDA_UPDATE_UNSIGNED=1` 或手工替换二进制。

### 4.2 云服务器放行两个端口（需你授权）

```bash
sudo ufw allow from <MAC_EXIT_IP> to any port 7836 proto tcp
sudo ufw allow from <MAC_EXIT_IP> to any port 7837 proto udp
sudo ufw allow from <WIN_EXIT_IP> to any port 7836 proto tcp
sudo ufw allow from <WIN_EXIT_IP> to any port 7837 proto udp
```

建议**只对这两台的出口 IP 放行**，不要 `allow from any`（这台机器还跑着 nginx 与 PandaPal 服务）。

> 我没有执行这条命令——需要你明确授权，或者你告诉我两台设备的公网出口 IP 由我代劳。

### 4.3 三台设备共享同一个 `network.shared_secret`

UDP 平面没有 secret 直接不启动（`internal/core/udp.go:78-81`），打洞帧的 HMAC 也用它。

### 4.4 Mac / Windows 配置 STUN 服务器

打洞要拿到自己的公网 IP:端口。`You` 字段（hello 回应里的对端观察 IP）只给 **IP 不给端口**，所以跨网打洞**必须**配 STUN。用 §1.4 里实测可达的那几个。

---

## 5. 逐台配置

### 5.1 云服务器（会合点 + 中继）

`~/panda-e2e/config.yaml` 或正式配置里，`network` 段：

```yaml
node:
  name: panda-cloud
  resource_class: Standard
  kind: vm
  identity: vm-<你的云主机标识>
network:
  listen_addr: 0.0.0.0:7836
  udp_listen: "0.0.0.0:7837"      # 打洞数据面
  shared_secret: "<三台共享的密钥>"
  panel_addr: 127.0.0.1:7840

  # 会合点：公网可达，auto 模式在拿到 2 个对端的 dial_check 回执后会自证；
  # 这里显式 always，避免自证前有节点找不到它。
  rendezvous: always
  bootstrap_edges: 2

  # 盲中继：转发别人的密封帧。默认 trust.relay.transit 对 mesh 档是放行的，
  # 这里显式写出，避免三台设备里有 guarded 档时行为依赖默认值。
  transit: true
  trust:
    relay:
      transit:
        mesh: true
        guarded: true
        open: false
      serve:
        mesh: true
        guarded: false
        open: false

  # 云服务器自身不需要打洞（它公网直出），但要能被 STUN 之外的路径确认。
  stun_servers:
    - "stun.cloudflare.com:3478"
```

### 5.2 Mac（NAT 设备）

```yaml
node:
  name: panda-mac
  resource_class: Standard
  kind: physical
network:
  listen_addr: 0.0.0.0:7836
  udp_listen: "0.0.0.0:7837"      # 必须是 7837 之外也行，但不能是 "off"
  shared_secret: "<三台共享的密钥>"

  peers:
    - 81.70.135.110:7836          # 云服务器：唯一能交换目录信息的一跳
    - "punch:win-desktop"          # 显式打洞句柄，见下

  auto_punch: true
  transit: true
  rendezvous: never                # Mac 在 NAT 后，auto 也拿不到公网证明

  stun_servers:
    - "stun.cloudflare.com:3478"   # 打洞必需：只有它给出带端口的公网地址
    - "stun.nextcloud.com:443"

  # ws:// 到公网 IP：sessaead 协商成功后这条闸不生效（enforceLinkPolicy 只在
  # 未协商加密时触发）。保留 allow_cleartext_for 作为老版本对端的兜底。
  allow_cleartext_for:
    - "81.70.135.110"
```

**关于 `punch:win-desktop`**：`punch:<node-id>` 是显式打洞句柄（`internal/config/config.go:1394`），它绕过 `nat_suspect` 与 60 秒自动打洞间隔，是给「我知道对端在 NAT 后」的确定性用法。node-id 要用**运行时 id**（`panda nodes` 里显示的那一列），不是显示名。

### 5.3 Windows（NAT 设备，v0.1.0）

```yaml
node:
  name: win-desktop
  resource_class: Standard
  kind: physical
network:
  listen_addr: 0.0.0.0:7836
  udp_listen: "0.0.0.0:7837"
  shared_secret: "<三台共享的密钥>"

  peers:
    - 81.70.135.110:7836
    - "punch:<mac-runtime-node-id>"

  auto_punch: true
  transit: true
  rendezvous: never
  stun_servers:
    - "stun.cloudflare.com:3478"
    - "stun.nextcloud.com:443"
  allow_cleartext_for:
    - "81.70.135.110"
```

**Windows 防火墙**要放行（管理员 PowerShell）：
```powershell
New-NetFirewallRule -DisplayName PANDA_7836 -Direction Inbound -Protocol TCP -LocalPort 7836 -Action Allow
New-NetFirewallRule -DisplayName PANDA_7837 -Direction Inbound -Protocol UDP -LocalPort 7837 -Action Allow
```
（TCP 7836 之前已放行；**UDP 7837 是新增的**，打洞数据面要用。）

### 5.4 Clash 绕过（Mac 必做）

打洞的 UDP 必须走物理网卡，不能进代理。三选一：

**A. 在 Clash Verge 里给 panda 进程加直连规则**（推荐）
```yaml
rules:
  - PROCESS-NAME,panda,DIRECT
  - PROCESS-NAME,panda.exe,DIRECT
```

**B. 临时关掉 TUN 模式**（Clash Verge → 设置 → TUN 模式关）

**C. 用环境变量绕过**（仅对 Go 程序部分有效，不如 A/B 可靠）

验证：打洞前在 Mac 上跑
```bash
# 应该看到本机公网 IP 的稳定映射，而不是代理出口
python3 -c "
import socket,struct,secrets
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',7837))
print('local',s.getsockname())
s.sendto(struct.pack('!HHI',0x0001,0,0x2112A442)+secrets.token_bytes(12),('stun.cloudflare.com',3478))
s.settimeout(5); d,_=s.recvfrom(2048); print('reflexive hint ok', len(d))
"
```

---

## 6. 打洞的角色选举（一个必须知道的细节）

`internal/core/pathtry.go:381` 按**运行时 node id 的字典序**选边：小的当 offerer（喷包），大的发 `punch_want`（请求）。

- `panda-mac` < `win-desktop` → Mac 当 offerer，Windows 发 want。
- 两边必须比较**同一个字符串**。若一台用的是 `k:<pub>` 稳定 id、另一台用实例 id，两边会各自认为自己是 offerer，各自去喷对方 —— 双方都以为自己是小的那边，握手永远不成。

**对策**：三台都用 `node.name`（运行时 id 由 name 派生，`RuntimeNodeID`），确保 id 形式一致。`panda nodes` 输出里的 node 列就是参与比较的字符串。

---

## 7. 实施顺序

```bash
# ── 0. 前置检查（Mac）
#    Clash 规则加 panda → DIRECT，然后验证 STUN 仍能看到本机映射

# ── 1. 云服务器（需你授权放行端口）
sudo ufw allow from <MAC_IP> to any port 7836 proto tcp
sudo ufw allow from <MAC_IP> to any port 7837 proto udp
sudo ufw allow from <WIN_IP> to any port 7836 proto tcp
sudo ufw allow from <WIN_IP> to any port 7837 proto udp
# 部署 panda 0.1.0 + 上述 config.yaml，起 daemon

# ── 2. Mac
#    用 v0.1.0 二进制，配置 peers: [云服务器, punch:win-desktop]
#    起 daemon

# ── 3. Windows
#    升级到 v0.1.0，装配置，放行 UDP 7837，起 daemon

# ── 4. 验证（三台都要看到另外两台）
panda nodes
panda doctor            # 看 mesh / 打洞自检
panda task add --title "跨网验证" --prompt "报告你在哪台机器上" --wait
```

**期望结果**：
- `panda nodes` 三台全部 `running`
- 打洞成功时日志出现 `udp: pinhole confirmed`（`internal/core/udp.go:655`）
- 打洞失败时三台仍能互通——走中继，帧端到端密封

---

## 8. 已知限制

| 限制 | 表现 | 缓解 |
|---|---|---|
| Windows 的 NAT 形态未知 | 若是对称 NAT，打洞失败 | 中继兜底，功能不断；或给 Windows 配端口映射 |
| CGNAT / 运营商 NAT | 无公网 IP，打洞无从谈起 | 中继兜底 |
| 打洞窗口 4 秒 | 跨洲 RTT 下可能不够 | 60 秒重试间隔 + 中继兜底 |
| STUN 单次查询 | NAT 映射过期后候选失效 | 路由 TTL 90 秒 + keepalive 续期；必要时加重查 |
| Windows 升级需签名 | `panda update` 拒绝无签名归档 | 手工替换二进制，或 `OPENPANDA_UPDATE_UNSIGNED=1` |
| `punch_want` 需同 id 形式 | 两边都当 offerer，握手不成 | 统一用 node.name 派生的运行时 id |
