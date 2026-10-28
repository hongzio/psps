package isolation

import "testing"

// What container itself took or refused, measured with `container create
// --memory` on 1.6.1 (memoryInBytes, where it took one), except bare
// numbers: container takes those as bytes.
func TestMemoryBytesFollowsContainer(t *testing.T) {
	for in, want := range map[string]uint64{"": 0, "4G": 4 << 30, "4g": 4 << 30, "4GB": 4 << 30, "4GiB": 4 << 30, "4 G": 4 << 30,
		"4096M": 4 << 30, "1.5G": 1610612736, "512m": 512 << 20, "200M": 200 << 20, "1T": 1 << 40} {
		if got, err := MemoryBytes(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"4096", "8589934592", "0", "abc", "199M", "-1G"} {
		if _, err := MemoryBytes(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}
