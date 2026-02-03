# 适用于go-zero的日志模块

# 使用方法
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
