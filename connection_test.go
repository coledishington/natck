package main

import (
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
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

func Atoi(t *testing.T, s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("Atoi failed to parse %v as int: %v", s, err)
	}
	return i
}

func createFile(t *testing.T, path string) *os.File {
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
	if err != nil {
		t.Fatalf("cpFile failed to open new file %v: %v", path, err)
	}
	return dst
}

func cpFile(t *testing.T, sPath, dPath string) {
	src, err := os.Open(sPath)
	if err != nil {
		t.Fatal("cpFile failed to open src:", err)
	}
	defer src.Close()

	dst := createFile(t, dPath)
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

func tPath(p ...string) string {
	p = append([]string{"testdata"}, p...)
	return path.Join(p...)
}

func tUrl(t *testing.T, port int, path string) *url.URL {
	s := fmt.Sprintf("http://127.0.0.1:%v/%v", port, path)
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("Failed to parse url %v: %v", s, err)
	}
	return u
}

func makeHtmlDocWithLinks(t *testing.T, urls []*url.URL, dPath string) {
	doc := `<!doctype html>
<html lang="en-US">
<head></head>
<body>
	<ul>
		{{range .}}
		<li>
			<a href="{{print .String}}">{{.Host}}</a>
		</li>
		{{end}}
	</ul>
</body>
</html>`

	tpl := template.Must(template.New("doc").Parse(doc))
	dst := createFile(t, dPath)
	defer dst.Close()
	err := tpl.ExecuteTemplate(dst, "doc", urls)
	if err != nil {
		t.Fatal("Failed to fill in html template:", err)
	}
}

// startHttpServer Starts an http server, raising a test error if
// any issue occurs. This server is setup over localhost, so in
// effect the client and server are one hop from each other, with
// no NAT between them. This can provide test coverage for
// simple cases whilst avoiding platform-specific
// network infrastructure.
func startHttpServer(t *testing.T, tSrv *httpTestServer, root string) {
	stats := &tSrv.stats
	statsCb := func(c net.Conn, s http.ConnState) {
		stats.m.Lock()
		defer stats.m.Unlock()
		if s == http.StateNew {
			stats.connections++
		}
	}

	handler := http.FileServer(http.Dir(root))

	// Bind to port to make sure the server is ready to
	// accept connections immediately
	listenStr := fmt.Sprint(":", tSrv.port)
	listenAddr, err := net.ResolveTCPAddr("tcp", listenStr)
	if err != nil {
		t.Errorf("Failed to resolve tcp address %v: %v", listenStr, err)
	}
	listener, err := net.ListenTCP("tcp", listenAddr)
	if err != nil {
		t.Errorf("Failed to listen on tcp port %v: %v", tSrv.port, err)
	}

	// Example Http keep-alive defaults, in seconds, are Apache(5),
	// Cloudflare(900), GFE(610), LiteSpeed(5s), Microsoft-IIS(120),
	// and nginx(75).
	tSrv.server = &http.Server{
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
		err := tSrv.server.Serve(listener)
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
				urls = append(urls, tUrl(t, port, "index.html"))
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
					port:         p,
					replyLatency: tc.inPortLatency[p],
					redirectCode: redirectCode,
					redirectTo:   tUrl(t, redirectPort, "index.html").String(),
				})
			}

			for _, srv := range httpServers {
				root := t.TempDir()
				cpFile(t, tPath("wildcard_robots.txt"), path.Join(root, "robots.txt"))
				cpFile(t, tPath("no_links.html"), path.Join(root, "index.html"))
				startHttpServer(t, srv, root)
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

func TestMeasureMaxConnectionsCrawlingBehaviour(t *testing.T) {
	var (
		canterburyPort int = 8081
		otagoPort          = 8082
		rakiuraPort        = 8083
		tasmanPort         = 8084
		westCoast          = 8085
	)

	var (
		// Canterbury
		HanmerSprings *url.URL = tUrl(t, canterburyPort, "hanmer_springs.html")
		Kaikoura               = tUrl(t, canterburyPort, "kaikoura.html")
		Christchurch           = tUrl(t, canterburyPort, "christchurch.html")
		Rakaia                 = tUrl(t, canterburyPort, "rakaia.html")
		Ashburton              = tUrl(t, canterburyPort, "ashburton.html")
		Timaru                 = tUrl(t, canterburyPort, "timaru.html")
		// Otago
		Oamaru     = tUrl(t, otagoPort, "oamaru.html")
		Dunedin    = tUrl(t, otagoPort, "dunedin.html")
		Queenstown = tUrl(t, otagoPort, "queenstown.html")
		Wanaka     = tUrl(t, otagoPort, "wanaka.html")
		// Rakiura
		StewartIsland = tUrl(t, rakiuraPort, "stewart_island.html")
		Oban          = tUrl(t, rakiuraPort, "oban.html")
		// Tasman
		Takaka      = tUrl(t, tasmanPort, "takaka.html")
		Collingwood = tUrl(t, tasmanPort, "collingwood.html")
		Puponga     = tUrl(t, tasmanPort, "puponga.html")
		Motueka     = tUrl(t, tasmanPort, "motueka.html")
		Richmond    = tUrl(t, tasmanPort, "richmond.html")
		Nelson      = tUrl(t, tasmanPort, "nelson.html")
		Tapawera    = tUrl(t, tasmanPort, "tapawera.html")
		Murchison   = tUrl(t, tasmanPort, "murchison.html")
		// West Coast
		Reefton         = tUrl(t, westCoast, "reefton.html")
		SpringsJunction = tUrl(t, westCoast, "springs_junction.html")
	)

	type serverAdjacencies map[*url.URL][]*url.URL

	canterburyAdjacencies := serverAdjacencies{
		HanmerSprings: {SpringsJunction, Kaikoura, Christchurch},
		Kaikoura:      {HanmerSprings, Christchurch},
		Christchurch:  {HanmerSprings, Kaikoura, Rakaia},
		Rakaia:        {Christchurch, Ashburton},
		Ashburton:     {Rakaia, Timaru},
		Timaru:        {Ashburton, Oamaru, Queenstown},
	}

	otagoAdjacencies := serverAdjacencies{
		Oamaru:     {Timaru, Dunedin},
		Dunedin:    {Oamaru, Queenstown},
		Queenstown: {Dunedin, Wanaka, Timaru},
		Wanaka:     {Queenstown, Reefton},
	}

	tasmanAdjacencies := serverAdjacencies{
		Puponga:     {Collingwood},
		Collingwood: {Puponga, Takaka},
		Takaka:      {Collingwood, Motueka},
		Motueka:     {Takaka, Richmond},
		Richmond:    {Motueka, Tapawera, Murchison, Nelson},
		Nelson:      {Richmond},
		Tapawera:    {Richmond},
		Murchison:   {Richmond, SpringsJunction},
	}

	RakiuraAdjacencies := serverAdjacencies{
		StewartIsland: {Oban},
		Oban:          {StewartIsland},
	}

	westCoastAdjacencies := serverAdjacencies{
		Reefton:         {SpringsJunction, Wanaka},
		SpringsJunction: {Reefton, Murchison, HanmerSprings},
	}

	regions := []serverAdjacencies{
		canterburyAdjacencies,
		otagoAdjacencies,
		tasmanAdjacencies,
		RakiuraAdjacencies,
		westCoastAdjacencies,
	}

	testcases := map[string]struct {
		inUrls       []*url.URL
		outPortConns []int
	}{
		"Start on isolated server": {
			inUrls:       []*url.URL{StewartIsland},
			outPortConns: []int{8083},
		},
		"Start on surrounded server": {
			inUrls:       []*url.URL{HanmerSprings},
			outPortConns: []int{8081, 8082, 8084, 8085},
		},
		"Start on outside edge of cyclic shape": {
			inUrls:       []*url.URL{Kaikoura},
			outPortConns: []int{8081, 8082, 8084, 8085},
		},
		"Start on sparsely linked server": {
			inUrls:       []*url.URL{Puponga},
			outPortConns: []int{8081, 8082, 8084, 8085},
		},
		"Start on two indirectly linked servers": {
			inUrls:       []*url.URL{Richmond, Kaikoura},
			outPortConns: []int{8081, 8082, 8084, 8085},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			httpSrvs := []*httpTestServer{}
			for _, regionAdjacencies := range regions {
				var u *url.URL

				root := t.TempDir()
				cpFile(t, tPath("wildcard_robots.txt"), path.Join(root, "robots.txt"))
				for page, links := range regionAdjacencies {
					u = page
					base := path.Base(page.Path)
					filename := strings.TrimPrefix(base, "/")
					dest := path.Join(root, filename)
					makeHtmlDocWithLinks(t, links, dest)
				}

				srv := &httpTestServer{port: Atoi(t, u.Port())}
				startHttpServer(t, srv, root)
				httpSrvs = append(httpSrvs, srv)
			}

			nConns := MeasureMaxConnections(tc.inUrls)
			if nConns != len(tc.outPortConns) {
				t.Errorf("expected to measure %d client connections, got %d", len(tc.outPortConns), nConns)
			}

			for _, srv := range httpSrvs {
				srv.stats.m.Lock()
				connections := 0
				if slices.Contains(tc.outPortConns, srv.port) {
					connections++
				}
				if srv.stats.connections != connections {
					t.Errorf("expected server on port %d to get %d connections, got %d", srv.port, connections, srv.stats.connections)
				}
				srv.stats.m.Unlock()
			}

			totalConnections := 0
			for _, srv := range httpSrvs {
				s := &srv.stats
				s.m.Lock()
				totalConnections += s.connections
				s.m.Unlock()
			}
			if totalConnections != len(tc.outPortConns) {
				t.Errorf("expected the total number of new http server connections to be %d, got %d", len(tc.outPortConns), totalConnections)
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
		root := t.TempDir()
		cpFile(t, tPath("wildcard_robots.txt"), path.Join(root, "robots.txt"))
		cpFile(t, tPath("no_links.html"), path.Join(root, "index.html"))
		srv := &httpTestServer{port: i, replyLatency: time.Millisecond}
		startHttpServer(t, srv, root)
		httpSrvs = append(httpSrvs, srv)
	}

	urls := []*url.URL{}
	for i := 8000; i < 8000+nConnections; i++ {
		urls = append(urls, tUrl(t, i, "index.html"))
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
