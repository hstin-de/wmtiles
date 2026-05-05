package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hstin-de/wmtiles/cmd/wmtiles/web"
)

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: wmtiles serve [--addr :8080] <file.wmt>")
	}
	path, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}

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
	cliSection("WMTiles serve")
	cliKV("file", path)
	cliKV("data URL", baseURL+"/wmt")
	if len(web.ViewerJS) == 0 {
		cliKV("viewer", "not bundled")
		cliKV("note", "/wmt works; /viewer.js and / return 404")
	} else {
		cliKV("viewer URL", baseURL+"/")
	}
	cliKV("status", "listening")
	return http.ListenAndServe(*addr, mux)
}

func serveBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}
