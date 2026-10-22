package api

import "testing"

func TestTeamRunExecutorConcurrency(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultTeamRunExecutorConcurrency},
		{"  ", defaultTeamRunExecutorConcurrency},
		{"1", 1},
		{" 256 ", 256},
		{"4096", maxTeamRunExecutorConcurrency},
		{"0", defaultTeamRunExecutorConcurrency},
		{"-3", defaultTeamRunExecutorConcurrency},
		{"4097", defaultTeamRunExecutorConcurrency},
		{"many", defaultTeamRunExecutorConcurrency},
	}
	for _, tc := range cases {
		t.Setenv(teamRunExecutorConcurrencyEnv, tc.raw)
		if got := teamRunExecutorConcurrency(); got != tc.want {
			t.Errorf("%s=%q: got %d, want %d", teamRunExecutorConcurrencyEnv, tc.raw, got, tc.want)
		}
	}
}
