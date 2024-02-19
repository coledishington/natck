package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

func readUrls(input io.Reader) ([]url.URL, error) {
	urls := make([]url.URL, 0)
	b := make([]byte, 0)
	for {
		_, err := input.Read(b)
		if err == io.EOF {
			break
		}
		if err != nil {
			err = fmt.Errorf("failed to read url line: %w", err)
			return nil, err
		}

		u, err := url.Parse(string(b))
		if err != nil {
			err = fmt.Errorf("failed to read url line: %w", err)
			return nil, err
		}

		urls = append(urls, *u)
	}
	return urls, nil
}

func scrapUrl(client *http.Client, host *url.URL) ([]url.URL, error) {
	resp, err := client.Get(host.String())
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", host.String(), err)
		return nil, err
	}
	defer resp.Body.Close()

	urls := Scrap(host, resp.Body)
	return urls, nil
}

func MeasureMaxConnections(urls []url.URL) int {
	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0
	rUrls := make([]url.URL, 0)
	client := http.Client{}
	var i, r, a int

	// Make requests until an error occurs
	for i = range urls {
		u := &urls[i]
		sUrls, err := scrapUrl(&client, u)
		if err != nil {
			break
		}

		// Find uri from same host
		var rUrl *url.URL
		for j := range sUrls {
			sUrl := &sUrls[j]
			if u.Host == sUrl.Host {
				rUrl = sUrl
				break
			}
		}
		// Re-use original uri if a new uri was not scraped
		if rUrl == nil {
			rUrl = u
		}
		rUrls = append(rUrls, *rUrl)
		r++
	}

	// Confirm the NAT mappings are still active
	for i = 0; i < r; i++ {
		rUrl := &rUrls[i]
		_, err := scrapUrl(&client, rUrl)
		if err != nil {
			break
		}
		a++
	}

	return a
}

func main() {
	fmt.Println("Measure max connections! Please provide a path to a url list")
	for {
		var path string

		_, err := fmt.Scanln(&path)
		if err != nil {
			fmt.Println("Failed to read input file path:", err)
			break
		}

		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("Could not open %v: %v", path, err)
			break
		}

		urls, err := readUrls(f)
		if err != nil {
			fmt.Printf("Failed to read urls: %v: %v", path, err)
			break
		}

		nConns := MeasureMaxConnections(urls)
		fmt.Println("Max connections is", nConns)
		fmt.Println()
	}
}
