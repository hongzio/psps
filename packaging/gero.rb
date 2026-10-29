class Gero < Formula
  desc "Run Claude Code and Codex agents inside Apple container isolations"
  homepage "https://github.com/hongzio/gero"
  url "https://github.com/hongzio/gero/archive/refs/tags/v@VERSION@.tar.gz"
  sha256 "@SHA256@"
  head "https://github.com/hongzio/gero.git", branch: "main"

  depends_on "go" => :build
  # The isolations are Apple's container VMs and the shim gero puts in them
  # is linux/arm64: Apple silicon only.
  depends_on arch: :arm64
  depends_on "container"
  depends_on :macos

  def install
    ENV["CGO_ENABLED"] = "0"
    # gero embeds the linux shim, so it is built first (`mise run shim`).
    with_env(GOOS: "linux", GOARCH: "arm64") do
      system "go", "build", "-trimpath", "-o", "internal/isolation/assets/gero-shim", "./cmd/gero-shim"
    end
    ldflags = "-X github.com/hongzio/gero/internal/api.Version=#{version}"
    system "go", "build", *std_go_args(ldflags:), "./cmd/gero"
  end

  def caveats
    <<~EOS
      gero runs its daemon as a LaunchAgent of yours; any gero command starts it,
      and after `brew upgrade gero` the next one moves it onto the new version.

      Once, for the network fence that keeps isolations off the Mac and the LAN:
        sudo gero pf install
      The fence helper is a copy of gero: run that again after an upgrade.

      Before `brew uninstall gero`:
        gero daemon uninstall
        sudo gero pf uninstall
    EOS
  end

  test do
    assert_equal "gero #{version}", shell_output("#{bin}/gero version").strip
  end
end
