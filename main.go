package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"
)

type roundtrip struct {
	connId  uint
	client  *http.Client
	url     *url.URL
	nextUrl *url.URL
}

type connection struct {
	id          uint
	client      *http.Client
	url         *url.URL
	nextUrl     *url.URL
	lastRequest time.Time
	lastReply   time.Time
}

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

	// Parse urls from content
	urls := Scrap(host, resp.Body)

	// Parse url from redirect
	location, err := resp.Location()
	if err != nil {
		var found *url.URL
		for i := range urls {
			if urls[i] == *location {
				found = &urls[i]
				break
			}
		}
		if found == nil {
			urls = append(urls, *location)
		}
	}

	// Find urls for the scraped host
	hostUrls := []url.URL{}
	for i := range urls {
		if urls[i] != *host && urls[i].Host == host.Host {
			hostUrls = append(hostUrls, urls[i])
		}
	}
	return hostUrls, nil
}

func makeClient() *http.Client {
	// Need a unique transport per http.Client to avoid re-using the same
	// connections, otherwise the NAT count will be wrong.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 0
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Do not follow re-directs
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}
	return &client
}

func makeConnection(h *url.URL) *connection {
	c := &connection{
		client:  makeClient(),
		url:     h,
		nextUrl: h,
	}
	return c
}

func scrapConnection(r *roundtrip) {
	sUrls, err := scrapUrl(r.client, r.url)
	if err != nil {
		r.nextUrl = nil
		return
	}

	if len(sUrls) > 0 {
		r.nextUrl = &sUrls[0]
	}
}

func scrapConnections(wg *sync.WaitGroup, pendingScraps <-chan *roundtrip, scraped chan<- *roundtrip, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var r *roundtrip
		var ok bool

		/* Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		/* Stop blocking if canceled */
		select {
		case r, ok = <-pendingScraps:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		scrapConnection(r)
		select {
		case scraped <- r:
		case <-cancel:
			return
		}
	}
}

func MeasureMaxConnections(urls []url.URL) int {
	nWorkers := 0
	scrapRequest := make(chan *roundtrip)
	scrapedReply := make(chan *roundtrip)
	stopScraping := make(chan struct{})
	scapersCounter := sync.WaitGroup{}

	pendingConns := make([]*connection, 0)
	for i := range urls {
		c := makeConnection(&urls[i])
		c.id = uint(i)
		pendingConns = append(pendingConns, c)
	}

	scrapersEstimate := min(len(urls), 2*runtime.NumCPU())
	scrapersEstimate = max(scrapersEstimate, int(len(urls)/100))
	for i := 0; i < scrapersEstimate; i++ {
		scapersCounter.Add(1)
		go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopScraping)
		nWorkers += 1
	}

	activeConns := make([]*connection, 0)
	failedConns := make([]*connection, 0)
	for {
		anyFreeScrapers := true
		activeConnsScraped := 0
		/* Send off any new packets before http keep-alive or NAT mapping expires */
		for i := range activeConns {
			if !anyFreeScrapers {
				break
			}

			c := activeConns[i]
			if time.Since(c.lastReply) < 3500*time.Millisecond || time.Since(c.lastRequest) < 1000*time.Millisecond {
				continue
			}

			select {
			case scrapRequest <- &roundtrip{connId: c.id, client: c.client, url: c.url, nextUrl: c.nextUrl}:
				c.lastRequest = time.Now()
				activeConnsScraped++
			default:
				anyFreeScrapers = false
			}
		}

		pendingConnsScraped := 0
		/* Send off first packets of pending connections */
		for {
			if !anyFreeScrapers || len(pendingConns) == 0 {
				break
			}

			c := pendingConns[0]
			select {
			case scrapRequest <- &roundtrip{connId: c.id, client: c.client, url: c.url, nextUrl: c.nextUrl}:
				pendingConns = pendingConns[1:]
				pendingConnsScraped++
			default:
				anyFreeScrapers = false
			}
		}

		scrapedReplies := 0
		/* Collect completed connections */
		for more := true; more; {
			select {
			case r, ok := <-scrapedReply:
				if !ok {
					more = false
					break
				}

				scrapedReplies++
				i := slices.IndexFunc(activeConns, func(c *connection) bool {
					return r.connId == c.id
				})
				if i == -1 {
					c := &connection{id: r.connId, client: r.client, url: r.url, nextUrl: r.nextUrl}
					c.lastReply = time.Now()
					if c.nextUrl == nil {
						failedConns = append(failedConns, c)
					} else {
						activeConns = append(activeConns, c)
					}
					break
				}
				if r.nextUrl == nil {
					failedConns = append(failedConns, activeConns[i])
					activeConns = slices.Delete(activeConns, i, i+1)
				} else {
					c := activeConns[i]
					c.nextUrl = r.nextUrl
					c.lastReply = time.Now()
				}
			default:
				more = false
			}
		}

		if (len(activeConns) + len(failedConns)) == len(urls) {
			break
		}

		/* Avoid tightly looping */
		if 8*activeConnsScraped > pendingConnsScraped {
			for j := 0; j < 8*activeConnsScraped; j++ {
				scapersCounter.Add(1)
				go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopScraping)
				nWorkers += 1
			}
		} else if (activeConnsScraped + pendingConnsScraped + scrapedReplies) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	close(stopScraping)
	close(scrapRequest)
	scapersCounter.Wait()
	close(scrapedReply)
	return len(activeConns)
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
