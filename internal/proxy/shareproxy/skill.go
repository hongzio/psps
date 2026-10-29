package shareproxy

import (
	"fmt"
	"strings"

	"github.com/hongzio/gero/internal/isolation"
)

// The mounts say where the folders are; the skill says whose they are.

const skillHead = `---
name: gero-share
description: What the folders under /shared are, in this gero isolation - the
  operator's Mac's own folders, mounted in - and how to treat them. Use before
  reading or writing anything under /shared, or when asked about files "on the
  Mac", the "shared folder" or "the host".
---

# Folders shared with the Mac

Each folder under /shared is one of the operator's folders on their Mac, mounted
into this isolation. It is not a copy: what you read is what is on the Mac now,
and what you write lands there at once, as the operator's own files.

`

const skillTail = `
- A read-only folder refuses every write ("Read-only file system"). That is the
  operator's decision; ask them, rather than copying the files elsewhere to
  change them and passing that off as done.
- In a read-write folder, change only what you were asked to. Never add or edit
  what runs on the Mac by itself or by habit: git hooks (.git/hooks), .envrc,
  shell rc files, launch agents, editor tasks, scripts or Makefile targets the
  operator did not ask for. Do not create links (symlinks) there: on the Mac
  they point at the Mac's own paths.
- Do not put credentials or tokens in a shared folder.
- Work that is only yours belongs in your workspace, not here.
`

func skillBody(mounts []isolation.Mount, missing []string) string {
	var b strings.Builder
	b.WriteString(skillHead)
	if len(mounts) == 0 {
		b.WriteString("None is mounted now.\n")
	}
	for _, m := range mounts {
		mode := "read-write"
		if m.ReadOnly {
			mode = "read-only"
		}
		fmt.Fprintf(&b, "- %s is the Mac's %s, %s\n", m.Target, m.Source, mode)
	}
	for _, n := range missing {
		fmt.Fprintf(&b, "- not mounted now: %s\n", n)
	}
	b.WriteString(skillTail)
	return b.String()
}
