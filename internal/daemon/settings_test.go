package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func settingsFile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSettingsMissingIsDefault(t *testing.T) {
	s, err := LoadSettings(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil || s.DNS != nil {
		t.Fatalf("%+v, %v", s, err)
	}
}

func TestSettingsDNS(t *testing.T) {
	s, err := LoadSettings(settingsFile(t, `dns = ["1.1.1.1", "2606:4700:4700::1111"]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.1.1.1", "2606:4700:4700::1111"}; !reflect.DeepEqual(s.DNS, want) {
		t.Fatalf("%v", s.DNS)
	}
}

// What the fence blocks would leave an isolation where nothing resolves.
func TestSettingsRefuseWhatTheFenceBlocks(t *testing.T) {
	for _, bad := range []string{"127.0.2.2", "192.168.1.1", "10.0.0.53", "100.100.100.100", "fd00::53", "::1", "one.one.one.one"} {
		if _, err := LoadSettings(settingsFile(t, `dns = ["`+bad+`"]`)); err == nil {
			t.Errorf("%s taken", bad)
		}
	}
}

func TestSettingsRefuseAnUnknownKey(t *testing.T) {
	_, err := LoadSettings(settingsFile(t, `nameservers = ["1.1.1.1"]`))
	if err == nil || !strings.Contains(err.Error(), "nameservers") {
		t.Fatalf("%v", err)
	}
}
