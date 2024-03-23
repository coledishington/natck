package main

import (
	"io"
	"net/url"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func urlCmp(u1, u2 *url.URL) bool {
	return u1.Host == u2.Host && u1.Path == u2.Path
}

func findAllAtomTagInNode(n *html.Node, tag atom.Atom) []*html.Node {
	matches := []*html.Node{}
	stack := []*html.Node{}
	for iter := n; iter != nil; {
		if iter.DataAtom == tag {
			matches = append(matches, iter)
		}
		for child := iter.FirstChild; child != nil; child = child.NextSibling {
			stack = append(stack, child)
		}
		if len(stack) == 0 {
			break
		}
		iter = stack[len(stack)-1]
		stack = stack[:len(stack)-1]
	}
	return matches
}

func findAtomAttrInNode(n *html.Node, needle atom.Atom) (html.Attribute, bool) {
	for _, a := range n.Attr {
		if atom.Lookup([]byte(a.Key)) == needle {
			return a, true
		}
	}
	return html.Attribute{}, false
}

func findHref(n *html.Node) (*url.URL, error) {
	href, found := findAtomAttrInNode(n, atom.Href)
	if !found {
		return nil, nil
	}

	return url.Parse(href.Val)
}

func Scrap(host *url.URL, body io.Reader) []*url.URL {
	urls := []*url.URL{}
	doc, err := html.Parse(body)
	if err != nil {
		return urls
	}

	// Parse gettable urls
	links := findAllAtomTagInNode(doc, atom.A)
	for _, n := range links {
		u, err := findHref(n)
		if u == nil || err != nil {
			continue
		}

		nUrl := u
		if !nUrl.IsAbs() {
			path := nUrl.Path
			*nUrl = *host
			nUrl.Path = path
		}

		found := false
		for _, u := range urls {
			found = urlCmp(nUrl, u)
			if found {
				break
			}
		}
		if found {
			continue
		}
		urls = append(urls, nUrl)
	}

	return urls
}
