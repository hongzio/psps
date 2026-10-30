class Gero < Formula
  desc "Run Claude Code and Codex agents inside Apple container isolations"
  homepage "https://github.com/hongzio/gero"
  url "https://github.com/hongzio/gero/releases/download/v@VERSION@/gero-@VERSION@-darwin-arm64.tar.gz"
  # Said outright: from the file name Homebrew would read the "64" of arm64.
  version "@VERSION@"
  sha256 "@SHA256@"

  # The release's own binary (scripts/release.sh), so no Go toolchain here:
  # pure Go, with the linux shim it puts in the isolations embedded. The
  # isolations are Apple's container VMs and the shim is linux/arm64: Apple
  # silicon only.
  depends_on arch: :arm64
  depends_on "container"
  depends_on :macos

  def install
    bin.install "gero"
  end

  def caveats
    <<~EOS
      gero runs its daemon as a LaunchAgent of yours; any gero command starts it,
      and after `brew upgrade gero` the next one moves it onto the new version.

      Once, for the network fence that keeps isolations off the Mac and the LAN:
        sudo gero pf install
      The fence helper is a copy of gero. After an upgrade, run that again only
      if gero says to (`gero iso ls`, `gero pf status`).

      Before `brew uninstall gero`:
        gero daemon uninstall
        sudo gero pf uninstall
    EOS
  end

  test do
    assert_equal "gero #{version}", shell_output("#{bin}/gero version").strip
  end
end
