# Homebrew formula for Zero.
#
# NOT PUBLISHED. This is a draft for review. Nothing installs from it until it
# is copied into a tap (Gitlawb/homebrew-tap, Formula/zero.rb) and the release
# workflow is taught to bump the version and checksums on each tag. Both of
# those are release decisions, so they are deliberately not made here.
#
# Verify locally without a tap:
#
#   brew install --build-from-source ./packaging/homebrew/zero.rb
#   brew audit --strict --formula ./packaging/homebrew/zero.rb
#
# The checksums below are the real ones published with v0.7.0, read from the
# .sha256 files that ship beside each archive, so this draft is installable as
# written rather than being a skeleton with placeholders.
class Zero < Formula
  desc "Terminal coding agent"
  homepage "https://github.com/Gitlawb/zero"
  version "0.7.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/Gitlawb/zero/releases/download/v0.7.0/zero-v0.7.0-macos-arm64.tar.gz"
      sha256 "75e859fe25f3f63785f512f20b9c9501c67394166f10746f80e374672a1a8b7f"
    end
    on_intel do
      url "https://github.com/Gitlawb/zero/releases/download/v0.7.0/zero-v0.7.0-macos-x64.tar.gz"
      sha256 "184256abd5738b77d44cf4a99d71ac32d0a0355714ebc698a2221a69aeb71976"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/Gitlawb/zero/releases/download/v0.7.0/zero-v0.7.0-linux-arm64.tar.gz"
      sha256 "dd0355f78b6ab044e1181184e29432d0ab7652a1dc27a161960f06e8520b4f21"
    end
    on_intel do
      url "https://github.com/Gitlawb/zero/releases/download/v0.7.0/zero-v0.7.0-linux-x64.tar.gz"
      sha256 "f5120c2cc1e9f45ebf69d472b6026eb8e37eee2c113211efedd7d0917437490c"
    end
  end

  def install
    bin.install "zero"
    # The Linux archive carries the sandbox helper beside the binary. Without it
    # on PATH, native sandboxing is silently unavailable rather than broken, so
    # install it whenever the archive provides one.
    bin.install "zero-linux-sandbox" if File.exist?("zero-linux-sandbox")
  end

  def caveats
    <<~EOS
      Update with `brew upgrade zero`.

      `zero upgrade` refuses on a Homebrew install on purpose: replacing the keg
      binary would leave Homebrew describing a version that is no longer on disk,
      and the next `brew upgrade` would revert it.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/zero --version")
  end
end
