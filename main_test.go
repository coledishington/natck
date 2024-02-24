package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"testing"
	"time"
)

type httpServerStats struct {
	m           sync.Mutex
	connections int
}

type httpTestServer struct {
	server       *http.Server
	testdata     string
	port         int
	replyLatency time.Duration
	stats        httpServerStats
}

type HandlerWrapper struct {
	latency time.Duration
	wrapped http.Handler
}

func (h HandlerWrapper) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	if h.latency != 0 {
		time.Sleep(h.latency)
	}
	h.wrapped.ServeHTTP(res, req)
}

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

// startHttpServer Starts an http server, raising a test error if
// any issue occurs. This server is setup over localhost, so in
// effect the client and server are one hop from each other, with
// no NAT between them. This can provide test coverage for
// simple cases whilst avoiding platform-specific
// network infrastructure.
func startHttpServer(t *testing.T, tSrv *httpTestServer) {
	dir := t.TempDir()
	t.Cleanup(func() {
		err := os.RemoveAll(dir)
		if err != nil {
			t.Errorf("Failed to cleanup root directory of http server on port %v: %v", tSrv.port, err)
		}
	})

	cpFile(t, tSrv.testdata, path.Join(dir, "index.html"))

	stats := &tSrv.stats
	statsCb := func(c net.Conn, s http.ConnState) {
		stats.m.Lock()
		defer stats.m.Unlock()
		if s == http.StateNew {
			stats.connections++
		}
	}

	handler := http.FileServer(http.Dir(dir))
	tSrv.server = &http.Server{
		Addr:      fmt.Sprint(":", tSrv.port),
		ConnState: statsCb,
		Handler: HandlerWrapper{
			latency: tSrv.replyLatency,
			wrapped: handler,
		},
	}
	t.Cleanup(func() {
		err := tSrv.server.Close()
		if err != nil {
			t.Errorf("Failed to close http server on port %v: %v", tSrv.port, err)
		}
	})

	go func() {
		err := tSrv.server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Unexpected shutdown of http server on port %v: %v", tSrv.port, err)
		}
	}()
}

func TestMeasureMaxConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnections in short mode due to re-request timeouts.")
	}

	testcases := map[string]struct {
		inPorts          []int
		outNConns        int
		outRequestedUrls []string
	}{
		"few servers": {
			inPorts:   []int{8081, 8082, 8083},
			outNConns: 3,
		},
		"repeat servers": {
			inPorts:   []int{8081, 8081, 8082},
			outNConns: 3,
		},
		"reachable and unreachable servers": {
			inPorts:   []int{8081, 8089, 8090, 8082, 8091},
			outNConns: 2,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			urls := make([]url.URL, 0)
			for _, port := range tc.inPorts {
				u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%v/index.html", port))
				if err != nil {
					t.Fatal("Test url failed to parse")
				}
				urls = append(urls, *u)
			}

			portToNConns := make(map[int]int, 0)
			for _, port := range tc.inPorts {
				portToNConns[port]++
			}

			httpServers := []*httpTestServer{
				{testdata: tPath("no_links.html"), port: 8081},
				{testdata: tPath("no_links.html"), port: 8082},
				{testdata: tPath("no_links.html"), port: 8083},
			}
			for i := range httpServers {
				startHttpServer(t, httpServers[i])
			}

			nConns := MeasureMaxConnections(urls)
			if nConns != tc.outNConns {
				t.Errorf("expected to measure %d client connections, got %d", tc.outNConns, nConns)
			}

			for i := range httpServers {
				s := httpServers[i]
				s.stats.m.Lock()
				if s.stats.connections != portToNConns[s.port] {
					t.Errorf("expected server on port %d to get %d connections, got %d", s.port, portToNConns[s.port], s.stats.connections)
				}
				s.stats.m.Unlock()
			}

			totalConnections := 0
			for i := range httpServers {
				s := &httpServers[i].stats
				s.m.Lock()
				totalConnections += s.connections
				s.m.Unlock()
			}
			if totalConnections != tc.outNConns {
				t.Errorf("expected the total number of new http server connections to be %d, got %d", tc.outNConns, totalConnections)
			}
		})
	}
}

func TestMeasureMaxConnectionsBig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnections in short mode due to re-request timeouts.")
	}

	nConnections := 1000
	httpSrvs := []*httpTestServer{}
	for i := 8000; i < 8000+nConnections; i++ {
		srv := &httpTestServer{testdata: tPath("no_links.html"), port: i, replyLatency: time.Millisecond}
		startHttpServer(t, srv)
		httpSrvs = append(httpSrvs, srv)
	}

	urls := []url.URL{}
	for i := 8000; i < 8000+nConnections; i++ {
		ln := fmt.Sprintf("http://localhost:%v/index.html", i)
		u, err := url.Parse(ln)
		if err != nil {
			t.Fatal("Test url failed to parse", ln)
		}
		urls = append(urls, *u)
	}

	nConns := MeasureMaxConnections(urls)
	if nConns != nConnections {
		t.Error("expected (nConns=", 10, "), got (nConns=", nConns, ")")
	}
	for i := range httpSrvs {
		s := &httpSrvs[i].stats
		s.m.Lock()
		if s.connections != 1 {
			t.Fatal("expected no more than one connection per http server, got", s.connections)
		}
		s.m.Unlock()
	}
}
