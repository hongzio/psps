package isolation

import (
	"reflect"
	"testing"
)

// As container 1.6 reports a container started with two volumes, a
// read-only share and a writable one.
const inspectWithShares = `[{"configuration": {"mounts": [
 {"destination": "/home/agent", "options": [], "source": "/Users/x/Library/Application Support/com.apple.container/volumes/gero-dev-home/volume.img",
  "type": {"volume": {"name": "gero-dev-home", "format": "ext4"}}},
 {"destination": "/shared/b", "options": [], "source": "/Users/x/notes", "type": {"virtiofs": {}}},
 {"destination": "/shared/a", "options": ["ro"], "source": "/Users/x/docs", "type": {"virtiofs": {}}}
]}, "id": "gero-dev", "status": {"state": "running"}}]`

func TestMountsAreTheSharesNotTheVolumes(t *testing.T) {
	got, err := parseMounts(inspectWithShares)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{{Source: "/Users/x/notes", Target: "/shared/b"}, {Source: "/Users/x/docs", Target: "/shared/a", ReadOnly: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if _, err := parseMounts("[]"); err == nil {
		t.Fatal("no container read as no mounts")
	}
}
