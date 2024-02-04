package main

import (
	"io"
	"net/url"

	"golang.org/x/net/html"
)

func urlCmp(u1, u2 *url.URL) bool {
	return u1.Host == u2.Host && u1.Path == u2.Path
}

func Scrap(host *url.URL, body io.Reader) []url.URL {
	urls := []url.URL{}

	for z := html.NewTokenizer(body); ; {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}

		if tt != html.StartTagToken {
			continue
		}

		name, hasAttrs := z.TagName()
		if string(name) != "a" {
			continue
		}

		for more := hasAttrs; more; {
			var k, v []byte

			k, v, more = z.TagAttr()
			aKey := string(k)
			if string(aKey) != "href" {
				continue
			}

			aVal := string(v)
			aUrl, err := url.Parse(aVal)
			if err != nil {
				// Invalid url in href
				continue
			}

			var nUrl *url.URL
			if aUrl.IsAbs() {
				nUrl = aUrl
			} else {
				// Handle relative links
				absUrl := *host
				absUrl.Path = aUrl.Path
				nUrl = &absUrl
			}

			found := false
			for _, u := range urls {
				found = urlCmp(nUrl, &u)
				if found {
					break
				}
			}
			if found {
				continue
			}
			urls = append(urls, *nUrl)
		}
	}
	return urls
}
