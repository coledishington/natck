package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"testing"
)

func tPath(p ...string) string {
	p = append([]string{"testdata"}, p...)
	return path.Join(p...)
}

func cpFile(t *testing.T, sPath, dPath string) {
	src, err := os.Open(sPath)
	if err != nil {
		t.Fatal("cpFile failed to open src:", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
	if err != nil {
		t.Fatal("cpFile failed to open dst:", err)
	}
	defer dst.Close()

	buf := make([]byte, os.Getpagesize())
	for {
		n, err := src.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("cpFile failed read:", err)
		}
		if n == 0 {
			break
		}

		_, err = dst.Write(buf[:n])
		if err != nil {
			t.Fatal("cpFile failed write:", err)
		}
	}
}

func spawnHttpServer(t *testing.T, testdata string, port int) {
	dir := t.TempDir()
	t.Cleanup(func() {
		err := os.RemoveAll(dir)
		t.Errorf("Failed to cleanup root directory of http server on port %v: %v", port, err)
	})

	cpFile(t, testdata, path.Join(dir, "index.html"))
	srv := http.Server{
		Addr:    fmt.Sprint(":", port),
		Handler: http.FileServer(http.Dir(dir)),
	}
	t.Cleanup(func() {
		err := srv.Close()
		t.Errorf("Failed to close http server on port %v: %v", port, err)

	})

	go func() {
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Unexpected shutdown of http server on port %v: %v", port, err)
		}
	}()
}

func TestMeasureMaxConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnections in short mode due to re-request timeouts.")
	}

	testcases := map[string]struct {
		inUrls           []string
		outNConns        int
		outRequestedUrls []string
	}{
		"few servers": {
			inUrls: []string{
				"http://127.0.0.1:8081/index.html",
				"http://127.0.0.1:8082/index.html",
				"http://127.0.0.1:8083/index.html",
			},
			outNConns: 3,
		},
		"reachable and unreachable server": {
			inUrls: []string{
				"http://127.0.0.1:8081/index.html",
				"http://127.0.0.1:8089/index.html",
			},
			outNConns: 1,
		},
	}

	spawnHttpServer(t, tPath("no_links.html"), 8081)
	spawnHttpServer(t, tPath("no_links.html"), 8082)
	spawnHttpServer(t, tPath("no_links.html"), 8083)

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			urls := []url.URL{}
			for _, inUrl := range tc.inUrls {
				u, err := url.Parse(inUrl)
				if err != nil {
					t.Fatal("Test url failed to parse")
				}
				urls = append(urls, *u)
			}
			nConns := MeasureMaxConnections(urls)
			if nConns != tc.outNConns {
				t.Error("expected (nConns=", tc.outNConns, "), got (nConns=", nConns, ")")
			}
		})
	}
}
