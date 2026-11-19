package daemon

import (
	"context"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
)

func TestAnAgentKeptFromArchivingIsNotArchived(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })

	on, off := true, false
	v, err := m.UpdateAgent(a.ID, api.UpdateAgent{NoArchive: &on})
	if err != nil || !v.NoArchive {
		t.Fatalf("keeping it: %v %+v", err, v.NoArchive)
	}
	if err := m.Archive(ctx, a.ID); err == nil {
		t.Fatal("archived although kept from archiving")
	}
	if s, _ := m.store.Agent(a.ID); s.ArchivedAt != nil {
		t.Fatal("archived in the store")
	}

	// A new session of the line is kept too.
	renewed, err := m.Renew(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the new session", func() bool { return status(m, renewed.ID) == event.StatusIdle })
	if v, _ := m.viewOf(renewed); !v.NoArchive {
		t.Fatal("the new session is not kept")
	}
	if err := m.Archive(ctx, renewed.ID); err == nil {
		t.Fatal("the new session archived although kept")
	}

	if _, err := m.UpdateAgent(renewed.ID, api.UpdateAgent{NoArchive: &off}); err != nil {
		t.Fatal(err)
	}
	if err := m.Archive(ctx, renewed.ID); err != nil {
		t.Fatalf("archiving once unticked: %v", err)
	}
}
