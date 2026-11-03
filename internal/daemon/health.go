package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/store"
)

// A container can run and still do nothing: its memory full, every
// process in it waits on pages the kernel keeps taking back, and
// `container inspect` still says running. So a running isolation is also
// looked into: how much memory it has left, and whether a command there
// finishes at all.

const (
	healthTTL     = 10 * time.Second
	healthTimeout = 5 * time.Second
	// memoryFullPct: an isolation with less of its memory free than this
	// has no room left to work in, or is working out of its swap.
	memoryFullPct = 5
)

// isoHealth is what was last seen inside an isolation.
type isoHealth struct {
	at           time.Time
	label, about string
}

// healthScript prints the memory figures, then the process using the most.
const healthScript = `cat /proc/meminfo; echo ---; ps -eo rss=,comm= --sort=-rss 2>/dev/null | head -1`

// checkHealth fills in Health for the running isolations among isos,
// looking into those not looked into in the last healthTTL, together.
func (m *Manager) checkHealth(ctx context.Context, isos []api.Isolation) {
	var wg sync.WaitGroup
	for i := range isos {
		v := &isos[i]
		if v.State != "running" {
			continue
		}
		m.mu.Lock()
		h, ok := m.health[v.ID]
		m.mu.Unlock()
		if ok && time.Since(h.at) < healthTTL {
			v.Health, v.HealthDetail = h.label, h.about
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := m.probeHealth(ctx, v.Container)
			if ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			was := m.health[v.ID]
			m.health[v.ID] = h
			m.mu.Unlock()
			if h.label != was.label {
				if h.label == "" {
					m.log.Printf("isolation %s: healthy again", v.Name)
				} else {
					m.log.Printf("isolation %s: %s: %s", v.Name, h.label, h.about)
				}
			}
			v.Health, v.HealthDetail = h.label, h.about
		}()
	}
	wg.Wait()
}

func (m *Manager) probeHealth(ctx context.Context, container string) isoHealth {
	pctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	start := time.Now()
	out, err := m.cli.RunAsAgent(pctx, container, "sh", "-c", healthScript)
	h := isoHealth{at: time.Now()}
	if errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		h.label = api.HealthStuck
		h.about = fmt.Sprintf("a command there did not finish in %s", healthTimeout)
		return h
	}
	if err != nil {
		// Not knowing is not a fault: the shim and the agents say
		// for themselves when they cannot be reached.
		return h
	}
	h.label, h.about = judgeHealth(out, time.Since(start))
	return h
}

// judgeHealth reads healthScript's output.
func judgeHealth(out string, took time.Duration) (label, about string) {
	mem, top, _ := strings.Cut(out, "\n---")
	var total, avail int64
	for _, line := range strings.Split(mem, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	if total == 0 || avail*100 >= total*memoryFullPct {
		return "", ""
	}
	about = fmt.Sprintf("%s of %s free", bytesShort(avail), bytesShort(total))
	if f := strings.Fields(top); len(f) >= 2 {
		if kb, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			about += fmt.Sprintf("; %s uses %s", strings.Join(f[1:], " "), bytesShort(kb*1024))
		}
	}
	if took > time.Second {
		about += fmt.Sprintf("; looking took %s", took.Round(100*time.Millisecond))
	}
	return api.HealthMemoryFull, about
}

func bytesShort(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%dK", n>>10)
}

// forgetHealth drops what was seen inside an isolation whose VM is gone.
func (m *Manager) forgetHealth(id string) {
	m.mu.Lock()
	delete(m.health, id)
	m.mu.Unlock()
}

// killIfStuck kills a running VM that does not finish a command, as gero
// iso kill does, before its container is replaced or removed: container's
// rm waits on such a VM for good. Its agents end either way; what its
// disks had not written is lost, as it could not be written.
func (m *Manager) killIfStuck(ctx context.Context, iso store.Isolation) {
	if state, err := m.cli.State(ctx, iso.Container); err != nil || state != "running" {
		return
	}
	h := m.probeHealth(ctx, iso.Container)
	if h.label != api.HealthStuck {
		return
	}
	pids, err := m.cli.Kill(ctx, iso.Container)
	if err != nil {
		m.log.Printf("isolation %s: %s (%s); killing it before its container is replaced: %v", iso.Name, h.label, h.about, err)
		return
	}
	m.forgetHealth(iso.ID)
	m.log.Printf("isolation %s: %s (%s), so killed before its container is replaced (processes %v)", iso.Name, h.label, h.about, pids)
}
