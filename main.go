package main

import (
	"fmt"
	"net/http"
	"net/url"
)

func scrapUrl(client *http.Client, host url.URL) ([]url.URL, error) {
	resp, err := client.Get(host.String())
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", host.String(), err)
		return nil, err
	}
	defer resp.Body.Close()

	urls := Scrap(&host, resp.Body)
	return urls, nil
}

func main() {
	client := http.Client{}

	fmt.Println("Lets's scrap some URLs!")
	for {
		var ln string

		_, err := fmt.Scanln(&ln)
		if err != nil {
			fmt.Println("Failed to read Input Url: ", err)
			continue
		}

		u, err := url.Parse(ln)
		if err != nil {
			fmt.Println("Entered line was not a valid url: ", err)
			continue
		}

		urls, err := scrapUrl(&client, *u)
		if err != nil {
			fmt.Println("Failed to scrap server: ", err)
			continue
		}
		fmt.Println("Scraped the links: ")
		for _, u := range urls {
			fmt.Println(" ", u.String())
			fmt.Println("  ", u)
		}
		fmt.Println()
	}
}
