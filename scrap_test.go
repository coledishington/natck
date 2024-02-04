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

func urlsToStrings(urls []url.URL) []string {
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
	}

	host := "http://localhost:8081/"
	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(fmt.Sprint(host, tc.inHtml))
			if err != nil {
				t.Fatal("Failed to parse test url: ", err)
			}

			links := Scrap(u, openFile(t, tc.inHtml))

			sort.Strings(tc.outUrls)
			if !reflect.DeepEqual(tc.outUrls, urlsToStrings(links)) {
				t.Error("Failed to parse urls out of html: ", tc.outUrls, " != ", links)
			}
		})
	}
}
