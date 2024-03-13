package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"sync"
	"testing"
	"time"
)

type httpRequest struct {
	client, server netip.AddrPort
	method         string
	path           string
	received       time.Time
}

type httpServerStats struct {
	m           sync.Mutex
	connections int
	requests    []*httpRequest
}

type httpTestServer struct {
	server       *http.Server
	testdata     string
	listenAddr   netip.AddrPort
	replyLatency time.Duration
	redirectCode int
	redirectTo   string
	stats        httpServerStats
	wrapped      http.Handler
}

func (h *httpTestServer) addRequest(req *http.Request) error {
	stats := &h.stats
	stats.m.Lock()
	defer stats.m.Unlock()

	raddr, err := netip.ParseAddrPort(req.RemoteAddr)
	if err != nil {
		return err
	}

	stats.requests = append(stats.requests, &httpRequest{
		client:   raddr,
		server:   h.listenAddr,
		method:   req.Method,
		path:     req.URL.Path,
		received: time.Now(),
	})
	return nil
}

func (h *httpTestServer) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	// Cannot call test.Fatal in server go routine, close server instead
	if h.addRequest(req) != nil {
		h.server.Close()
		return
	}

	if h.replyLatency != 0 {
		time.Sleep(h.replyLatency)
	}
	if h.redirectCode != 0 {
		res.Header()[http.CanonicalHeaderKey("Content-Type")] = nil
		http.Redirect(res, req, h.redirectTo, h.redirectCode)
		return
	}
	h.wrapped.ServeHTTP(res, req)
}

func parseAddrPort(t *testing.T, a string) netip.AddrPort {
	addr, err := netip.ParseAddrPort(a)
	if err != nil {
		t.Fatal("ParseAddrPort failed:", err)
	}
	return addr
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
			t.Errorf("Failed to cleanup root directory of http server listening on %v: %v", tSrv.listenAddr.String(), err)
		}
	})

	cpFile(t, tSrv.testdata, path.Join(dir, "index.html"))

	connStateCb := func(c net.Conn, s http.ConnState) {
		stats := &tSrv.stats
		stats.m.Lock()
		defer stats.m.Unlock()
		if s == http.StateNew {
			stats.connections++
		}
	}

	tSrv.wrapped = http.FileServer(http.Dir(dir))

	// Example Http keep-alive defaults, in seconds, are Apache(5),
	// Cloudflare(900), GFE(610), LiteSpeed(5s), Microsoft-IIS(120),
	// and nginx(75).
	tSrv.server = &http.Server{
		Addr:        tSrv.listenAddr.String(),
		ConnState:   connStateCb,
		Handler:     tSrv,
		IdleTimeout: 5 * time.Second,
	}

	t.Cleanup(func() {
		err := tSrv.server.Close()
		if err != nil {
			t.Errorf("Failed to close http server listening on %v: %v", tSrv.listenAddr.String(), err)
		}
	})

	go func() {
		err := tSrv.server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Unexpected shutdown of http server listening on %v: %v", tSrv.listenAddr.String(), err)
		}
	}()
}

func tAddrPort(t *testing.T, port int) netip.AddrPort {
	return parseAddrPort(t, fmt.Sprintf("127.0.0.1:%v", port))
}

func tPath(p ...string) string {
	p = append([]string{"testdata"}, p...)
	return path.Join(p...)
}

func tUrl(t *testing.T, port int) *url.URL {
	addr := tAddrPort(t, port)
	s := fmt.Sprintf("http://%v/index.html", addr.String())
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("Failed to parse url: %v", err)
	}
	return u
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
			outNConns: 3,
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
			outNConns:      2,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			urls := []*url.URL{}
			for _, port := range tc.inPorts {
				urls = append(urls, tUrl(t, port))
			}

			portToNConns := make(map[int]int, 0)
			for _, port := range tc.inPorts {
				portToNConns[port]++
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
					listenAddr:   tAddrPort(t, p),
					replyLatency: tc.inPortLatency[p],
					redirectCode: redirectCode,
					redirectTo:   tUrl(t, redirectPort).String(),
				})
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
				sPort := int(s.listenAddr.Port())
				sConns := s.stats.connections
				if s.stats.connections != portToNConns[sPort] {
					t.Errorf("expected server listening on %v to get %d connections, got %d", s.listenAddr, portToNConns[sPort], sConns)
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
		srv := &httpTestServer{
			testdata:     tPath("no_links.html"),
			listenAddr:   tAddrPort(t, i),
			replyLatency: time.Millisecond,
		}
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
	for i := range httpSrvs {
		s := &httpSrvs[i].stats
		s.m.Lock()
		if s.connections != 1 {
			t.Fatal("expected no more than one connection per http server, got", s.connections)
		}
		s.m.Unlock()
	}
}

func TestMeasureMaxConnectionsCrawlingBehaviour(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnectionsCrawlingBehaviour in short mode due to re-request timeouts.")
	}

	nConns := MeasureMaxConnections(urls)
}
