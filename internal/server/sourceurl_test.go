package server

import "testing"

// The source repository is advertised in nodeinfo 2.1, the v2 instance entity
// and the OAuth server metadata.
func TestSourceURL(t *testing.T) {
	la := startLiveApp(t)
	api := &apiClient{t: t, base: la.base}
	sw := api.ok("GET", "/nodeinfo/2.1", nil).(map[string]any)["software"].(map[string]any)
	if sw["repository"] != SourceURL || sw["homepage"] != SourceURL {
		t.Errorf("nodeinfo software: %v", sw)
	}
	if got := api.ok("GET", "/api/v2/instance", nil).(map[string]any)["source_url"]; got != SourceURL {
		t.Errorf("instance source_url: %v", got)
	}
	if got := api.ok("GET", "/.well-known/oauth-authorization-server", nil).(map[string]any)["service_documentation"]; got != SourceURL {
		t.Errorf("oauth service_documentation: %v", got)
	}
}
