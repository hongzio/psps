package isolation

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// assets holds the linux gero-shim, cross-compiled by `mise run shim` before
// the daemon is built. A plain `go build` embeds only the README, and image
// builds then fail with a message saying so.
//
//go:embed assets
var assets embed.FS

// Agent CLI versions are pinned: their protocols are what the drivers parse,
// so an upgrade is a decision, made here, with the driver tests rerun.
const (
	claudeCodeVersion = "2.1.311"
	codexVersion      = "0.164.0"
	// session-manager-plugin is the one binary ansible's aws_ssm needs that
	// no package manager carries; it needs no credentials of its own.
	ssmPluginVersion = "1.2.896.0"
)

const dockerfile = `FROM docker.io/library/node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git openssh-client curl procps ripgrep unzip postgresql-client \
 && rm -rf /var/lib/apt/lists/*
RUN case "$(dpkg --print-architecture)" in arm64) sub=ubuntu_arm64 ;; amd64) sub=ubuntu_64bit ;; esac \
 && curl -fsSLo /tmp/smp.deb "https://s3.amazonaws.com/session-manager-downloads/plugin/` + ssmPluginVersion + `/$sub/session-manager-plugin.deb" \
 && dpkg -i /tmp/smp.deb && rm /tmp/smp.deb
RUN npm install -g @anthropic-ai/claude-code@` + claudeCodeVersion + ` @openai/codex@` + codexVersion + ` \
 && npm cache clean --force
RUN usermod -l agent -d ` + AgentHome + ` -m node && groupmod -n agent node
# The credential proxies' side of the isolation lives in /etc/gero, written by
# ` + "`gero-shim sync`" + `: git's URL rewrite (included; git ignores it while absent)
# and the proxies' environment, for login and interactive shells alike.
RUN mkdir -p /etc/gero \
 && printf '[include]\n\tpath = /etc/gero/gitconfig\n' >> /etc/gitconfig \
 && printf '[ -r /etc/gero/env.sh ] && . /etc/gero/env.sh\n' > /etc/profile.d/gero.sh \
 && printf '[ -r /etc/gero/env.sh ] && . /etc/gero/env.sh\n' >> /etc/bash.bashrc
COPY gero-shim ` + shimPath + `
ENTRYPOINT ["` + shimPath + `", "serve", "--uid", "1000", "--gid", "1000", "--user", "agent", "--home", "` + AgentHome + `"]
`

func shimBinary() ([]byte, error) {
	b, err := assets.ReadFile("assets/gero-shim")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("this gero was built without the linux shim; build with `mise run build`")
	}
	return b, err
}

// ImageTag names the base image by its content, so a new shim or a new
// Dockerfile is a new image and an existing isolation keeps the one it has.
func ImageTag() (string, error) {
	shim, err := shimBinary()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(dockerfile))
	h.Write(shim)
	return "gero-base:" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// EnsureImage builds the base image unless it already exists.
func (c CLI) EnsureImage(ctx context.Context, progress io.Writer) (string, error) {
	tag, err := ImageTag()
	if err != nil {
		return "", err
	}
	if c.ImageExists(ctx, tag) {
		return tag, nil
	}
	shim, _ := shimBinary()
	dir, err := os.MkdirTemp("", "gero-image")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "gero-shim"), shim, 0o755); err != nil {
		return "", err
	}
	return tag, c.Build(ctx, tag, dir, progress)
}

// Names of what one isolation owns, from its ID.
func ContainerName(id string) string { return "gero-" + id }
func NetworkName(id string) string   { return "gero-" + id + "-net" }
func HomeVolume(id string) string    { return "gero-" + id + "-home" }
func StateVolume(id string) string   { return "gero-" + id + "-state" }

// Create starts a fresh container for the isolation with that ID. Its
// volumes are created if missing and reused if present, which is how a
// container is recreated without losing agents' sessions and workspaces.
func (c CLI) Create(ctx context.Context, id, image string, cpus int, memory, network string, mounts []Mount) error {
	for _, v := range []string{HomeVolume(id), StateVolume(id)} {
		if err := c.VolumeEnsure(ctx, v); err != nil {
			return err
		}
	}
	if network != "" {
		if err := c.NetworkEnsure(ctx, network); err != nil {
			return err
		}
	}
	return c.Run(ctx, RunSpec{
		Name:    ContainerName(id),
		Image:   image,
		CPUs:    cpus,
		Memory:  memory,
		Network: network,
		Volumes: [][2]string{
			{HomeVolume(id), AgentHome},
			{StateVolume(id), StateRoot},
		},
		Mounts: mounts,
		Labels: map[string]string{"dev.gero.isolation": id},
	})
}
