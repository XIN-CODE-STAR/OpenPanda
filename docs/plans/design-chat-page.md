# 对话页（sessions）设计稿与生图 Prompt

日期：2026-10-15
状态：布局目标已部分实施；配色与生图 prompt 为历史方案（2026-10-20 更新）
归属：`docs/plans/design-webui-consolidation.md` 的第 7 页（对话），PR 分支 `refactor/webui-chat`
用途：先用生图模型出视觉稿 → 满意后按「设计思路 + 图」执行代码

> **2026-10-20 实施状态**
>
> - 配色已改为 `tokens.css` 的中性墨色体系，不再使用暖纸色（#F5F3EC/#EDE9DD）或绿色 tonal 按钮。第 3–5 节的生图 prompt 仅保留为历史设计探索，不是现行视觉契约。
> - 空态现在是：垂直居中的问候 + 一行说明 + 3 个建议 chip；点击 chip 把建议填入 composer，不直接发送。设备拓扑不再占据空态，输入框上的熊猫承担品牌标记。
> - 会话栏在桌面是侧栏历史；≤720px 时收进顶栏的导航面板，不再作为独立栏存在。

---

## 1. 现状审计（已核实）

结构（`views/sessions.tsx`，1925 行）：

```
thread-rail（会话栏）  project picker · scope banner · +新对话 · thread-tools · thread-list · bulk-bar · rail-scrim
chat-pane
  ├─ chat-head         rail-toggle · 标题组（标题 + 铅笔 + 项目归属） · CostChip
  ├─ chat-empty        ← 空态：左 50% FleetTopologyCard，右 50% hero（Panda 标记 + 问候 + 3 个 chip）
  │  或 chat-log
  └─ composer          toolbar（节点选择 · 范围选择） · textarea · 底行（提示 · 授权高风险操作 · 发送）
```

**问题清单（按优先级）**

| # | 问题 | 事实依据 |
|---|---|---|
| 1 | **空态主次倒置**：单机用户（多数新用户）看到左半屏是「设备网络」拓扑卡，而真正的主行动（提问）在底部输入框，视觉重心被次要信息占走 | `ChatEmpty` 是 50/50 分栏；`FleetTopologyCard` 在左 |
| 2 | **欢迎 hero 贴顶**，垂直不居中，上方大片空白、下方被 composer 挤压 | `chat-empty-cta` 随分栏自然高度 |
| 3 | **会话栏空态无引导**：只有一行灰字「还没有对话。」，就地没有行动按钮 | `thread-list` 里 `ordered.length === 0` 只渲染 `.thread-empty` |
| 4 | **「选择/管理」按钮在空列表时仍显示**，点了没有可操作对象 | `thread-tools` 无条件渲染 `thread-manage` |
| 5 | **顶部用量 chip 无常驻标签**：只有 `data-tip` 悬浮提示，扫一眼不知道是什么 | `CostChip` 渲染 `$x · Nk tok` + `data-tip` |
| 6 | **composer 的两个下拉可发现性弱**：`未选项目` / `自动(最佳节点)` 样式过轻，单机用户看到「最佳节点」是噪音 | `.composer-toolbar` 内两个 select |
| 7 | **标题语义重复**：顶部「新对话」与左栏「+ 新对话」并存 | `chat-title-text` 用 `sessions.new` |

**已经做对的**（不要改坏）：桌面会话历史在侧栏，≤720px 收进顶栏导航面板；三态有 `PageHeader`/`ErrorState`/`LoadingState` 可用。

---

## 2. 目标设计

**一句话：让「提问」成为绝对主角，把设备/网络信息降级为一条可点的细横幅。**

| 区域 | 改造 |
|---|---|
| 空态 | 取消 50/50 分栏；设备信息不占空态，主区让给居中的欢迎 hero（当前实现） |
| 欢迎 hero | 垂直居中于日志区；一句问候 + 一行说明 + 3 个建议 chip，点击 chip 填入 composer；品牌标记由输入框上的熊猫承担（当前实现） |
| composer | 输入框加大（视觉上成为页面的第二主角）；两个下拉合并为左侧一个「上下文」按钮组（图标 + 文本，展开才见细节）；发送按钮保持主色 pill（尚未实施） |
| 顶部栏 | 用量 chip 加常驻小标签（图标 + 数值），保留 hover 详情、点击跳系统页；标题只在已有会话时显示，新会话不显示重复的「新对话」（尚未实施） |
| 会话栏 | 桌面并入侧栏历史；≤720px 收进顶栏导航面板；会话行时间统一为相对时间（当前实现） |
| 响应式 | 目标：≥1280 三栏；960–1280 会话栏可收；<960 会话栏变抽屉、空态单列；<640 底行换行、chip 换行。未按此实施；当前壳层只在 720px 切换（见 `shell.css`） |
| 可及性 | 路由切换后焦点落到页面标题；抽屉打开时焦点进抽屉；用量 chip、建议 chip 均可 Tab 可达并有可读标签 |

---

## 3. 生图 Prompt

### 3.1 英文版（推荐给 Midjourney / DALL·E / GPT-image / Nano Banana）

```
A clean flat UI design mockup of a desktop web application called "OpenPanda", a
local-first multi-agent orchestration console. Straight-on screenshot, no device
frame, no perspective, 16:10 aspect ratio.

THREE-COLUMN LAYOUT on a warm off-white background (#F5F3EC):

Left sidebar, about 190px, slightly darker warm grey (#EDE9DD): a small green panda
logo with the wordmark "OpenPanda" at the top; a rounded search field carrying a
small keyboard-shortcut hint; three labelled navigation groups, each with a tiny
uppercase caption and two to four nav items; the active item uses a soft green
tinted pill with a thin green accent bar on its left edge; at the bottom a compact
node status row (a small green dot, a node name, a tiny version label) and two
utility links.

Middle session rail, about 210px, same warm grey: a compact search input at the top,
a full-width secondary button "+ New chat" below it, then five to six conversation
rows — each row has a medium-weight title, a small rounded project tag, and a
right-aligned light-grey relative timestamp; one row is selected, shown with a subtle
tinted background and a thin green left border.

Main pane, filling the rest: a slim top bar with the conversation title on the left
next to a small pencil icon, and on the right a single small monospace chip showing a
usage number with a tiny label; below it a large empty state with one soft rounded
card centred in the upper-middle area, containing a green pixel-art wordmark, a
friendly one-line greeting, a short grey subtitle, and three pill-shaped suggestion
chips in a row; at the very bottom a prominent composer — a large rounded input
surface with a hairline border, a placeholder line, and a bottom row inside it with a
small hint on the left, a labelled checkbox, and a solid green rounded "Send" button
on the right.

DESIGN LANGUAGE: flat, no gradients, 1px hairline borders (#DDD6C4), very soft
shadows, 8-14px rounded corners, generous whitespace, restrained palette of warm
paper neutrals with a single green accent (#0C7A55) and a sparing blue-grey
secondary. Typography: modern geometric sans-serif, clear hierarchy, 13-15px body,
medium weight for titles; monospace only for ids and numbers. Mood: calm,
professional, uncluttered, high information clarity.

AVOID: dark theme, neon or saturated colours, gradients, glossy 3D, glassmorphism,
isometric or angled perspective, device mockups, hands, people, stock-photo
elements, decorative illustration, coloured drop shadows.
```

### 3.2 中文版（推荐给即梦 / Seedream / 文心一格）

```
一张桌面端 Web 应用的扁平化 UI 设计稿，产品名「OpenPanda」，本地优先的多智能体编排控制台。
正视角度截图，无设备外框、无透视，16:10 比例。

三栏布局，暖白色背景（#F5F3EC）：

左侧边栏约 190px，暖灰底色（#EDE9DD）：顶部是绿色熊猫图标 + 「OpenPanda」字标；一个带快捷键提示的圆角搜索框；三组导航，每组有小号分组标题和 2–4 个导航项；当前项「对话」用淡绿色胶囊高亮，左侧带一条细绿竖条；底部是一行紧凑的节点状态（绿点 + 节点名 + 小号版本号）与两个工具链接。

中间会话栏约 210px，同底色：顶部一个紧凑搜索框，下面一个整宽次级按钮「+ 新对话」，再往下 5–6 条会话行——每行有中等字重的标题、一个小号圆角项目标签、右对齐的浅灰相对时间；其中一条为选中态，淡色底 + 细绿左边框。

右侧主区：顶部细条，左侧是会话标题（旁边一个小铅笔图标），右侧一个小号等宽字体胶囊，显示用量数值加一个小标签；中部是大面积空态，一张柔和的圆角卡片居中偏上，卡片内含绿色像素风字标、一行问候语、一行灰色说明、三个胶囊形建议按钮；最底部是醒目的输入区——大号圆角输入框、1px 细描边、一行占位提示文字，框内底部一行左侧是提示文字、中间是带文字的复选框、右侧是实心绿色圆角「发送」按钮。

设计语言：扁平、无渐变、1px 细边框（#DDD6C4）、极轻阴影、8–14px 圆角、留白充足；配色克制——暖纸色中性色 + 单一绿色强调（#0C7A55），少量蓝灰辅助；现代几何无衬线字体，层次清晰，13–15px 正文，标题中等字重；等宽字体只用于 id 与数字。整体气质：安静、专业、不拥挤、信息清晰。

避免：深色主题、霓虹或高饱和色、渐变、玻璃拟态、3D 或等距透视、设备外框、人物与手、装饰性插画、彩色投影。
```

### 3.3 参数与使用建议

| 项 | 建议 |
|---|---|
| 比例 | 16:10（1440×900）最贴近真实控制台；退而求其次 16:9 |
| 风格词 | 追加 `UI/UX design mockup`、`flat design`、`Dribbble style`、`high fidelity` |
| 出图数量 | 一次出 2–4 张挑一张；同一 prompt 换 seed 比改 prompt 更能选出好构图 |
| **关于文字** | 图里的文字**大概率是乱码或伪英文**——这是正常的。**只看布局、层级、配色、间距、留白**，不要因为文字乱码就否定构图 |
| 若模型支持负面词 | `dark theme, gradient, neon, 3d render, glassmorphism, perspective, device frame, people, hands, watermark` |

---

## 4. 出图后的验收清单（判断"满意"的标准）

- [ ] 三栏宽度比例合理：侧栏 ≤ 主区的 1/4，会话栏略窄于侧栏
- [ ] **主区第一眼看到的是欢迎卡片，不是设备信息**（问题 1 是否被解决）
- [ ] 欢迎卡片在垂直方向大致居中，不贴顶（问题 2）
- [ ] 底部输入区足够醒目，是页面第二视觉重心
- [ ] 配色只有暖纸中性色 + 一个绿色强调，没有多余彩色
- [ ] 留白充足，不拥挤；卡片圆角与描边克制
- [ ] 会话栏有空态引导按钮（问题 3）

---

## 5. v2 Prompt（按用户对首轮出图的反馈修订）

首轮出图整体满意，两处配色需收敛：

| 反馈 | v1 | v2 |
|---|---|---|
| 项目标签 | 一个标签一种彩色（紫/蓝/青/绿/橙…） | **全部统一为同一种中性灰**（暖浅灰底 `#EAE6D9` + 灰褐文字 `#6E6757`），全图只有这一种标签色 |
| 「新对话」按钮 | 实心深绿 + 白字 | **淡绿底 + 深绿字 + 1px 细绿描边**（tonal），不要深色块、不要白字 |
| 「发送」按钮 | 实心深绿 + 白字 | 同上，与「新对话」同一处理 |
| 其余 | — | 不变（布局、hero、输入区、留白、暖纸色系） |

### 5.1 v2 中文 Prompt（推荐）

```
一张桌面端 Web 应用的扁平化 UI 设计稿，产品名「OpenPanda」，本地优先的多智能体编排控制台。
正视角度截图，无设备外框、无透视，16:10 比例。整体配色极度克制：只有暖纸色中性色系 + 一种低饱和绿作为唯一强调色，没有任何其他彩色。

三栏布局，暖白色背景（#F5F3EC）：

左侧边栏约 190px，暖灰底色（#EDE9DD）：顶部是绿色熊猫图标 + 「OpenPanda」字标；一个带「Ctrl K」快捷键提示的圆角搜索框；三组导航，每组有小号灰色分组标题（主要功能 / 项目管理 / 系统设置），组内是若干导航项；当前项「对话」用极淡的绿色底 + 左侧一条细绿竖条表示选中；底部是一行紧凑的节点状态（小绿点 + 「本地节点」 + 小号版本号 v1.2.0）与两个工具链接。

中间会话栏约 250px：顶部一个紧凑搜索框；下面一个整宽按钮「+ 新对话」——这个按钮是淡绿色底、深绿色文字、1px 细绿描边，不要实心深色块、不要白色文字；再往下 6 条会话行，每行有中等字重的标题、一个项目标签、右对齐的浅灰相对时间。所有项目标签必须使用完全同一种中性样式：暖浅灰底色（#EAE6D9）+ 灰褐色文字（#6E6757）的小圆角胶囊，不要任何彩色（不要紫、蓝、青、绿、橙——全部统一成这一种灰）。其中一条会话为选中态，淡色底 + 细绿左边框。

右侧主区：顶部细条左侧是会话标题（旁边一个小铅笔图标），右侧一个小号等宽字体胶囊显示用量「¥0.128 · 12.4k tok」；中部是大面积空态，垂直居中——一个绿色像素风熊猫图标、「你好！我是 OpenPanda」大标题、一行灰色说明文字、三个白底细描边的胶囊建议按钮（各带一个小图标与右箭头）；最底部是输入区：大号圆角输入框（白色底、1px 浅灰描边、占位文字「请输入你的问题，或描述你想要完成的任务…」），框内底部一行左侧是「上下文」下拉（带小图标）、中间是「联网搜索」复选框、右侧是「发送」按钮——发送按钮同样用淡绿色底 + 深绿色文字 + 细绿描边，不要实心深色块、不要白色文字。输入框下方还有两行极小的灰色说明文字。

设计语言：扁平、无渐变、1px 细边框（#DDD6C4）、极轻阴影、8–14px 圆角、留白充足；配色只有暖纸中性色 + 一种低饱和绿，严禁出现第二种彩色；现代几何无衬线字体，层次清晰，13–15px 正文，标题中等字重；等宽字体只用于数字。整体气质：安静、专业、不拥挤、信息清晰。

避免：深色主题、霓虹或高饱和色、深绿或任何深色的实心按钮块、彩色标签、渐变、玻璃拟态、3D 或等距透视、设备外框、人物与手、装饰性插画、彩色投影。
```

### 5.2 v2 英文 Prompt

```
A clean flat UI design mockup of a desktop web application called "OpenPanda", a
local-first multi-agent orchestration console. Straight-on screenshot, no device
frame, no perspective, 16:10 aspect ratio. Extremely restrained palette: warm paper
neutrals plus ONE low-saturation green as the only accent — no other hue anywhere.

THREE-COLUMN LAYOUT on a warm off-white background (#F5F3EC):

Left sidebar, about 190px, slightly darker warm grey (#EDE9DD): a green panda logo
with the wordmark "OpenPanda" at the top; a rounded search field carrying a small
keyboard-shortcut hint; three labelled navigation groups, each with a tiny grey
caption and a few nav items; the active item is marked with a very pale green wash and
a thin green bar on its left edge; at the bottom a compact node status row (a small
green dot, a node name, a tiny version label) and two utility links.

Middle session rail, about 250px, same warm grey: a compact search input at the top;
below it a full-width "+ New chat" button rendered as a SOFT TONAL button — pale green
wash background, deep green text, 1px hairline green border; explicitly NOT a solid
dark block and NOT white text. Then six conversation rows, each with a medium-weight
title, a small rounded project tag, and a right-aligned light-grey relative timestamp.
ALL SIX PROJECT TAGS MUST USE EXACTLY ONE NEUTRAL STYLE — warm light grey fill
(#EAE6D9) with muted brown-grey text (#6E6757) — no purple, blue, teal, green or
orange tags, one uniform grey for every tag. One row is selected, shown with a subtle
tinted background and a thin green left border.

Main pane: a slim top bar with the conversation title on the left next to a small
pencil icon, and on the right a small monospace chip showing a cost figure and a token
count; below it a large empty state, vertically centred — a green pixel-art panda mark,
a large friendly greeting heading, one grey subtitle line, and three white pill-shaped
suggestion chips with hairline borders, each carrying a small icon and a right arrow;
at the very bottom a composer — a large rounded white input surface with a 1px
light-grey border and a placeholder line; inside it a bottom row with a "context"
dropdown on the left, a labelled checkbox in the middle, and a "Send" button on the
right rendered the SAME WAY as the New chat button: pale green wash background, deep
green text, hairline green border — not a solid dark block, not white text. Two lines
of very small grey helper text sit below the input.

DESIGN LANGUAGE: flat, no gradients, 1px hairline borders (#DDD6C4), very soft shadows,
8-14px rounded corners, generous whitespace. Palette limited to warm paper neutrals
plus one low-saturation green; NO second hue anywhere. Typography: modern geometric
sans-serif, clear hierarchy, 13-15px body, medium weight titles; monospace only for
numbers. Mood: calm, professional, uncluttered.

AVOID: dark theme, neon or saturated colours, solid dark-green or any dark filled
button blocks, coloured tags, gradients, glassmorphism, isometric perspective, device
frames, people, hands, decorative illustration, coloured drop shadows.
```

### 5.3 v2 新增验收项

- [ ] 6 个项目标签**颜色完全一致**（同一种中性灰），没有任何一个标签是彩色
- [ ] 「新对话」和「发送」都不是实心深色块、没有白色文字（应为淡底 + 深色字 + 细描边）
- [ ] 全图只有一种强调色（低饱和绿），找不到第二种色相

---

## 6. 实施映射（图满意后按此执行）

**属于本页 PR（`refactor/webui-chat`）**
- `views/sessions.tsx`：`ChatEmpty` 改为「顶部细横幅 + 居中 hero」；`thread-tools` 空列表时隐藏管理按钮；`thread-list` 空态加主按钮；标题重复问题
- 页面私有类：`chat-empty*` / `hero-chips` / `thread-*` / `composer*` 在 `styles/chat.css` 与 `styles/views.css` 中的对应规则

**属于共享层（单独 PR，本页只记录）**
- 用量 chip 的常驻标签需要 badge 口径统一 → `refactor/webui-shared-badges`
- 两个下拉合并为「上下文」按钮组需要浮层原语 → `refactor/webui-shared-overlay`
- 会话行相对时间需要统一时间口径 → `refactor/webui-shared-time`
- `chat-empty*` / `composer*` 的部分规则与 `views.css` 重复 → `refactor/webui-shared-primitives`
