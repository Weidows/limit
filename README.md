![limit — 通用程序资源限制器](docs/banner.png)

**通用程序资源限制器 — 进程树级 CPU / 内存 / 磁盘 IO / 进程数硬限制**

[![Platform](https://img.shields.io/badge/platform-Windows%20%C2%B7%20Linux%20%C2%B7%20macOS-0078D6)](#)
[![Go](https://img.shields.io/badge/Go-stdlib%20only-00ADD8?logo=go&logoColor=white)](#)
[![Deps](https://img.shields.io/badge/deps-0-brightgreen)](#)

`limit [选项] -- <命令> [参数...]` 把命令及其**所有子进程**整体关进一个资源容器,
像 Docker 一样限制 CPU / 内存 / 磁盘 IO / 进程数。Go 实现,零依赖单文件二进制。

## 平台支持

| 平台 | 状态 | 后端 |
|---|---|---|
| Windows | ✅ 全部选项可用,全部实测 | Job Object(Windows 原生进程组容器,Windows 容器本身就用它实现) |
| Linux / macOS | ⚠️ 规划中 | cgroup v2 / rlimit(尚未移植) |

非 Windows 平台的二进制可以正常解析选项、看 `--help`;真正执行到「施加限制」时
**明确报错退出,不会静默放行**——不会让你误以为限制生效了。移植前,
linux 下临时等价手段是 `systemd-run --scope -p CPUQuota=25% -- <命令>`。

## 快速上手

```console
:: 限速解压大压缩包,全程不卡机
limit --cpu 25% --mem 2G --io 50 -- 7z x big.7z -oD:\out

:: HDD 保护磁头: --iops 限「寻道频率」(设备层事务/s),src / dst 任一盘超了就压
:: 大文件顺序拷贝几乎不受影响,大量小文件被削得最狠
limit --iops 200 D: E: -- robocopy S:\ D:\backup /E /J

:: 超时收工(墙钟计时;到期整棵树被终止,退出码 124 同 coreutils timeout)
limit --timeout 2h -- robocopy S:\ D:\backup /E /J

:: 缓存写程序(7z 等)限不住回写?用 CPU 顶当 IO 节流阀——对所有 IO 都生效
limit --cpu 5% -- 7z x big.7z -oHDD:\out

:: 后台转码给前台让路
limit --prio idle --cpu 10% -- ffmpeg -i in.mp4 out.mkv

:: 防失控程序吃爆内存
limit --mem 1G --mem-total 4G -- some_tool
```

## 选项

| 选项 | 说明 |
|---|---|
| `--cpu <百分比>` | CPU 硬顶(调度器级强制执行) |
| `--mem <大小>` | 单进程 commit 内存硬顶(`512M`/`4G`) |
| `--mem-total <大小>` | 整棵树 commit 内存硬顶 |
| `--io <MB/s>` | 磁盘 IO 速率上限(读写合计,**整 Job 聚合口径**;仅对无缓冲/直写精确生效,缓冲写穿透——见要点 3) |
| `--iops <次数/s> [盘...]` | **限寻道频率**=物理盘设备层事务数/秒(免管理员)。不给盘符=所有固定卷,也可 `--iops 200 D: E:` 只盯这两盘,**任一盘超目标就压**(同一物理盘只算一次)。大文件几乎无感,大量小文件压得最狠——见要点 5 |
| `--procs <个数>` | 进程数上限(含子进程树) |
| `--affinity <掩码>` | CPU 亲和性(`0x0F` 或 `0,2-5`) |
| `--prio <级别>` | `idle\|below\|normal\|above\|high` |
| `--timeout <时长>` | 墙钟超时,到期 `TerminateJobObject` 整棵树并返回 **124**(`90s`/`10m`/`1h`,纯数字=秒) |
| `--no-kill-on-close` | limit 被杀时不拉子进程陪葬(默认陪葬) |

退出时打印:退出码、CPU 时间(user+kernel)、峰值 commit(单进程/整棵树)。

## 安装

```console
go install github.com/Weidows/limit@latest
go build -o limit .
```

## 实测验证(Windows 后端)

| 限制 | 测试 | 结果 |
|---|---|---|
| `--cpu` | burn 4 线程,5%/10%/25% 三档 | 5%≈1 核、10%≈2 核精确;上限高于线程需求时不触顶 ✅ |
| `--mem` | alloc 分配 1024MB | 254MB 处分配失败,错误码 1455 `ERROR_COMMITMENT_LIMIT` ✅ |
| `--io` | diotest 直写拷贝 | 读写合计精确压在档位上 ✅ |
| `--iops`(设备层) | 4000×64KB 小文件 vs 2GB 单文件,D:→E:,robocopy `/J` | 200 档:小文件 ×3.6、大文件**无影响**;100 档:×6.9 / ×1.7;实测压在 189/s、97/s ✅ |
| `--timeout 3s` | `cmd /c ping -n 30` | 3s 后整棵树被终止,无残留进程,退出码 124 ✅ |
| `--affinity 0x1` | burn 2 线程 | CPU 5.55s(绑 1 核)✅ |
| `--procs 1` | cmd 里再起 cmd | 孙进程创建被拒 ✅ |
| × **7z** | 2GB 包解压 7.5GB 到 HDD | `--cpu` 当节流阀:小包线性减速;大包把 61MB/s 回写爆发削到 13MB/s ✅ |
| × **robocopy** | 17000 文件/660MB 拷贝 | 缓冲写数据侧穿透 `--io`(814 MB/s 无感);加 `/J` 走无缓冲后 `--io` 精确生效 ✅ |

实际压制IO后的效果图:

![1792912155368](image/README/1792912155368.png)

按程序归档的完整数据(负载、档位、耗时、曲线结论)在
[docs/MEASURED.md](docs/MEASURED.md);每实测一个新程序就往那里追加一节。

## 语义要点(必读)

1. **`--cpu` 相对整机算力**:100% = 全部逻辑 CPU(20 线程机上 5% = 1 核)。
   上限高于线程需求时不触顶。
2. **`--mem` 是 commit 口径硬顶**:撞顶时分配直接失败(错误码 1455),类似 cgroup 的
   `memory.max`;RSS(工作集)只有软限,这是 Windows 语义。
3. **`--io` 是整 Job 读写合计、聚合配额**(多盘并行共享一个额度,与微软文档「按盘独立」不符,以实测为准)。
   只对无缓冲/直写/写穿透 IO 精确生效——**缓冲写程序(xcopy/robocopy 默认模式、7z 等)的数据写完全穿透**:
   `WriteFile` 只进系统缓存即返回,真正落盘的 IO 由内核回写器在 Job 之外全速发出(实测 `--io 30` 下
   robocopy 814 MB/s、xcopy 506 MB/s,与不限速无异)。解法:robocopy 加 `/J` 走无缓冲,`--io` 立即精确生效
   (实测 `/J --io 50` → 37 MB/s);xcopy 无此模式,换 robocopy `/J` 或用 `--cpu` 节流阀。
4. **缓存写限不住?`--cpu` 当节流阀**:CPU 顶对所有 IO 类型生效。吃 CPU 的负载
   (压缩/解压/转码)被压住后产出速率同步下降;实测 IO 速率 ≈ min(磁盘自然速率, CPU 顶折算速率)。
5. **`--iops` 限的是「设备层事务数/秒」= HDD 寻道频率(这是 HDD 的唯一正解)**:顺序 1MB
   只算 1 次事务(≈100MB/s 也只有 100 事务/s,几乎无寻道),随机 4KB 每 4KB 就 1 次寻道
   (同样字节量事务数高 256 倍)。所以同一档位下**大文件几乎无感、大量小文件被削得最狠**,
   吞吐天花板 ≈ 目标 × 平均 IO 大小(200/s ⇒ 4KB 随机 0.8MB/s、1MB 顺序 200MB/s)。
   不给盘符就盯**所有固定卷**,也可 `--iops 200 D: E:` 只盯这两盘;**任一盘超目标就冻结整棵树**
   (同物理盘多盘符只算一次)。控制精度约 ±5%(目标 200 → 189,目标 100 → 97)。
   实测:4000 小文件 200 档 ×3.6、100 档 ×6.9;2GB 大文件 200 档**零影响**、100 档 ×1.7(见 MEASURED.md)。
   目标值这么选:先 `--iops 1000000 D:` 跑一遍,日志里的「均值」就是该负载的自然事务率,
   取 1/2~1/3 即可。注意计数器是**整盘**口径(同盘其他程序也计入),这正对应"保交互"语义;
   背景负载本身高于目标时会被饿住(占空比拉满 + 告警一次)。
6. **整棵进程树生效**:挂起创建 → 入 Job → 恢复,开头几毫秒也不漏。
   limit 自身不能再进别的 Job/容器(嵌套限制),会明确报错。

---

## 相关

- `skills/using-limit-cli/SKILL.md` — agent 可直接引用的使用技能(选项语义 + 常用配方)
