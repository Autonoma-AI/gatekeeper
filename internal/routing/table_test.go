package routing

import "testing"

func newTestTable() *Table {
	return NewTable(map[string]Upstream{
		"WEB.preview.test": {Namespace: "preview-acme-pr-7", Service: "web", Port: 3000},
		"api.preview.test": {Namespace: "preview-acme-pr-7", Service: "api", Port: 8080},
	})
}

func TestResolve(t *testing.T) {
	tbl := newTestTable()

	tests := []struct {
		name        string
		host        string
		wantService string
		wantOK      bool
	}{
		{"exact", "api.preview.test", "api", true},
		{"uppercase normalised", "WEB.preview.test", "web", true},
		{"host with port", "api.preview.test:443", "api", true},
		{"mixed case with port", "Api.Preview.Test:8443", "api", true},
		{"unknown host", "nope.preview.test", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up, ok := tbl.Resolve(tt.host)
			if ok != tt.wantOK {
				t.Fatalf("Resolve(%q) ok = %v, want %v", tt.host, ok, tt.wantOK)
			}
			if ok && up.Service != tt.wantService {
				t.Fatalf("Resolve(%q) service = %q, want %q", tt.host, up.Service, tt.wantService)
			}
		})
	}
}

func TestUpstreamURL(t *testing.T) {
	got := Upstream{Namespace: "preview-acme-pr-7", Service: "web", Port: 3000}.URL()
	want := "http://web.preview-acme-pr-7.svc.cluster.local:3000"
	if got != want {
		t.Fatalf("URL() = %q, want %q", got, want)
	}
}
