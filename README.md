![limit — 通用程序资源限制器](docs/banner.png)

**通用程序资源限制器 — 进程树级 CPU / 内存 / 磁盘 IO / 进程数硬限制**

[![Platform](https://img.shields.io/badge/platform-Windows%20%C2%B7%20Linux%20%C2%B7%20macOS-0078D6)](#)
[![Go](https://img.shields.io/badge/Go-stdlib%20only-00ADD8?logo=go&logoColor=white)](#)
[![Deps](https://img.shields.io/badge/deps-0-brightgreen)](#)

`limit [选项] -- <命令> [参数...]` 把命令及其**所有子进程**整体关进一个资源容器,
像 Docker 一样限制 CPU / 内存 / 磁盘 IO / 进程数。Go 实现,零依赖单文件二进制。

发布件覆盖 **Windows / Linux / macOS** 全平台(amd64/arm64,见
[Releases](https://github.com/Weidows/limit/releases))。

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

:: HDD 上批量小文件拷贝/解压,用 IOPS 顶摁住寻道风暴(带宽顶管不住这种负载)
limit --iops 60 -- xcopy S:\ D:\ /E

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
| `--io <MB/s>` | 磁盘 IO 速率上限(读写合计,**整 Job 聚合口径**) |
| `--iops <次数/s>` | IO 每秒操作数上限(读写合计,可与 `--io` 并用;HDD 小文件场景用) |
| `--procs <个数>` | 进程数上限(含子进程树) |
| `--affinity <掩码>` | CPU 亲和性(`0x0F` 或 `0,2-5`) |
| `--prio <级别>` | `idle\|below\|normal\|above\|high` |
| `--no-kill-on-close` | limit 被杀时不拉子进程陪葬(默认陪葬) |

退出时打印:退出码、CPU 时间(user+kernel)、峰值 commit(单进程/整棵树)。

## 安装

```console
go install github.com/Weidows/limit@latest
go build -o limit .
```

## 实测验证(Windows 后端)

> i5-14600KF 14C/20T,Windows 11,2026-10。**全部数字实测,不抄文档。**
> 选项语义按「全平台一致」设计;下表是 Windows 后端(Job Object)的落实情况。

| 限制 | 测试 | 结果 |
|---|---|---|
| `--cpu` | burn 4 线程,5%/10%/25% 三档 | 5%≈1 核、10%≈2 核精确;上限高于线程需求时不触顶 ✅ |
| `--mem` | alloc 分配 1024MB | 254MB 处分配失败,错误码 1455 `ERROR_COMMITMENT_LIMIT` ✅ |
| `--io` | diotest 直写拷贝 | 读写合计精确压在档位上 ✅ |
| `--iops` | diotest 写穿透小 IO | 压到 ≈100 操作/s,create/close 元数据也计数 ✅ |
| `--affinity 0x1` | burn 2 线程 | CPU 5.55s(绑 1 核)✅ |
| `--procs 1` | cmd 里再起 cmd | 孙进程创建被拒 ✅ |
| × **7z** | 2GB 包解压 7.5GB 到 HDD | `--cpu` 当节流阀:小包线性减速;大包把 61MB/s 回写爆发削到 13MB/s ✅ |
| × **robocopy** | 17000 文件/660MB 拷贝 | 四类顶全压得住:`--iops 30` 减速 15 倍;小数据量时 `--io`/`--iops` 无感(全进缓存)✅ |

按程序归档的完整数据(负载、档位、耗时、曲线结论)在
[docs/MEASURED.md](docs/MEASURED.md);每实测一个新程序就往那里追加一节。

## 语义要点(必读)

1. **`--cpu` 相对整机算力**:100% = 全部逻辑 CPU(20 线程机上 5% = 1 核)。
   上限高于线程需求时不触顶。
2. **`--mem` 是 commit 口径硬顶**:撞顶时分配直接失败(错误码 1455),类似 cgroup 的
   `memory.max`;RSS(工作集)只有软限,这是 Windows 语义。
3. **`--io` 是整 Job 读写合计、聚合配额**(多盘并行共享一个额度,与微软文档「按盘独立」不符,以实测为准)。
   只对直写/写穿透 IO 精确生效——缓存写程序退出后由系统全速回写,**会逃过限制**。
4. **缓存写限不住?`--cpu` 当节流阀**:CPU 顶对所有 IO 类型生效。吃 CPU 的负载
   (压缩/解压/转码)被压住后产出速率同步下降;实测 IO 速率 ≈ min(磁盘自然速率, CPU 顶折算速率)。
5. **`--iops` 管小文件/HDD 场景**:带宽顶管不住随机寻道风暴,IOPS 顶直接限操作数
   (create/close 元数据也计数)。可与 `--io` 并用,谁先撞顶谁生效。
6. **整棵进程树生效**:挂起创建 → 入 Job → 恢复,开头几毫秒也不漏。
   limit 自身不能再进别的 Job/容器(嵌套限制),会明确报错。

---

## 相关

- `skills/using-limit-cli/SKILL.md` — agent 可直接引用的使用技能(选项语义 + 常用配方)
