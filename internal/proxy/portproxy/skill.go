package portproxy

import (
	"fmt"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

const skillHead = `---
name: gero-port
description: Which ports of this gero isolation the operator's Mac reaches, and
  at what address there. Use when running a dev server, a web app or any server
  the operator wants to open, or when asked for a URL "on the Mac", "in my
  browser" or "localhost".
---

# Ports the Mac reaches

The operator's Mac connects to these ports of this isolation, on its own
127.0.0.1 only (nothing else on their network does):

`

const skillTail = `
- Listen on the port as it is listed here, on localhost or on every address;
  both are reached. Another port is not reached from the Mac: ask the operator
  to forward it (gero's port service) rather than looking for a way around.
- Give the operator the Mac's address, not this isolation's: the URL to open is
  the one listed above.
- What you serve runs in the operator's browser on their Mac, as a page from
  127.0.0.1, which browsers trust more than one from the internet. Do not serve
  pages that send requests to other addresses on the Mac or their network, or
  that ask for passwords or tokens.
- A connection only ever starts on the Mac: nothing here can connect to the Mac
  through these ports.
`

func skillBody(fws []proxy.Forward) string {
	var b strings.Builder
	b.WriteString(skillHead)
	if len(fws) == 0 {
		b.WriteString("None now.\n")
	}
	for _, f := range fws {
		if f.HTTP {
			fmt.Fprintf(&b, "- port %d here is http://127.0.0.1:%d on the Mac (HTTP; WebSockets pass)\n", f.Port, f.HostPort)
		} else {
			fmt.Fprintf(&b, "- port %d here is 127.0.0.1:%d on the Mac (TCP, bytes unchanged)\n", f.Port, f.HostPort)
		}
	}
	b.WriteString(skillTail)
	return b.String()
}
