# 三节点跨网实测手册（Windows 多跳）

> 目标：Mac / 云服务器（公网）/ Windows（另一局域网）三台真机组成一个 mesh，
> Windows 升级到 v0.1.0 后跑通跨网任务，并验证打洞是否真的建立直连。
> 依据：[design-cross-network-three-node.md](../plans/design-cross-network-three-node.md)

---

## 0. 前置清单

| 项 | 状态 | 说明 |
|---|---|---|
| 三台都装 v0.1.0 | ☐ | Windows 必须升级：v0.0.11 **没有** transit/dirsync/pathtry，连中继都不会用 |
| 三台共享同一个 `network.shared_secret` | ☐ | UDP 平面没有 secret 直接不启动 |
| 云服务器放行 7836/tcp + 7837/udp | ☐ | 只对两台的出口 IP 放行 |
| Mac 上 panda 绕过 Clash | ☐ | TUN 开着的话打洞会打在代理上 |
| 三台都配 `stun_servers` | ☐ | 跨网打洞必须知道自己的公网 **IP:端口** |

---

## 1. 云服务器（会合点 + 中继）

### 1.1 放行端口（只对两台设备的出口 IP）

```bash
# 先查 Mac 的出口 IP（在 Mac 上跑）：curl -s https://ifconfig.me
sudo ufw allow from <MAC_出口IP>  to any port 7836 proto tcp
sudo ufw allow from <MAC_出口IP>  to any port 7837 proto udp
sudo ufw allow from <WIN_出口IP> to any port 7836 proto tcp
sudo ufw allow from <WIN_出口IP> to any port 7837 proto udp
sudo ufw status numbered | grep 783
```

> 不要 `allow from any`——这台机器还跑着 nginx 与 PandaPal。

### 1.2 配置（`~/.config/openpanda/config.yaml` 的 network 段）

```yaml
node:
    name: panda-cloud
    resource_class: Standard
    kind: vm
    identity: vm-<你的主机标识>       # 决定运行时 node id，三台必须各不相同
network:
    listen_addr: 0.0.0.0:7836
    udp_listen: "0.0.0.0:7837"        # 打洞数据面
    shared_secret: "<三台共享>"
    panel_addr: 127.0.0.1:7840
    panel_token: "<留空则自动生成>"

    # 公网直出的节点才当会合点
    rendezvous: always
    bootstrap_edges: 2

    # 盲中继：mac/windows 之间的帧经这里转发（端到端密封，中继只见到
    # "for k:<pub>, TTL, N bytes"）
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

    stun_servers:
        - "stun.cloudflare.com:3478"
```

### 1.3 启动并自检

```bash
panda daemon --config ~/.config/openpanda/config.yaml
panda doctor      # 看 mesh listener / rendezvous
panda nodes       # 应看到自己；两台接入后应为 3
```

---

## 2. Mac（NAT 设备）

### 2.1 Clash 必须放行 panda 直连

Clash Verge → 规则，置顶加一条：

```yaml
rules:
  - PROCESS-NAME,panda,DIRECT
  - PROCESS-NAME,panda.exe,DIRECT
```

或临时关掉 TUN 模式。**这一步不做，打洞必然失败**——UDP 会被代理劫走，NAT 映射开在代理的出口上，而不是你的路由器上。

### 2.2 验证 STUN 还能看到自己的映射

```bash
python3 -c "
import socket,struct,secrets
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',7837))
s.sendto(struct.pack('!HHI',0x0001,0,0x2112A442)+secrets.token_bytes(12),('stun.cloudflare.com',3478))
s.settimeout(5); print('STUN 回应字节数:', len(s.recvfrom(2048)[0]))"
```

有回应 = UDP 出得去。

### 2.3 配置

```yaml
node:
    name: panda-mac
    resource_class: Standard
    kind: physical
network:
    listen_addr: 0.0.0.0:7836
    udp_listen: "0.0.0.0:7837"
    shared_secret: "<三台共享>"
    panel_addr: 127.0.0.1:7840

    peers:
        - 81.70.135.110:7836        # 云服务器：唯一能交换目录信息的一跳
        - "punch:<windows运行时node-id>"   # 显式打洞句柄，见 §5

    auto_punch: true
    transit: true
    rendezvous: never                # NAT 后，auto 也拿不到公网证明
    allow_cleartext_for:
        - "81.70.135.110"           # ws:// 到公网 IP：sessaead 协商成功后这条闸不触发

    stun_servers:
        - "stun.cloudflare.com:3478"
        - "stun.nextcloud.com:443"
```

---

## 3. Windows（NAT 设备，必须先升级）

### 3.1 升级到 v0.1.0

归档已在 `dist/panda-0.1.0-windows-amd64.zip`（含 `panda.exe` + `adapters/` + `drivers/` + `extensions/`）。

在 Windows 上（管理员 PowerShell）：

```powershell
# 1) 停掉旧 daemon（计划任务 PANDA）
Stop-ScheduledTask -TaskName PANDA -ErrorAction SilentlyContinue
Get-Process panda -ErrorAction SilentlyContinue | Stop-Process -Force

# 2) 备份旧版本
Copy-Item C:\panda\panda.exe C:\panda\panda.v0.0.11.bak -ErrorAction SilentlyContinue

# 3) 解包新版本（把 zip 传到 C:\panda-v010.zip）
Expand-Archive -Path C:\panda-v010.zip -DestinationPath C:\panda-v010 -Force
Copy-Item C:\panda-v010\openpanda\bin\panda.exe C:\panda\panda.exe -Force
Copy-Item C:\panda-v010\openpanda\adapters\* C:\panda\adapters\ -Recurse -Force
Copy-Item C:\panda-v010\openpanda\drivers\* C:\panda\drivers\ -Recurse -Force -ErrorAction SilentlyContinue
Copy-Item C:\panda-v010\openpanda\extensions\* C:\panda\extensions\ -Recurse -Force -ErrorAction SilentlyContinue

# 4) 验证
C:\panda\panda.exe version     # 应报 v0.1.0 Constellation
```

> 若走 `panda update apply`，因为本地包没有 `OPENPANDA_RELEASE_KEY` 签名，需要
> `OPENPANDA_UPDATE_UNSIGNED=1`；否则更新器会按设计拒绝无签名更新。

### 3.2 防火墙（**新增 UDP 7837**）

```powershell
New-NetFirewallRule -DisplayName PANDA_7836 -Direction Inbound -Protocol TCP -LocalPort 7836 -Action Allow
New-NetFirewallRule -DisplayName PANDA_7837 -Direction Inbound -Protocol UDP -LocalPort 7837 -Action Allow
Get-NetFirewallRule -DisplayName PANDA_783* | Format-Table DisplayName,Enabled
```

### 3.3 配置（`C:\panda\config.yaml`）

```yaml
node:
    name: panda-win
    resource_class: Standard
    kind: physical
network:
    listen_addr: 0.0.0.0:7836
    udp_listen: "0.0.0.0:7837"
    shared_secret: "<三台共享>"
    panel_addr: 127.0.0.1:7840

    peers:
        - 81.70.135.110:7836
        - "punch:<mac运行时node-id>"

    auto_punch: true
    transit: true
    rendezvous: never
    allow_cleartext_for:
        - "81.70.135.110"

    stun_servers:
        - "stun.cloudflare.com:3478"
        - "stun.nextcloud.com:443"
```

### 3.4 重启 daemon

```powershell
Start-ScheduledTask -TaskName PANDA
Get-Content C:\panda\daemon.log -Tail 40
```

---

## 4. 三台互相看见

在**任意一台**上：

```bash
panda nodes
```

期望三行都是 `running`。`panda doctor` 里应看到：

```
✓ mesh listener     0.0.0.0:7836 (shared_secret set)
✓ peer 81.70.135.110:7836   linked as <对方node-id> — session encrypted
```

---

## 5. 打洞（关键验证）

### 5.1 运行时 node id 是什么

打洞角色按**运行时 node id 的字典序**选举（小的当 offerer）。三台的 id 必须形式一致，
否则两边都认为自己是 offerer，握手永远不成。

```bash
panda nodes          # node 列就是参与比较的字符串
```

- 物理机：`node.name` 本身（如 `panda-mac`）
- 虚拟机：`name@vm-<hash>`（如 `panda-cloud@vm-3d2a1b`）

`punch:<id>` 里填的是**这个运行时 id**。

### 5.2 观察打洞是否建立

在 Mac 上：

```bash
panda logs <task-id>       # 或看 daemon 日志
```

打洞成功时日志出现：

```
udp: pinhole confirmed   peer=<对端id> addr=<对端公网 IP:端口> ack=true
```

出现这行 = **Mac 与 Windows 之间建立了不经过云服务器的直连**。

没有这行但任务仍然成功 = 走的中继（功能正常，只是延迟/带宽经过云服务器）。

### 5.3 打洞失败时的排查顺序

| 症状 | 原因 | 处理 |
|---|---|---|
| `pinhole confirmed` 从不出现，Mac 侧有 `punch offer failed` | 对端候选地址学不到 | 检查两台的 `stun_servers` 是否可达；`stun.l.google.com:19302` 在部分网络不通，换 `stun.cloudflare.com:3478` |
| Mac 侧 `refusing cleartext link` | sessaead 没协商上 | 两台版本都要 ≥ v0.1.0；检查 `shared_secret` 是否一致 |
| 打洞总打中继（日志显示 `task_result handed to transit`） | 端口映射被改写 | 属 EIM 之外的 NAT；功能不受影响，可用中继 |
| 云服务器 `peer not connected` | 7836 没放行 / secret 不一致 | 查 ufw 与 `panda nodes` |

---

## 6. 端到端任务验证

### 6.1 Windows 执行一个任务（Mac 发起）

在 Mac 上：

```bash
panda task add --title "Windows 多跳验证" \
  --prompt "在 Windows 上执行：写一个 proof.txt，内容写上你这台机器的 hostname，然后读回来" \
  --requires agent:codex \
  --preferred <windows运行时node-id> \
  --wait --wait-timeout 180s
```

期望 `→ done`，且结果里能看到 Windows 的主机名。

### 6.2 三节点多跳（Mac → 云 → Windows）

把云服务器也放进链路：让 Mac 的 peer 列表保留云服务器，Windows 也连云服务器；
此时 Windows 与 Mac 之间没有直接边，任务要经云服务器中继一跳。

```bash
panda task <task-id> --trace
```

trace 里应出现两跳（`hop 1` / `hop 2`）。

### 6.3 带产物的多跳（验证 artifact 走中继）

在项目目录下放一个大一点的���件（≥1 MiB，会切成多片）：

```bash
dd if=/dev/urandom of=big.bin bs=1m count=3
panda task add --title "产物跨网回传" --prompt "处理 big.bin 并回报" \
  --project <project> --requires agent:codex \
  --preferred <windows运行时node-id> --wait --wait-timeout 300s
```

跑完后比对两端哈希：

```bash
shasum -a 256 <project>/big.bin
ssh xustalis-server 'sha256sum <远端路径>/big.bin'
```

一致 = 产物的分包、回传与校验都正确。

---

## 7. 收尾

```bash
# 停止测试 daemon
panda daemon --stop 2>/dev/null || pkill -f "panda daemon"
```

如果用了 SSH 隧道，隧道一并关掉。
