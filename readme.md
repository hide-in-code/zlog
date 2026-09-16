# 适用于 go-zero 的日志模块

基于 zap 的生产级日志组件，文件输出带缓冲、自动轮转，并通过
`fdatasync` + `posix_fadvise(POSIX_FADV_DONTNEED)` 周期性地把日志文件的
页缓存从内核 page cache 中回收，避免大日志量把进程 cgroup 内存顶高
（典型症状：`systemctl status` 里 Memory 显示几百 MB，但 `ps` 的 RSS
只有十几 MB，多出来的全是日志写入产生的 page cache）。

## 安装

    go get github.com/hide-in-code/zlog

## 使用

从 go-zero 的配置里拿 `logx.LogConf`（`rest.RestConf`/`zrpc.RpcConf` 都内嵌了
`service.ServiceConf`，日志配置字段是 `Log`），按名字创建 logger：

    // etc/app.yaml
    // Name: demo
    // Log:
    //   Mode: file
    //   Path: logs
    //   Level: info
    //   Encoding: json
    //   Rotation: daily     # 本库把 daily 实现为按小时切分
    //   KeepDays: 0         # 不按天数清理
    //   MaxBackups: 8       # 整个目录最多保留 8 个日志文件（含正在写的那个）

    package main

    import (
        "github.com/hide-in-code/zlog"
        "github.com/zeromicro/go-zero/core/conf"
        "github.com/zeromicro/go-zero/rest"
        "go.uber.org/zap"
    )

    type Config struct {
        rest.RestConf
    }

    func main() {
        var c Config
        conf.MustLoad("etc/app.yaml", &c)

        // 名字决定子目录与文件前缀：logs/demo/demo-2026-09-16-00.log
        logger := zlog.NewWithConf("demo", c.Log)
        defer zlog.CloseAll() // 退出前把所有缓冲刷盘

        logger.Info("service started", zap.String("addr", c.Host))
        logger.Error("upstream failed",
            zap.String("peer", "10.0.0.1"),
            zap.Int("retry", 3),
        )
    }

业务代码里同样按名字取用即可（同名 logger 是同一个实例，不需要到处传指针）：

    func (l *ReceiveLogic) Receive(req *types.Request) error {
        logger := zlog.NewWithConf("demo", l.svcCtx.Config.Log)
        logger.Info("received", zap.String("vin", req.Vin))
        return nil
    }

## 行为与配置

复用 go-zero 的 `logx.LogConf`，不需要新增任何配置项：

| 配置项 | 默认 | 说明 |
| --- | --- | --- |
| Mode | console | `file`/`volume` 走文件写入，否则输出到 stdout |
| Path | logs | 日志目录，实际写入 `<Path>/<name>/` 下 |
| Level | info | 不低于该级别的日志才写：debug 全写，info 还会写 warn/error，error 只写 error；`severe`/`fatal` 按 error 处理（避免未 flush 就被 os.Exit） |
| Encoding | json | `json` 或 `plain` |
| KeepDays | 0（永久保留） | 按文件 mtime 清理超过 N 天的日志 |
| Rotation | daily | `daily`=按小时轮转；`size`=按 MaxSize 轮转 |
| MaxSize | 0（size 模式默认 100MB） | 单文件大小上限，单位 MB |
| MaxBackups | 0（不限制） | **日志目录最多保留多少个日志文件**（含正在写的那个），两种轮转模式都生效 |

保留策略（受限设备主要靠这里控制占用）：

- `MaxBackups=N`（N>0）表示目录里最多留 N 个日志文件，即最近的 N 个时间片。
  按小时轮转且 `N=8` 时，目录里始终是「当前小时 + 前 7 个小时」≈ 8 小时窗口。
- **正在写的那个文件永远不删**，所以 `N=1` 表示只留当前小时。
- `KeepDays` 与 `MaxBackups` 相互独立、谁先命中谁删（与 go-zero 的说明一致）。
  `MaxBackups=0` 且 `KeepDays=0` 时永不删除，日志会一直涨，受限设备务必至少设一个。
- 清理在启动时、每次轮转时、以及每小时巡检时执行；不再需要外部 cron 删文件
  （外部删除可能删到进程正持有的 inode，数据会写进已 unlink 的文件里而「消失」）。
- 每次删除会在 stderr 打印一行 `[zlog] <name>: retained N log files, removed M`，
  便于确认策略是否真的生效。

> 与 go-zero 的差异：`logx` 只在 `size` 模式使用 `MaxBackups`，且文件按
> `access/error/severe/slow/stat` 分类；本库把它统一成「目录内日志文件数上限」，
> 并且在按小时轮转下也生效，文件名保持 `<name>-YYYY-MM-DD-HH.log` 可读。

文件写入特性：

- 日志先进入 1MB 内存缓冲，后台每 100ms 刷盘一次；缓冲写满时先尝试刷盘，
  刷不动（例如磁盘满）才把该条日志作为错误返回给 zap，不会无限占用内存。
- 每 1 秒（或累计写入 64MB）执行一次 `fdatasync` + `posix_fadvise(FADV_DONTNEED)`，
  把已落盘的日志页从 page cache 逐出，cgroup 内存不会随日志量增长。
- 轮转时同样先同步并回收旧文件的页缓存，再切换新文件。
- 目录下始终维护 `<name>.log` 软链接指向当前文件（绝对路径），`tail -f` 可直接使用。
- 并发安全，任意多个 goroutine 可同时调用；`CloseAll()` 会把所有 logger 刷盘
  并清空缓存，进程退出前调用一次即可。

轮转语义：

- 按小时文件命名 `<name>-YYYY-MM-DD-HH.log`，边界就是本机时钟的整点：
  `HH:00:00` 那一刻写入的日志属于 `HH` 这个文件。
- 是否轮转取决于「当前文件所在的小时」与「时钟当前小时」是否一致，而不是
  「下一次轮转时间」这种期限值。因此时钟被 NTP 前后步进（无 RTC 的板子很常见）
  也不会把日志写进名字与时间不符的文件里。
- 轮转失败（目录被删、磁盘满）不会丢日志：继续写当前文件，并在下一次写入时重试轮转。

## 排查「日志变少了 / 内容截断」

写入失败（磁盘满、I/O 错误）以前会被静默吞掉：缓冲里的日志被丢弃，只在 stderr
打印一行提示。现在：

- 后台刷盘 / fsync / 轮转失败会打印限流后的 `[zlog] <name>: <err>` 到 stderr
  （默认每分钟最多一条，避免刷屏，同时不会只报一次就沉默）。
- 已经写进缓冲但没写成功的字节会保留到下一次刷盘重试，不再直接丢弃。
- zap 自己也会把每次写入错误输出到它的 ErrorOutput，所以应用侧 stderr/journal
  里出现 `write error` 通常意味着磁盘或权限问题。

怀疑日志缺失时按顺序检查：

1. `df -h`：磁盘是否写满。`KeepDays=0` 且 `MaxBackups=0`（都是默认）表示**永不删除旧日志**，
   按 382MB/小时算一天就是 9GB，嵌入式设备很容易被写满，写满后日志会大量丢失。
   受限设备建议按排查窗口设 `MaxBackups`（例如要查最近 8 小时就设 8）。
2. 应用 stderr / journal 里是否有 `[zlog] ...` 或 zap 的 `write error`；
   正常工作时每小时会有一条 `retained N log files, removed M`。
3. 确认应用实际传入的 `logx.LogConf`（尤其 `Mode`、`Path`、`Level`、`Rotation`）：
   `size` 轮转的文件名带秒，其它值（含 `daily`）走按小时命名。
4. 用 `tail -f <name>.log` 看实时写入是否在继续；`ls -l --time-style=full-iso`
   看每个小时文件的 mtime 是否都落在该小时的第 59 分（说明每小时都在正常写入）。

> 说明：非 Linux（或非 amd64/arm64）平台退化为每次 `fsync`，日志同样可靠，
> 只是不做页缓存回收。

## 快速验证

写 1GB 日志对比 page cache 增量（本机 kernel 5.15 实测）：

    # 普通写（fsync 一次）: Cached +1024MB  ← 问题所在
    # fadvise 周期回收:     Cached ~0MB     ← 修复后

## 测试

    go test -race ./...
