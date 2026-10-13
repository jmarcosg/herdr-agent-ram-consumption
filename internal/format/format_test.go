package format

import "testing"

func TestSize(t *testing.T) {
	cases := map[uint64]string{
		0:             "0 MB",
		300 * MB:      "300 MB",
		300*MB + MB/2: "301 MB",
		1023 * MB:     "1023 MB",
		1024*MB - 1:   "1.0 GB",
		GB + GB/5:     "1.2 GB",
		GB + GB/4:     "1.3 GB",
		10 * GB:       "10.0 GB",
	}
	for in, want := range cases {
		if got := Size(in); got != want {
			t.Errorf("Size(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSeverity(t *testing.T) {
	cases := map[int]string{
		0:   "ok",
		74:  "ok",
		75:  "warn",
		89:  "warn",
		90:  "crit",
		100: "crit",
	}
	for in, want := range cases {
		if got := Severity(in); got != want {
			t.Errorf("Severity(%d) = %q, want %q", in, got, want)
		}
	}
}
