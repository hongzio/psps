package daemon

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// iconColors are the colors a line's icon may be drawn in, the sidebar's
// own; "" is the text's.
var iconColors = []string{"", "red", "orange", "yellow", "green", "teal", "blue", "violet", "pink"}

// An icon's name is a Lucide one's, such as flask-conical; which the web UI
// offers is its own business.
var iconNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func checkIcon(i store.Icon) error {
	if i.Name == "" {
		return nil
	}
	if len(i.Name) > 40 || !iconNameRE.MatchString(i.Name) {
		return badRequest(fmt.Errorf("icon %q: a name is lowercase words joined by hyphens", i.Name))
	}
	if !slices.Contains(iconColors, i.Color) {
		return badRequest(fmt.Errorf("icon color %q: one of red, orange, yellow, green, teal, blue, violet, pink, or none", i.Color))
	}
	return nil
}

// withIcons gives each agent its line's icon.
func (m *Manager) withIcons(as []api.Agent) error {
	icons, err := m.store.LineIcons()
	if err != nil {
		return err
	}
	for i, a := range as {
		if ic, ok := icons[cmp.Or(a.Line, a.ID)]; ok {
			as[i].Icon = &ic
		}
	}
	return nil
}
