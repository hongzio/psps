class Psps < Formula
  desc "Run Claude Code and Codex agents inside Apple container isolations"
  homepage "https://github.com/hongzio/psps"
  url "https://github.com/hongzio/psps/releases/download/v@VERSION@/psps-@VERSION@-darwin-arm64.tar.gz"
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
    bin.install "psps"
  end

  def caveats
    <<~EOS
      psps runs its daemon as a LaunchAgent of yours; any psps command starts it,
      and after `brew upgrade psps` the next one moves it onto the new version.

      Once, for the network fence that keeps isolations off the Mac and the LAN:
        sudo psps pf install
      The fence helper is a copy of psps. After an upgrade, run that again only
      if psps says to (`psps iso ls`, `psps pf status`).

      Before `brew uninstall psps`:
        psps daemon uninstall
        sudo psps pf uninstall
    EOS
  end

  test do
    assert_equal "psps #{version}", shell_output("#{bin}/psps version").strip
  end
end
