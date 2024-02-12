package main

import (
	"fmt"
	"net/http"
	"testing"
)

// type netNameSpace struct {
// 	name    string
// 	address net.Addr
// }

// func AddInterface() Interface

// func joinNets() {

// }

// func httpServerFileHandlerTracer

// Do I need a raw socket to stop TCP syn replies?
// * Raw socket requires

// type

func spawnHttpServer(t *testing.T, port int) {
	rDir := http.Dir(fmt.Sprint("testdata/http-server-", port))
	srv := http.Server{
		Addr:    fmt.Sprint(":", port),
		Handler: http.FileServer(rDir),
	}
	t.Cleanup(func() { srv.Close() }) // TODO: print any errors

	go func() {
		srv.ListenAndServe() // Print error (unless duplicated by srv.Close())
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
		// "one server": {
		// 	inUrls:    []string{"http://127.0.0.1:8081/index.html"},
		// 	outNConns: 1,
		// },
		// "two servers": {
		// 	inUrls: []string{
		// 		"http://127.0.0.1:8081/index.html",
		// 		"http://127.0.0.1:8082/index.html",
		// 	},
		// 	outNConns: 2,
		// },
		"reachable and unreachable server": {
			inUrls: []string{
				"http://127.0.0.1:8081/index.html",
				"http://127.0.0.1:8089/index.html",
			},
			outNConns: 1,
		},
	}

	// Setup all possible servers but only use a subset in each test
	spawnHttpServer(t, 8081)
	spawnHttpServer(t, 8082)

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			nConns := MeasureMaxConnections(tc.inUrls)
			if nConns != tc.outNConns {
				t.Error("expected (nConns=", tc.outNConns, "), got (nConns=", nConns, ")")
			}
		})
	}
}
