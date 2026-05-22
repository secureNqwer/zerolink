//go:build linux

package gui

import (
	"context"
	"net/http"
	"os"
	"time"

	webview "github.com/webview/webview_go"

	"github.com/secureNqwer/zerolink/messenger"
	"github.com/secureNqwer/zerolink/version"
)

func Run(engine *messenger.Engine, _ interface{}) {
	webui := messenger.NewWebUI(engine)
	addr := ":8081"
	srv := &http.Server{Addr: addr, Handler: webui.Handler()}
	go srv.ListenAndServe()

	time.Sleep(200 * time.Millisecond)

	w := webview.New(true)
	defer w.Destroy()
	title := version.Name
	if sr := engine.ServerRelay(); sr != nil && sr.Connected() {
		title = version.Name + " - " + sr.Username()
	}
	w.SetTitle(title)
	w.SetSize(1000, 650, webview.HintNone)
	w.Navigate("http://localhost" + addr)
	w.Run()

	srv.Shutdown(context.Background())
	engine.Stop()
	os.Exit(0)
}
