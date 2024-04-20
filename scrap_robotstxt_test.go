package main

import (
	"reflect"
	"testing"
	"time"
)

func TestScrapRobotsTxt(t *testing.T) {
	testcases := map[string]struct {
		in  string
		out map[string]string
	}{
		"No wildcards": {
			in:  "testdata/no_wildcard_robots.txt",
			out: map[string]string{},
		},
		"Wildcard User-agent": {
			in:  "testdata/wildcard_robots.txt",
			out: map[string]string{"Crawl-delay": time.Microsecond.String()},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			out := map[string]string{}
			if delay, found := scrapRobotsTxt(openFile(t, tc.in)); found {
				out["Crawl-delay"] = delay.String()
			}

			if !reflect.DeepEqual(tc.out, out) {
				t.Error("Failed to parse values: ", tc.out, " != ", out)
			}
		})
	}
}
