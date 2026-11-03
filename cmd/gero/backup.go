package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hongzio/gero/internal/backup"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/store"
)

// backupCmd saves gero's files on the Mac to one archive, written beside
// FILE first so an old one is replaced only by a whole new one.
func backupCmd(p paths.Paths, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	pos := parse(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: gero backup FILE")
	}
	file := pos[0]
	f, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	sum, err := backup.Write(f, p)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), file); err != nil {
		return err
	}
	fmt.Printf("saved %d files (%s) to %s\n", sum.Files, byteSize(sum.Bytes), file)
	for _, s := range sum.Skipped {
		fmt.Printf("  left out %s: not a plain file\n", s)
	}
	fmt.Println("It holds the credentials: keep it as you would them. The isolations' volumes are not in it.")
	return nil
}

// restoreCmd puts an archive back, then says which isolations' volumes are
// missing from container: those start with empty homes.
func restoreCmd(p paths.Paths, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	force := fs.Bool("force", false, "replace the store that is here")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: gero restore FILE [--force]")
	}
	sum, err := backup.Restore(pos[0], p, *force)
	if errors.Is(err, backup.ErrExists) {
		return fmt.Errorf("%w (%s); --force does it", err, p.DB())
	}
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files (%s)\n", sum.Files, byteSize(sum.Bytes))

	st, err := store.Open(p.DB())
	if err != nil {
		return err
	}
	isos, err := st.Isolations()
	st.Close()
	if err != nil {
		return err
	}
	cli, err := isolation.FindCLI()
	if err != nil {
		fmt.Printf("could not check the isolations' volumes: %v\n", err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, iso := range isos {
			for _, v := range []string{isolation.HomeVolume(iso.ID), isolation.StateVolume(iso.ID)} {
				if !cli.VolumeExists(ctx, v) {
					fmt.Printf("  isolation %s: volume %s is missing; it starts empty\n", iso.Name, v)
				}
			}
		}
	}
	fmt.Println("Start the daemon with: gero")
	return nil
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
