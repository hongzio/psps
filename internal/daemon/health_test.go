package daemon

import (
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
)

func TestAnIsolationWithItsMemoryFullIsSaidToBe(t *testing.T) {
	full := "MemTotal:       12373504 kB\nMemFree:           55296 kB\nMemAvailable:      77500 kB\n---\n11474872 compile"
	label, about := judgeHealth(full, 2*time.Second)
	if label != api.HealthMemoryFull || about != "75M of 11.8G free; compile uses 10.9G; looking took 2s" {
		t.Fatalf("full: %q %q", label, about)
	}
	roomy := "MemTotal:       12373504 kB\nMemAvailable:   10936320 kB\n---\n144316 claude"
	if label, about := judgeHealth(roomy, 0); label != "" || about != "" {
		t.Fatalf("roomy: %q %q", label, about)
	}
	if label, _ := judgeHealth("cat: /proc/meminfo: No such file\n---\n", 0); label != "" {
		t.Fatalf("unknown: %q", label)
	}
}
