# 适用于 go-zero 的日志模块

基于 zap 的生产级日志组件，文件输出带缓冲、自动轮转，并通过
`fdatasync` + `posix_fadvise(POSIX_FADV_DONTNEED)` 周期性地把日志文件的
页缓存从内核 page cache 中回收，避免大日志量把进程 cgroup 内存顶高
（典型症状：`systemctl status` 里 Memory 显示几百 MB，但 `ps` 的 RSS
只有十几 MB，多出来的全是日志写入产生的 page cache）。

## 安装

    github.com/hide-in-code/zlog

## 使用

    package handler

    import (
        "net/http"

        "demo/internal/logic"
        "demo/internal/svc"
        "demo/internal/types"
        "github.com/zeromicro/go-zero/rest/httpx"
    )

    var takeoverLogger *logger.Logger

    func DemoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
            var req types.Request
            if err := httpx.Parse(r, &req); err != nil {
                httpx.ErrorCtx(r.Context(), w, err)
                return
            }

            l := logic.NewdemoLogic(r.Context(), svcCtx)
            resp, err := l.demo(&req)
            if err != nil {
                httpx.ErrorCtx(r.Context(), w, err)
            } else {
                httpx.OkJsonCtx(r.Context(), w, resp)
            }
        }
    }

## 行为与配置

复用 go-zero 的 `logx.LogConf`，不需要新增任何配置项：

| 配置项 | 默认 | 说明 |
| --- | --- | --- |
| Mode | console | `file`/`volume` 走文件写入，否则输出到 stdout |
| Path | logs | 日志目录，实际写入 `<Path>/<name>/` 下 |
| Level | info | debug/info/warn/error；`severe`/`fatal` 按 error 处理（避免未 flush 就被 os.Exit） |
| Encoding | json | `json` 或 `plain` |
| KeepDays | 0（永久保留） | 按文件 mtime 清理过期日志 |
| Rotation | daily | `daily`=按小时轮转；`size`=按 MaxSize 轮转 |
| MaxSize | 0（size 模式默认 100MB） | 单文件大小上限，单位 MB |
| MaxBackups | 0（不限制） | size 模式下最多保留的轮转文件数 |

文件写入特性：

- 日志先进入 1MB 内存缓冲，后台每 100ms 刷盘一次（不丢日志：缓冲满时调用方阻塞等待）。
- 每 1 秒（或累计写入 64MB）执行一次 `fdatasync` + `posix_fadvise(FADV_DONTNEED)`，
  把已落盘的日志页从 page cache 逐出，cgroup 内存不会随日志量增长。
- 轮转时同样先同步并回收旧文件的页缓存，再切换新文件。
- 目录下始终维护 `<name>.log` 软链接指向当前文件，`tail -f` 可直接使用。
- 并发安全，任意多个 goroutine 可同时调用；`CloseAll()` 负责全部 flush 后关闭。

> 说明：非 Linux（或非 amd64/arm64）平台退化为每次 `fsync`，日志同样可靠，
> 只是不做页缓存回收。

## 快速验证

写 1GB 日志对比 page cache 增量（本机 kernel 5.15 实测）：

    # 普通写（fsync 一次）: Cached +1024MB  ← 问题所在
    # fadvise 周期回收:     Cached ~0MB     ← 修复后

## 测试

    go test -race ./...
