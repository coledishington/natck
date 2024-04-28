package main

import (
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/netip"
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

// Shortcut making http handlers by avoiding object creation
type HandlerFunc func(http.ResponseWriter, *http.Request) bool
type HandlerChain []HandlerFunc

type request struct {
	requester netip.Addr
	path      string
	received  time.Time
}

type httpServerStats struct {
	m           sync.Mutex
	connections int
	requests    []request
}

type httpTestServer struct {
	server   *http.Server
	stats    httpServerStats
	name     string
	handlers HandlerChain
}

func (h HandlerChain) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	for _, handler := range h {
		more := handler(res, req)
		if !more {
			break
		}
	}
}

func (s *httpServerStats) reset() {
	s.m.Lock()
	defer s.m.Unlock()
	s.connections = 0
	s.requests = []request{}
}

func (srv *httpTestServer) tUrl(t *testing.T, path string) *url.URL {
	if srv.server.Addr == "" {
		t.Fatalf("server %v has no addr yet", srv.name)
	}
	s := fmt.Sprintf("http://%v/%v", srv.server.Addr, path)
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("failed to parse url %v: %v", s, err)
	}
	return u
}

func (srv *httpTestServer) makeRequestStatsHandler() HandlerFunc {
	stats := &srv.stats
	return func(res http.ResponseWriter, req *http.Request) bool {
		received := time.Now()
		addrPort := netip.MustParseAddrPort(req.RemoteAddr)
		r := request{
			requester: addrPort.Addr(),
			path:      req.URL.Path,
			received:  received,
		}

		stats.m.Lock()
		defer stats.m.Unlock()
		stats.requests = append(stats.requests, r)
		return true
	}
}

func makeFileHandler(root string) HandlerFunc {
	handler := http.FileServer(http.Dir(root))
	return func(res http.ResponseWriter, req *http.Request) bool {
		handler.ServeHTTP(res, req)
		return false
	}
}

func makeLatencyHandler(wait time.Duration) HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) bool {
		time.Sleep(wait)
		return true
	}
}

func makeRedirectHandler(redirectTo string, redirectCode int) HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) bool {
		res.Header()[http.CanonicalHeaderKey("Content-Type")] = nil
		http.Redirect(res, req, redirectTo, redirectCode)
		return false
	}
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

func makeServerRoot(t *testing.T, files ...string) string {
	robots := []string{}
	html := []string{}
	for _, f := range files {
		if strings.HasSuffix(f, "robots.txt") {
			robots = append(robots, f)
		} else if strings.HasSuffix(f, ".html") {
			html = append(html, f)
		} else {
			t.Fatal("makeServerRoot: unsupported file type:", f)
		}
	}

	root := t.TempDir()

	if len(robots) > 1 {
		t.Fatal("makeServerRoot: more than one robots.txt:", robots)
	}
	if len(robots) == 1 {
		cpFile(t, robots[0], path.Join(root, "robots.txt"))
	}

	if len(html) > 0 {
		cpFile(t, html[0], path.Join(root, "index.html"))
	}
	for _, f := range html {
		cpFile(t, f, path.Join(root, path.Base(f)))
	}
	return root
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
func startHttpServer(t *testing.T, tSrv *httpTestServer) {
	stats := &tSrv.stats
	statsCb := func(c net.Conn, s http.ConnState) {
		stats.m.Lock()
		defer stats.m.Unlock()
		if s == http.StateNew {
			stats.connections++
		}
	}

	addrPort := "127.0.0.1:0"
	if tSrv.server != nil && tSrv.server.Addr != "" {
		addr := netip.MustParseAddr(tSrv.server.Addr)
		addrPort = netip.AddrPortFrom(addr, 0).String()
	}

	// Bind to port to make sure the server is ready to
	// accept connections immediately
	listener, err := net.Listen("tcp", addrPort)
	if err != nil {
		t.Errorf("failed to listen on localhost tcp port: %v", err)
	}

	// Example Http keep-alive defaults, in seconds, are Apache(5),
	// Cloudflare(900), GFE(610), LiteSpeed(5s), Microsoft-IIS(120),
	// and nginx(75).
	if tSrv.server == nil {
		tSrv.server = &http.Server{}
	}
	tSrv.server.Handler = tSrv.handlers
	tSrv.server.IdleTimeout = 5 * time.Second
	tSrv.server.ConnState = statsCb
	tSrv.server.Addr = listener.Addr().String()

	t.Cleanup(func() {
		err := tSrv.server.Close()
		if err != nil {
			t.Errorf("failed to close http server %v: %v", tSrv.name, err)
		}
	})

	go func() {
		err := tSrv.server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("unexpected shutdown of http server %v: %v", tSrv.name, err)
		}
	}()
}

func checkMaxConnections(t *testing.T, urls []*url.URL, nConns int, srvs []*httpTestServer) {
	measured := MeasureMaxConnections(urls)
	if measured != nConns {
		t.Errorf("expected to measure %d connections, got %d", measured, nConns)
	}

	for _, srv := range srvs {
		s := &srv.stats
		s.m.Lock()
		sConns := s.connections
		s.m.Unlock()
		if sConns > 1 {
			t.Errorf("no server should receive > 1 connection, %v got %d", srv.name, sConns)
		}
	}

	total := 0
	for _, srv := range srvs {
		s := &srv.stats
		s.m.Lock()
		total += s.connections
		s.m.Unlock()
	}
	if total != nConns {
		t.Errorf("expected total server connections to be %d, got %d", nConns, total)
	}
}

func TestSmallTopologyConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping due to re-request timeouts.")
	}

	const (
		stdSrv      = iota
		slowSrv     = iota
		redirectSrv = iota
	)

	testcases := map[string]struct {
		inSrvs    []int
		outNConns int
	}{
		"few servers": {
			inSrvs:    []int{stdSrv, stdSrv, stdSrv},
			outNConns: 3,
		},
		"slow server": {
			inSrvs:    []int{stdSrv, slowSrv},
			outNConns: 2,
		},
		"server redirect": {
			inSrvs:    []int{stdSrv, redirectSrv},
			outNConns: 2,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			srvs := []*httpTestServer{}
			for i, srvType := range tc.inSrvs {
				root := makeServerRoot(t, tPath("wildcard_robots.txt"), tPath("no_links.html"))

				handlers := HandlerChain{}
				switch srvType {
				case stdSrv:
					handlers = append(handlers, makeFileHandler(root))
				case slowSrv:
					handlers = append(handlers,
						makeLatencyHandler(time.Second),
						makeFileHandler(root))
				case redirectSrv:
					r := srvs[len(srvs)-1].tUrl(t, "index.html").String()
					h := makeRedirectHandler(r, http.StatusMovedPermanently)
					handlers = append(handlers, h)
				}
				srv := &httpTestServer{
					name:     fmt.Sprintf("http.%v", i),
					handlers: handlers,
				}
				startHttpServer(t, srv)
				srvs = append(srvs, srv)
			}

			urls := []*url.URL{}
			for _, srv := range srvs {
				urls = append(urls, srv.tUrl(t, "index.html"))
			}

			checkMaxConnections(t, urls, tc.outNConns, srvs)
		})
	}
}

func TestBigTopologyConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TestMeasureMaxConnections in short mode due to re-request timeouts.")
	}

	nConnections := 1000
	srvs := []*httpTestServer{}
	for i := range nConnections {
		root := makeServerRoot(t, tPath("wildcard_robots.txt"), tPath("no_links.html"))

		srv := &httpTestServer{
			name: fmt.Sprintf("http.%v", i),
			handlers: HandlerChain{
				makeLatencyHandler(time.Millisecond),
				makeFileHandler(root),
			},
		}
		startHttpServer(t, srv)
		srvs = append(srvs, srv)
	}

	urls := []*url.URL{}
	for _, srv := range srvs {
		urls = append(urls, srv.tUrl(t, "index.html"))
	}

	checkMaxConnections(t, urls, nConnections, srvs)
}

func TestRequestCrawlDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping due to re-request timeouts.")
	}

	srv := &httpTestServer{name: "server"}
	startHttpServer(t, srv)

	root := makeServerRoot(t, tPath("wildcard_robots.txt"))
	blog := srv.tUrl(t, "blog.html")
	makeHtmlDocWithLinks(t, []*url.URL{blog}, path.Join(root, "index.html"))
	cpFile(t, tPath("no_links.html"), path.Join(root, "blog.html"))
	srv.server.Handler = HandlerChain{
		srv.makeRequestStatsHandler(),
		makeFileHandler(root),
	}

	urls := []*url.URL{srv.tUrl(t, "")}
	srvs := []*httpTestServer{srv}
	checkMaxConnections(t, urls, len(urls), srvs)

	srv.stats.m.Lock()
	ref := srv.stats.requests[0].received
	for _, r := range srv.stats.requests[1:] {
		tPassed := r.received.Sub(ref)

		// Make sure the testing system can fulfil a request much
		// faster than every 500ms
		if tPassed > 250*time.Millisecond {
			t.Errorf("expected minor rate-limiting, got %v on %v", tPassed, r.path)
		}
	}
	srv.stats.m.Unlock()

	robotsPath := path.Join(root, "robots.txt")
	os.Remove(robotsPath)
	robotsTxt := *defaultRobotsTxtRecord.Clone()
	robotsTxt.Rules = []rule{
		{Token: "Crawl-delay", Value: "0.5"},
	}
	makeRobotsTxt(t, []record{robotsTxt}, robotsPath)

	srv.stats.reset()
	checkMaxConnections(t, urls, len(urls), srvs)

	srv.stats.m.Lock()
	ref = srv.stats.requests[0].received
	for _, r := range srv.stats.requests[1:] {
		tPassed := r.received.Sub(ref)
		if tPassed < 500*time.Millisecond {
			t.Errorf("expected rate-limiting of 500ms, got %v on %v", tPassed, r.path)
		}
	}
}

func TestRequestRateLimiting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping due to re-request timeouts.")
	}

	srv := &httpTestServer{name: "server"}
	startHttpServer(t, srv)

	root := makeServerRoot(t)

	// Set Crawl-delay slow enough to avoid requesting
	// the second html before the HTTP 429 is served.
	robotsTxt := *defaultRobotsTxtRecord.Clone()
	robotsTxt.Rules = []rule{
		{Token: "Crawl-delay", Value: "0.5"},
	}
	robotsPath := path.Join(root, "robots.txt")
	makeRobotsTxt(t, []record{robotsTxt}, robotsPath)

	blogs := []*url.URL{
		srv.tUrl(t, "blog1.html"),
		srv.tUrl(t, "blog2.html"),
	}
	makeHtmlDocWithLinks(t, blogs, path.Join(root, "index.html"))
	cpFile(t, tPath("no_links.html"), path.Join(root, "blog1.html"))
	cpFile(t, tPath("no_links.html"), path.Join(root, "blog2.html"))
	fileHandler := func() HandlerFunc {
		reply429Next := false
		handler := http.FileServer(http.Dir(root))
		return func(res http.ResponseWriter, req *http.Request) bool {
			if reply429Next {
				res.Header()[http.CanonicalHeaderKey("Retry-After")] = []string{"1"}
				res.WriteHeader(429)
			} else {
				handler.ServeHTTP(res, req)
			}
			reply429Next = req.URL.Path == "/"
			return false
		}
	}
	srv.server.Handler = HandlerChain{
		srv.makeRequestStatsHandler(),
		fileHandler(),
	}

	urls := []*url.URL{srv.tUrl(t, "")}
	srvs := []*httpTestServer{srv}
	checkMaxConnections(t, urls, len(urls), srvs)

	// Check connection was rate-limited after 429
	srv.stats.m.Lock()
	served_429 := false
	ref := srv.stats.requests[0].received
	for _, r := range srv.stats.requests[1:] {
		tPassed := r.received.Sub(ref)
		if served_429 && tPassed < time.Second {
			t.Errorf("expected connection to be rate-limited to 1s, got %v on %v", tPassed, r.path)
		} else if !served_429 && tPassed > 600*time.Millisecond {
			t.Errorf("expected connection to be faster than 1s, got %v on %v", tPassed, r.path)
		}
		ref = r.received
		if strings.Contains(r.path, "blog") {
			served_429 = true
		}
	}
	srv.stats.m.Unlock()
}

func TestCrawlingBehaviourOnSmallTopology(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping due to re-request timeouts.")
	}

	testcases := map[string]struct {
		inPreStart func(t *testing.T, srvs []*httpTestServer)
		inPreRun   func(t *testing.T, srvs []*httpTestServer) []*url.URL
		outNConns  int
	}{
		"no links": {
			inPreRun: func(t *testing.T, srvs []*httpTestServer) []*url.URL {
				root := makeServerRoot(t, tPath("wildcard_robots.txt"), tPath("no_links.html"))
				srvs[0].server.Handler = HandlerChain{
					makeFileHandler(root),
				}
				return []*url.URL{srvs[0].tUrl(t, "index.html")}
			},
			outNConns: 1,
		},
		"html link": {
			inPreRun: func(t *testing.T, srvs []*httpTestServer) []*url.URL {
				root := makeServerRoot(t, tPath("wildcard_robots.txt"))
				u := srvs[1].tUrl(t, "index.html")
				makeHtmlDocWithLinks(t, []*url.URL{u}, path.Join(root, "index.html"))
				srvs[0].server.Handler = HandlerChain{
					makeFileHandler(root),
				}
				return []*url.URL{srvs[0].tUrl(t, "index.html")}
			},
			outNConns: 2,
		},
		"html link with no .html suffix": {
			inPreRun: func(t *testing.T, srvs []*httpTestServer) []*url.URL {
				root := makeServerRoot(t, tPath("wildcard_robots.txt"))
				u := srvs[1].tUrl(t, "MainPage")
				makeHtmlDocWithLinks(t, []*url.URL{u}, path.Join(root, "MainPage"))
				srvs[0].server.Handler = HandlerChain{
					makeFileHandler(root),
				}
				return []*url.URL{srvs[0].tUrl(t, "MainPage")}
			},
			outNConns: 2,
		},
		"html link to IPv6 server": {
			inPreStart: func(t *testing.T, srvs []*httpTestServer) {
				srvs[1].server = &http.Server{Addr: "::1"}
			},
			inPreRun: func(t *testing.T, srvs []*httpTestServer) []*url.URL {
				root := makeServerRoot(t, tPath("wildcard_robots.txt"))
				u := srvs[1].tUrl(t, "index.html")
				makeHtmlDocWithLinks(t, []*url.URL{u}, path.Join(root, "index.html"))
				srvs[0].server.Handler = HandlerChain{makeFileHandler(root)}
				return []*url.URL{srvs[0].tUrl(t, "index.html")}
			},
			outNConns: 1,
		},
		"redirect": {
			inPreRun: func(t *testing.T, srvs []*httpTestServer) []*url.URL {
				mux := http.NewServeMux()

				// Ensure small Crawl-delay is read from robots.txt
				// for a fast test
				root := makeServerRoot(t, tPath("wildcard_robots.txt"))
				mux.Handle("/robots.txt", HandlerChain{makeFileHandler(root)})

				r := srvs[1].tUrl(t, "index.html").String()
				h := makeRedirectHandler(r, http.StatusMovedPermanently)
				mux.Handle("/", HandlerChain{h})

				srvs[0].server.Handler = mux
				return []*url.URL{srvs[0].tUrl(t, "index.html")}
			},
			outNConns: 2,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			srvs := []*httpTestServer{}
			for i := range 2 {
				s := &httpTestServer{
					name: fmt.Sprintf("http.%v", i),
				}
				srvs = append(srvs, s)
			}
			if tc.inPreStart != nil {
				tc.inPreStart(t, srvs)
			}
			for _, s := range srvs {
				startHttpServer(t, s)
			}

			root := makeServerRoot(t, tPath("wildcard_robots.txt"), tPath("no_links.html"))
			srvs[1].server.Handler = HandlerChain{makeFileHandler(root)}

			urls := tc.inPreRun(t, srvs)

			checkMaxConnections(t, urls, tc.outNConns, srvs)
		})
	}
}

func TestCrawlingBehaviour(t *testing.T) {
	const (
		canterbury = "canterbury"
		otago      = "otago"
		rakiura    = "rakiura"
		tasman     = "tasman"
		westCoast  = "westcoast"
	)

	type page struct {
		srv  string
		path string
	}

	var (
		// Canterbury
		hanmerSprings = page{canterbury, "hanmer_springs.html"}
		kaikoura      = page{canterbury, "kaikoura.html"}
		christchurch  = page{canterbury, "christchurch.html"}
		rakaia        = page{canterbury, "rakaia.html"}
		ashburton     = page{canterbury, "ashburton.html"}
		timaru        = page{canterbury, "timaru.html"}
		// Otago
		oamaru     = page{otago, "oamaru.html"}
		dunedin    = page{otago, "dunedin.html"}
		queenstown = page{otago, "queenstown.html"}
		wanaka     = page{otago, "wanaka.html"}
		// Rakiura
		stewartIsland = page{rakiura, "stewart_island.html"}
		oban          = page{rakiura, "oban.html"}
		// Tasman
		takaka      = page{tasman, "takaka.html"}
		collingwood = page{tasman, "collingwood.html"}
		puponga     = page{tasman, "puponga.html"}
		motueka     = page{tasman, "motueka.html"}
		richmond    = page{tasman, "richmond.html"}
		nelson      = page{tasman, "nelson.html"}
		tapawera    = page{tasman, "tapawera.html"}
		murchison   = page{tasman, "murchison.html"}
		// West Coast
		reefton         = page{westCoast, "reefton.html"}
		springsJunction = page{westCoast, "springs_junction.html"}
	)

	type serverAdjacencies map[page][]page

	canterburyAdjacencies := serverAdjacencies{
		hanmerSprings: {springsJunction, kaikoura, christchurch},
		kaikoura:      {hanmerSprings, christchurch},
		christchurch:  {hanmerSprings, kaikoura, rakaia},
		rakaia:        {christchurch, ashburton},
		ashburton:     {rakaia, timaru},
		timaru:        {ashburton, oamaru, queenstown},
	}

	otagoAdjacencies := serverAdjacencies{
		oamaru:     {timaru, dunedin},
		dunedin:    {oamaru, queenstown},
		queenstown: {dunedin, wanaka, timaru},
		wanaka:     {queenstown, reefton},
	}

	tasmanAdjacencies := serverAdjacencies{
		puponga:     {collingwood},
		collingwood: {puponga, takaka},
		takaka:      {collingwood, motueka},
		motueka:     {takaka, richmond},
		richmond:    {motueka, tapawera, murchison, nelson},
		nelson:      {richmond},
		tapawera:    {richmond},
		murchison:   {richmond, springsJunction},
	}

	rakiuraAdjacencies := serverAdjacencies{
		stewartIsland: {oban},
		oban:          {stewartIsland},
	}

	westCoastAdjacencies := serverAdjacencies{
		reefton:         {springsJunction, wanaka},
		springsJunction: {reefton, murchison, hanmerSprings},
	}

	regions := map[string]serverAdjacencies{
		canterbury: canterburyAdjacencies,
		otago:      otagoAdjacencies,
		tasman:     tasmanAdjacencies,
		rakiura:    rakiuraAdjacencies,
		westCoast:  westCoastAdjacencies,
	}

	testcases := map[string]struct {
		inPages  []page
		outConns []string
	}{
		"Start on isolated server": {
			inPages:  []page{stewartIsland},
			outConns: []string{rakiura},
		},
		"Start on surrounded server": {
			inPages:  []page{hanmerSprings},
			outConns: []string{canterbury, otago, tasman, westCoast},
		},
		"Start on outside edge of cyclic shape": {
			inPages:  []page{kaikoura},
			outConns: []string{canterbury, otago, tasman, westCoast},
		},
		"Start on sparsely linked server": {
			inPages:  []page{puponga},
			outConns: []string{canterbury, otago, tasman, westCoast},
		},
		"Start on two indirectly linked servers": {
			inPages:  []page{richmond, kaikoura},
			outConns: []string{canterbury, otago, tasman, westCoast},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			// Start servers to bind ports
			srvs := map[string]*httpTestServer{}
			srvs[canterbury] = &httpTestServer{name: canterbury}
			srvs[otago] = &httpTestServer{name: otago}
			srvs[rakiura] = &httpTestServer{name: rakiura}
			srvs[tasman] = &httpTestServer{name: tasman}
			srvs[westCoast] = &httpTestServer{name: westCoast}
			for _, srv := range srvs {
				startHttpServer(t, srv)
			}

			// Generate server pages
			for region, regionAdjacencies := range regions {
				srv := srvs[region]

				root := makeServerRoot(t, tPath("wildcard_robots.txt"))
				for page, links := range regionAdjacencies {
					urls := []*url.URL{}
					for _, link := range links {
						lSrv := srvs[link.srv]
						urls = append(urls, lSrv.tUrl(t, link.path))
					}

					dest := path.Join(root, page.path)
					makeHtmlDocWithLinks(t, urls, dest)
				}

				srv.server.Handler = HandlerChain{makeFileHandler(root)}
			}

			urls := []*url.URL{}
			for _, page := range tc.inPages {
				srv := srvs[page.srv]
				urls = append(urls, srv.tUrl(t, page.path))
			}

			srvsSlice := []*httpTestServer{}
			for _, s := range srvs {
				srvsSlice = append(srvsSlice, s)
			}
			checkMaxConnections(t, urls, len(tc.outConns), srvsSlice)

			for _, srv := range srvs {
				connections := 0
				if slices.Contains(tc.outConns, srv.name) {
					connections++
				}

				srv.stats.m.Lock()
				if srv.stats.connections != connections {
					t.Errorf("expected server %v to get %d connections, got %d", srv.name, connections, srv.stats.connections)
				}
				srv.stats.m.Unlock()
			}
		})
	}
}
