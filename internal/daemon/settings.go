package daemon

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/hongzio/gero/internal/pf"
)

// Settings is config.toml, read once when the daemon starts.
type Settings struct {
	// DNS are the nameservers isolations, and the builds of their image,
	// resolve through instead of the network's gateway. The gateway answers
	// by forwarding to the Mac's own resolver, and a local DNS proxy there
	// (Cloudflare WARP's 127.0.2.2, ...) never answers it: apt fails in the
	// build and nothing resolves in the isolation. Taken by containers made
	// from then on; an existing isolation takes it on its next upgrade.
	DNS []string `toml:"dns"`
}

// LoadSettings reads path; a missing file is every default. A nameserver
// the fence keeps isolations from (the Mac, its LAN) is refused here, not
// found later as an isolation where nothing resolves.
func LoadSettings(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	md, err := toml.Decode(string(b), &s)
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return s, fmt.Errorf("%s: unknown setting %q", path, un[0].String())
	}
	for _, ip := range s.DNS {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return s, fmt.Errorf("%s: dns %q is not an IP address", path, ip)
		}
		if pf.Fenced(a) {
			return s, fmt.Errorf("%s: dns %s is not on the internet, and the network fence keeps isolations off the Mac and its LAN", path, ip)
		}
	}
	return s, nil
}
