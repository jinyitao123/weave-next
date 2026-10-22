package api

import (
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestTeamRunCapacityReportsThisProcess(t *testing.T) {
	if got := (&Server{}).teamRunCapacity(); got != (teamRunCapacity{}) {
		t.Fatalf("unwired workers reported capacity %+v", got)
	}
	server := &Server{teamRunWorkers: &teamrun.Workers{ExecutorConcurrency: 256, Pooled: true}}
	t.Setenv(teamRunWorkersDisableEnv, "")
	if got, want := server.teamRunCapacity(), (teamRunCapacity{ExecutorConcurrency: 256, ExecutorMode: "pooled", Running: true}); got != want {
		t.Fatalf("capacity = %+v, want %+v", got, want)
	}
	t.Setenv(teamRunWorkersDisableEnv, "true")
	if got := server.teamRunCapacity(); got.Running || got.ExecutorConcurrency != 256 {
		t.Fatalf("disabled workers reported %+v", got)
	}
}

func TestTeamRunExecutorConcurrency(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		invalid bool
	}{
		{raw: "", want: defaultTeamRunExecutorConcurrency},
		{raw: "  ", want: defaultTeamRunExecutorConcurrency},
		{raw: "1", want: 1},
		{raw: " 256 ", want: 256},
		{raw: "4096", want: maxTeamRunExecutorConcurrency},
		{raw: "0", invalid: true},
		{raw: "-3", invalid: true},
		{raw: "4097", invalid: true},
		{raw: "many", invalid: true},
	}
	for _, tc := range cases {
		t.Setenv(teamRunExecutorConcurrencyEnv, tc.raw)
		got, err := teamRunExecutorConcurrency()
		if tc.invalid {
			if err == nil {
				t.Errorf("%s=%q: accepted as %d, want an error", teamRunExecutorConcurrencyEnv, tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s=%q: got %d, %v; want %d", teamRunExecutorConcurrencyEnv, tc.raw, got, err, tc.want)
		}
	}
}
