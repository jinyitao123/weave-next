package api

import (
	"net/http"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestTeamManagementRoutesAreRegistered(t *testing.T) {
	server := NewServer(&config.Config{}, nil, nil)
	routes := map[string]bool{}
	for _, route := range server.Echo.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodPut + " /v1/teams/:id/profile",
		http.MethodPost + " /v1/teams/:id/workers",
		http.MethodDelete + " /v1/teams/:id/workers/:worker",
	} {
		if !routes[want] {
			t.Fatalf("missing team management route %s", want)
		}
	}
}
