package engine

import (
	"argo/pkg/conf"
	"argo/pkg/inject"
	"argo/pkg/log"
	"argo/pkg/login"
	"argo/pkg/static/files"
	"argo/pkg/utils"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

type EngineInfo struct {
	Browser    *rod.Browser
	Options    conf.BrowserConf
	Launcher   *launcher.Launcher
	CloseChan  chan int
	Target     string
	Host       string
	HostName   string
	TabCount   int
	KnownFiles *files.KnownFiles // 从 robots.txt|sitemap.xml 获取路径
}

type UrlInfo struct {
	Url        string
	SourceType string
	Match      string
	SourceUrl  string
}

// var EngineInfoData *EngineInfo

func InitEngine(target string) *EngineInfo {
	// 初始化 js注入插件
	inject.LoadScript()
	// 初始化 登录插件
	login.InitLoginAuto()
	// 初始化 泛化模块
	InitNormalize()
	// 初始化 结果处理模块
	InitResultHandler()
	// 初始化tab控制携程池
	InitTabPool()
	// 初始化静态资源过滤
	InitFilter()
	// 初始化浏览器
	engineInfo := InitBrowser(target)
	// 初始化 urls队列 tab新建
	engineInfo.InitController()
	return engineInfo
}

func InitBrowser(target string) *EngineInfo {
	// 初始化
	browser := rod.New().Timeout(time.Duration(conf.GlobalConfig.BrowserConf.BrowserTimeout) * time.Second)
	// 启动无痕
	if conf.GlobalConfig.BrowserConf.Trace {
		browser = browser.Trace(true)
	}
	// options := launcher.New().Devtools(true)
	//  NoSandbox fix linux下root运行报错的问题
	options := launcher.New().NoSandbox(true).Headless(true)
	// 禁用所有提示防止阻塞 浏览器
	options = options.Append("disable-infobars", "")
	options = options.Append("disable-extensions", "")

	if conf.GlobalConfig.BrowserConf.UnHeadless || conf.GlobalConfig.Dev {
		options = options.Delete("--headless")
		browser = browser.SlowMotion(time.Duration(conf.GlobalConfig.AutoConf.Slow) * time.Second)
	}
	browser = browser.ControlURL(options.MustLaunch()).MustConnect().NoDefaultDevice().MustIncognito()
	closeChan := make(chan int, 1)
	u, _ := url.Parse(target)

	var knownFiles *files.KnownFiles
	httpclient, err := utils.BuildHttpClient(conf.GlobalConfig.BrowserConf.Proxy, nil)
	if err != nil {
		log.Logger.Errorln("ould not create http client")

	} else {
		knownFiles = files.New(httpclient)
	}

	return &EngineInfo{
		Browser:    browser,
		Options:    conf.GlobalConfig.BrowserConf,
		Launcher:   options,
		CloseChan:  closeChan,
		Target:     target,
		Host:       u.Host,
		HostName:   u.Hostname(),
		KnownFiles: knownFiles,
	}
}

func (ei *EngineInfo) Start() {
	log.Logger.Debugf("tab timeout: %d", conf.GlobalConfig.BrowserConf.TabTimeout)
	log.Logger.Debugf("browser timeout: %d", conf.GlobalConfig.BrowserConf.BrowserTimeout)
	// hook 请求响应获取所有异步请求
	router := ei.Browser.HijackRequests()
	defer router.Stop()
	router.MustAdd("*", func(ctx *rod.Hijack) {
		// 用于屏蔽某些请求 img、font
		// *.woff2 字体
		if ctx.Request.Type() == proto.NetworkResourceTypeFont {
			ctx.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			return
		}
		// 图片
		if ctx.Request.Type() == proto.NetworkResourceTypeImage {
			ctx.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			return
		}

		// 防止空指针
		if ctx.Request.Req() != nil && ctx.Request.Req().URL != nil {
			// 优化, 先判断,再组合
			if strings.Contains(ctx.Request.URL().String(), ei.HostName) {
				if ctx.Response.Payload().ResponseCode == 404 {
					return
				}

				var reqStr []byte
				fmt.Println(ctx.Request.Req().URL)
				reqStr, _ = httputil.DumpRequest(ctx.Request.Req(), true)
				save, body, _ := copyBody(ctx.Request.Req().Body)
				saveStr, _ := ioutil.ReadAll(save)
				ctx.Request.Req().Body = body
				ctx.LoadResponse(http.DefaultClient, true)

				pu := &PendingUrl{
					URL:             ctx.Request.URL().String(),
					Method:          ctx.Request.Method(),
					Host:            ctx.Request.Req().Host,
					Headers:         ctx.Request.Req().Header,
					Data:            string(saveStr),
					ResponseHeaders: transformHttpHeaders(ctx.Response.Payload().ResponseHeaders),
					ResponseBody:    utils.EncodeBase64(ctx.Response.Payload().Body),
					RequestStr:      utils.EncodeBase64(reqStr),
					Status:          ctx.Response.Payload().ResponseCode,
				}
				pushpendingNormalizeQueue(pu)
			}
		}

	})
	go router.Run()

	if ei.KnownFiles != nil {
		knownFiles, err := ei.KnownFiles.Request(ei.Target)
		if err != nil {
			log.Logger.Errorf("Could not parse known files for %s: %s\n", ei.Target, err)
		}

		for _, staticUrl := range knownFiles {
			fmt.Println(staticUrl)
			PushUrlWg.Add(1)
			go func(staticUrl string) {
				defer PushUrlWg.Done()
				PushStaticUrl(&UrlInfo{Url: staticUrl, SourceType: "static parse", SourceUrl: "robots.txt|sitemap.xml"})
			}(staticUrl)
		}
	}

	// 打开第一个tab页面 这里应该提交url管道任务
	ei.NewTab(&UrlInfo{Url: ei.Target}, 0)
	// 结束
	// 0. 首页解析完成
	// 1. url管道没有数据
	// 2. 携程池任务完成
	// 3. 没有tab页面存在
	if conf.GlobalConfig.Dev {
		log.Logger.Warn("!!! dev mode please ctrl +c kill !!!")
		select {}
	}
	<-ei.CloseChan
	log.Logger.Debug("front page over")
	urlsQueueEmpty()
	log.Logger.Debug("urlsQueueEmpty over")
	TabWg.Wait()
	TabPool.Release()
	log.Logger.Debug("tabPool over")
	if ei.Browser != nil {
		closeErr := ei.Browser.Close()
		if closeErr != nil {
			log.Logger.Errorf("browser close err: %s", closeErr)

		} else {
			log.Logger.Debug("browser close over")
		}
	}
	CloseNormalizeQueue()
	PendingNormalizeQueueEmpty()
	log.Logger.Debug("pendingNormalizeQueueEmpty over")
	ei.Launcher.Kill()
	ei.SaveResult()
}

func copyBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func transformHttpHeaders(rspHeaders []*proto.FetchHeaderEntry) http.Header {
	newRspHeaders := http.Header{}
	for _, data := range rspHeaders {
		newRspHeaders.Add(data.Name, data.Value)
	}
	return newRspHeaders
}