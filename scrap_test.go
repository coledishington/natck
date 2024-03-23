package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"sort"
	"testing"
)

func openFile(t *testing.T, path string) io.Reader {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("Failed to open file ", path, ": ", err)
	}

	t.Cleanup(func() {
		err := f.Close()
		if err != nil {
			t.Log("Failed to close file ", path, ": ", err)
		}
	})

	return f
}

func urlsToStrings(urls []*url.URL) []string {
	s := []string{}
	for _, v := range urls {
		s = append(s, v.String())
	}
	sort.Strings(s)
	return s
}

func TestScrap(t *testing.T) {
	testcases := map[string]struct {
		inHtml  string
		outUrls []string
	}{
		"No links": {
			inHtml:  "testdata/no_links.html",
			outUrls: []string{},
		},
		"Absolute hrefs": {
			inHtml: "testdata/absolute_hrefs.html",
			outUrls: []string{
				"http://localhost:8081/auckland.html",
				"http://localhost:8081/christchurch.html",
				"http://localhost:8081/wellington.html",
				"http://localhost:8081/hamilton.html",
				"http://localhost:8081/tauranga.html",
				"http://localhost:8081/lowerhutt.html",
				"http://localhost:8081/dunedin.html",
				"http://localhost:8081/palmerstonnorth.html",
				"http://localhost:8081/napier.html",
				"http://localhost:8081/hibiscuscoast.html",
			},
		},
		"Relative hrefs": {
			inHtml: "testdata/relative_hrefs.html",
			outUrls: []string{
				"http://localhost:8081/auckland.html",
				"http://localhost:8081/christchurch.html",
				"http://localhost:8081/wellington.html",
				"http://localhost:8081/hamilton.html",
				"http://localhost:8081/tauranga.html",
				"http://localhost:8081/lowerhutt.html",
				"http://localhost:8081/dunedin.html",
				"http://localhost:8081/palmerstonnorth.html",
				"http://localhost:8081/napier.html",
				"http://localhost:8081/hibiscuscoast.html",
			},
		},
		"Relative hrefs with base": {
			inHtml: "testdata/relative_hrefs_with_base.html",
			outUrls: []string{
				"http://island.nz/auckland.html",
				"http://island.nz/christchurch.html",
				"http://island.nz/wellington.html",
				"http://island.nz/hamilton.html",
				"http://island.nz/tauranga.html",
				"http://island.nz/lowerhutt.html",
				"http://island.nz/dunedin.html",
				"http://island.nz/palmerstonnorth.html",
				"http://island.nz/napier.html",
				"http://island.nz/hibiscuscoast.html",
			},
		},
	}

	host := "http://localhost:8081/"
	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(fmt.Sprint(host, tc.inHtml))
			if err != nil {
				t.Fatal("Failed to parse test url: ", err)
			}

			links := Scrap(u, openFile(t, tc.inHtml))
			slinks := urlsToStrings(links)
			sort.Strings(slinks)
			sort.Strings(tc.outUrls)
			if !reflect.DeepEqual(tc.outUrls, slinks) {
				t.Error("Failed to parse urls out of html: ", tc.outUrls, " != ", slinks)
			}
		})
	}
}
