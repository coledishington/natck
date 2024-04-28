package main

import (
	"html/template"
	"path"
	"slices"
	"testing"
	"time"
)

type rule struct {
	Token, Value string
}

type record struct {
	Agents []string
	Rules  []rule
}

func (r *record) Clone() *record {
	return &record{
		Agents: slices.Clone(r.Agents),
		Rules:  slices.Clone(r.Rules),
	}
}

var defaultRobotsTxtRecord = record{
	Agents: []string{"*"},
	Rules: []rule{
		{Token: ruleCrawlDelay, Value: "0.000001"},
		{Token: ruleAllow, Value: "/"},
	},
}

func makeRobotsTxt(t *testing.T, records []record, dPath string) {
	txt := `# This is a template generated robot file
{{- range .}}
	{{- range $agent := .Agents}}
User-agent: {{$agent}}
	{{- end}}
	{{- range $rule := .Rules}}
{{$rule.Token}}: {{$rule.Value}}
	{{- end}}
{{end}}`

	dest := createFile(t, dPath)

	tpl := template.Must(template.New("txt").Parse(txt))
	err := tpl.ExecuteTemplate(dest, "txt", records)
	if err != nil {
		t.Fatal("Failed to fill in robot template:", err)
	}
}

func TestScrapRobotsTxt(t *testing.T) {
	root := t.TempDir()

	mixedPath := path.Join(root, "mixed.txt")
	makeRobotsTxt(t, []record{{
		Agents: []string{"*"},
		Rules: []rule{
			{Token: ruleAllow, Value: "/tmp/a.html"},
			{Token: ruleDisallow, Value: "/tmp/"},
			{Token: ruleAllow, Value: "/"},
		},
	}}, mixedPath)

	testcases := map[string]struct {
		in              string
		outCrawlDelay   time.Duration
		allowedPaths    []string
		disallowedPaths []string
	}{
		"No wildcards": {
			in:            "testdata/no_wildcard_robots.txt",
			outCrawlDelay: 0,
			allowedPaths:  []string{"/tmp", "/public"},
		},
		"Wildcard User-agent": {
			in:            "testdata/wildcard_robots.txt",
			outCrawlDelay: time.Microsecond,
			allowedPaths:  []string{"/tmp", "/public"},
		},
		"Mixed Allow & Disallow": {
			in:              mixedPath,
			outCrawlDelay:   0,
			allowedPaths:    []string{"/tmp", "/tmp/a.html", "/public/index.html"},
			disallowedPaths: []string{"/tmp/", "/tmp/b.html"},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			txt := scrapRobotsTxt(openFile(t, tc.in))

			delay, _ := txt.crawlDelay()
			if delay != tc.outCrawlDelay {
				t.Error("Parsed Crawl-delay is '", delay, "', should be '", tc.outCrawlDelay, "'")
			}

			for _, p := range tc.allowedPaths {
				if !txt.pathAllowed(p) {
					t.Error("path ", p, ", should be allowed")
				}
			}

			for _, p := range tc.disallowedPaths {
				if txt.pathAllowed(p) {
					t.Error("path ", p, ", should be disallowed")
				}
			}
		})
	}
}
