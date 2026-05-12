package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hstin-de/wmtiles/cmd/wmtiles/web"
)

func runServe(c *serveCmd) error {
	path, err := filepath.Abs(c.File)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	addr := &c.Addr

	const noViewerMsg = "viewer not bundled in this build " +
		"(rebuild with `make`, or `go build -tags embed`)"

	mux := http.NewServeMux()
	mux.HandleFunc("/wmt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Range, Content-Length")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("/viewer.js", func(w http.ResponseWriter, r *http.Request) {
		if len(web.ViewerJS) == 0 {
			http.Error(w, noViewerMsg, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(web.ViewerJS)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if len(web.ViewerHTML) == 0 {
			http.Error(w, noViewerMsg+"; only /wmt is available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(web.ViewerHTML)
	})

	baseURL := serveBaseURL(*addr)
	ui.Banner("serve", path)
	ui.Section("Serving")
	ui.KV("data URL", ui.styled(baseURL+"/wmt", ansiBrCyan))
	if len(web.ViewerJS) == 0 {
		ui.KV("viewer", "not bundled")
		ui.KV("note", "/wmt works; /viewer.js and / return 404")
	} else {
		ui.KV("viewer URL", ui.styled(baseURL+"/", ansiBrCyan))
	}
	ui.KV("status", ui.styled("listening", ansiGreen, ansiBold))
	return http.ListenAndServe(*addr, mux)
}

func serveBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}
