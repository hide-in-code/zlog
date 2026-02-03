# 适用于go-zero的日志模块
受够了go-zero 框架中日志全挤在access.log中的情况，尤其是在使用go-zero开发单应用的时候，日志之间没有业务隔离，因此增加这个包用于日志管理

# 使用方法
## 安装

    go get github.com/hide-in-code/zlog

## 配置`etc/config.yaml`
命令行输出模式

    ...
    Log:
      ServiceName: demo
      Mode: console
      Encoding: json
      Rotation: daily
      Level: debug
      KeepDays: 7
    ...

文件保存模式

    ...
    Log:
      ServiceName: demo
      Mode: file
      Path: ./logs
      Encoding: json
      Rotation: daily
      Level: debug
      KeepDays: 7
    ...

## 使用
    
    package handler
    
    import (
    	"net/http"
    
    	"lean/demo/internal/logic"
    	"lean/demo/internal/svc"
    	"lean/demo/internal/types"
    
    	"github.com/hide-in-code/zlog"
    	"github.com/zeromicro/go-zero/rest/httpx"
    	"go.uber.org/zap"
    )
    
    var logger *zlog.Logger
    
    func DemoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
    	logger = zlog.NewWithConf("demo", svcCtx.Config.Log)
    	return func(w http.ResponseWriter, r *http.Request) {
    		var req types.Request
    		if err := httpx.Parse(r, &req); err != nil {
    			httpx.ErrorCtx(r.Context(), w, err)
    			return
    		}
    
    		logger.Info("这是一条info信息", zap.String("user", req.Name))
    
    		l := logic.NewDemoLogic(r.Context(), svcCtx)
    		resp, err := l.Demo(&req)
    		if err != nil {
    			httpx.ErrorCtx(r.Context(), w, err)
    		} else {
    			httpx.OkJsonCtx(r.Context(), w, resp)
    		}
    	}
    }

## 效果
- 日志是`console`输出模式时，encoding为`plain`时

        2026-02-03T14:32:24.593+08:00	 info 	[HTTP]  200  -  GET  /from/you - 127.0.0.1:47486 - curl/7.68.0	duration=0.3ms	caller=handler/loghandler.go:167	trace=38f0e92c59923eeef87b42aef18baedb	span=e47acd303e0a2da5
        2026-02-03T14:32:29.751+0800	INFO	handler/demohandler.go:26	这是一条info信息	{"module": "demo", "user": "you"}

- 日志是`console`输出模式时，encoding为`json`时

      {"level":"info","ts":"2026-02-03T14:38:42.799+0800","caller":"handler/demohandler.go:26","msg":"这是一条info信息","module":"demo","user":"me"}
      {"@timestamp":"2026-02-03T14:38:42.799+08:00","caller":"handler/loghandler.go:167","content":"[HTTP] 200 - GET /from/me - 127.0.0.1:46168 - curl/7.68.0","duration":"0.1ms","level":"info","span":"973d0cea6bd3c062","trace":"0904485c532220a67859061d6a4ac72a"}

- 当设置文件存储时

      logs
        ├── access.log
        ├── demo
        │   ├── demo-2026-02-03.log // 实时日志
        │   └── demo.log -> demo-2026-02-03.log
        ├── error.log // 框架自带日志
        ├── severe.log // 框架自带日志
        ├── slow.log // 框架自带日志
        └── stat.log // 框架自带日志



