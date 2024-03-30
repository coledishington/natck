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
	redirectCode int
	redirectTo   string
	stats        httpServerStats
}

type HandlerWrapper struct {
	latency      time.Duration
	redirectCode int
	redirectTo   string
	wrapped      http.Handler
}

func (h HandlerWrapper) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	if h.latency != 0 {
		time.Sleep(h.latency)
	}
	if h.redirectCode != 0 {
		res.Header()[http.CanonicalHeaderKey("Content-Type")] = nil
		http.Redirect(res, req, h.redirectTo, h.redirectCode)
		return
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

	// Example Http keep-alive defaults, in seconds, are Apache(5),
	// Cloudflare(900), GFE(610), LiteSpeed(5s), Microsoft-IIS(120),
	// and nginx(75).
	tSrv.server = &http.Server{
		Addr:      fmt.Sprint(":", tSrv.port),
		ConnState: statsCb,
		Handler: HandlerWrapper{
			redirectCode: tSrv.redirectCode,
			redirectTo:   tSrv.redirectTo,
			latency:      tSrv.replyLatency,
			wrapped:      handler,
		},
		IdleTimeout: 5 * time.Second,
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

func tUrl(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%v/index.html", port)
}

func TestMeasureMaxConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnections in short mode due to re-request timeouts.")
	}

	testcases := map[string]struct {
		inPorts        []int
		inPortLatency  map[int]time.Duration
		inPortRedirect map[int]int
		outNConns      int
	}{
		"few servers": {
			inPorts:   []int{8081, 8082, 8083},
			outNConns: 3,
		},
		"repeat servers": {
			inPorts:   []int{8081, 8081, 8082},
			outNConns: 2,
		},
		"reachable and unreachable servers": {
			inPorts:   []int{8081, 8089, 8090, 8082, 8091},
			outNConns: 2,
		},
		"slow server": {
			inPorts:       []int{8081, 8082},
			inPortLatency: map[int]time.Duration{8082: time.Second},
			outNConns:     2,
		},
		"server redirect": {
			inPorts:        []int{8081, 8082},
			inPortRedirect: map[int]int{8082: 8083},
			outNConns:      3,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			urls := []*url.URL{}
			for _, port := range tc.inPorts {
				u, err := url.Parse(tUrl(port))
				if err != nil {
					t.Fatal("Test url failed to parse")
				}
				urls = append(urls, u)
			}

			portToNConns := make(map[int]int, 0)
			// Servers should not repeat connections
			for _, port := range tc.inPorts {
				portToNConns[port] = 1
			}
			// http client should discover clients via redirect
			for _, port := range tc.inPortRedirect {
				portToNConns[port] = 1
			}

			httpServers := []*httpTestServer{}
			for _, p := range []int{8081, 8082, 8083} {
				redirectCode := 0
				redirectPort, exists := tc.inPortRedirect[p]
				if exists {
					redirectCode = http.StatusMovedPermanently
				}
				httpServers = append(httpServers, &httpTestServer{
					testdata:     tPath("no_links.html"),
					port:         p,
					replyLatency: tc.inPortLatency[p],
					redirectCode: redirectCode,
					redirectTo:   tUrl(redirectPort),
				})
			}

			for _, srv := range httpServers {
				startHttpServer(t, srv)
			}

			nConns := MeasureMaxConnections(urls)
			if nConns != tc.outNConns {
				t.Errorf("expected to measure %d client connections, got %d", tc.outNConns, nConns)
			}

			for _, srv := range httpServers {
				srv.stats.m.Lock()
				if srv.stats.connections != portToNConns[srv.port] {
					t.Errorf("expected server on port %d to get %d connections, got %d", srv.port, portToNConns[srv.port], srv.stats.connections)
				}
				srv.stats.m.Unlock()
			}

			totalConnections := 0
			for _, srv := range httpServers {
				s := &srv.stats
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

	urls := []*url.URL{}
	for i := 8000; i < 8000+nConnections; i++ {
		ln := fmt.Sprintf("http://localhost:%v/index.html", i)
		u, err := url.Parse(ln)
		if err != nil {
			t.Fatal("Test url failed to parse", ln)
		}
		urls = append(urls, u)
	}

	nConns := MeasureMaxConnections(urls)
	if nConns != nConnections {
		t.Error("expected (nConns=", 10, "), got (nConns=", nConns, ")")
	}
	for _, srv := range httpSrvs {
		s := &srv.stats
		s.m.Lock()
		if s.connections != 1 {
			t.Fatal("expected no more than one connection per http server, got", s.connections)
		}
		s.m.Unlock()
	}
}
