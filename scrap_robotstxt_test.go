package main

import (
	"html/template"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"
)

type record struct {
	Agents []string
	Rules  map[string]string
}

func (r *record) Clone() *record {
	return &record{
		Agents: slices.Clone(r.Agents),
		Rules:  maps.Clone(r.Rules),
	}
}

var defaultRobotsTxtRecord = record{
	Agents: []string{"*"},
	Rules: map[string]string{
		"Crawl-delay": "0.000001",
	},
}

func makeRobotsTxt(t *testing.T, records []record, dPath string) {
	txt := `# This is a template generated robot file
{{- range .}}
	{{- range $agent := .Agents}}
User-agent: {{$agent}}
	{{- end}}
	{{- range $token, $value := .Rules}}
{{$token}}: {{$value}}
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
