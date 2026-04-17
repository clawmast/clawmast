//go:build unix

package supervisor

import "testing"

func TestClassifyExitCode(t *testing.T) {
	cases := []struct {
		name string
		info ExitInfo
		want Classification
	}{
		{"zero", ExitInfo{Code: 0}, ClassGraceful},
		{"unexpected-low", ExitInfo{Code: 1}, ClassUnexpected},
		{"unexpected-mid", ExitInfo{Code: 42}, ClassUnexpected},
		{"unexpected-high", ExitInfo{Code: 63}, ClassUnexpected},
		{"no-restart", ExitInfo{Code: 64}, ClassNoRestart},
		{"rollback", ExitInfo{Code: 65}, ClassRollback},
		{"reserved-66", ExitInfo{Code: 66}, ClassUnexpected},
		{"reserved-127", ExitInfo{Code: 127}, ClassUnexpected},
		{"out-of-range-high", ExitInfo{Code: 200}, ClassUnexpected},
		{"negative", ExitInfo{Code: -1}, ClassUnexpected},
		{"signaled-sigterm", ExitInfo{Code: 15, Signaled: true}, ClassUnexpected},
		{"signaled-sigkill", ExitInfo{Code: 9, Signaled: true}, ClassUnexpected},
		{"signaled-over-zero", ExitInfo{Code: 0, Signaled: true}, ClassUnexpected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyExitCode(c.info)
			if got != c.want {
				t.Fatalf("ClassifyExitCode(%+v) = %v, want %v", c.info, got, c.want)
			}
		})
	}
}

func TestClassificationString(t *testing.T) {
	cases := map[Classification]string{
		ClassGraceful:   "graceful",
		ClassUnexpected: "unexpected",
		ClassNoRestart:  "no-restart",
		ClassRollback:   "rollback",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Classification(%d).String() = %q, want %q", c, got, want)
		}
	}
}
