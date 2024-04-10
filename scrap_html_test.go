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
		// "wiki": {
		// 	inHtml: "testdata/wikipedia.html",
		// 	outUrls: []string{
		// 		"http://island.nz/auckland.html",
		// 	},
		// },
	}

	host := "http://localhost:8081/"
	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(fmt.Sprint(host, tc.inHtml))
			if err != nil {
				t.Fatal("Failed to parse test url: ", err)
			}

			links := ScrapHtml(u, openFile(t, tc.inHtml))
			slinks := urlsToStrings(links)
			sort.Strings(slinks)
			sort.Strings(tc.outUrls)

			// reduced := []*url.URL{}
			// for _, u := range links {
			// 	found := false
			// 	for _, r := range reduced {
			// 		found = canonicalHost(r) == canonicalHost(u)
			// 		if found {
			// 			break
			// 		}
			// 	}
			// 	if found {
			// 		continue
			// 	}
			// 	reduced = append(reduced, u)
			// }
			// fmt.Println("---------------- reduced -------------------------------------------")
			// fmt.Println(reduced)
			// fmt.Println("-------------------------------------------------------------------")

			// _translate := func(u *url.URL) (netip.AddrPort, error) {
			// 	portString := urlPort(u)
			// 	p64, err := strconv.ParseUint(portString, 10, 16)
			// 	if err != nil {
			// 		return netip.AddrPort{}, err
			// 	}
			// 	p := uint16(p64)

			// 	addrs, err := net.LookupIP(u.Hostname())
			// 	if len(addrs) == 0 || err != nil {
			// 		return netip.AddrPort{}, io.ErrClosedPipe
			// 	}
			// 	addr := addrs[0]

			// 	addrPort, ok := netip.AddrFromSlice(addr)
			// 	if !ok {
			// 		return netip.AddrPort{}, io.ErrClosedPipe
			// 	}
			// 	return netip.AddrPortFrom(addrPort, p), nil
			// }

			// addred := []*url.URL{}
			// for _, r := range reduced {
			// 	ra, err := _translate(r)
			// 	if err != nil {
			// 		continue
			// 	}
			// 	found := false
			// 	for _, a := range addred {
			// 		aa, err := _translate(a)
			// 		if err != nil {
			// 			found = true
			// 			break
			// 		}
			// 		found = ra == aa
			// 		if found {
			// 			break
			// 		}
			// 	}
			// 	if !found {
			// 		addred = append(addred, r)
			// 	}
			// }
			// fmt.Println("---------------- addred -------------------------------------------")
			// fmt.Println(addred)
			// fmt.Println("-----------------------------------------------------------------------")

			if !reflect.DeepEqual(tc.outUrls, slinks) {
				t.Error("Failed to parse urls out of html: ", tc.outUrls, " != ", slinks)
			}
		})
	}
}
