# Homebrew formula for savings-preview. scripts/homebrew_formula.py fills in the
# version, download URLs and checksums for each release; docs/releasing.md
# describes how to publish the result to the tap.
class SavingsPreview < Formula
  desc "Read a cluster's usage history and show what Thoras would have saved"
  homepage "https://github.com/thoras-ai/savings-preview"
  version "@VERSION@"

  # The repository is private, so the archives are fetched through the GitHub
  # API with a token that can read it.
  HEADERS = ["Accept: application/octet-stream", "X-GitHub-Api-Version: 2022-11-28"].tap do |h|
    token = ENV.fetch("HOMEBREW_GITHUB_API_TOKEN", "")
    h << "Authorization: Bearer #{token}" unless token.empty?
  end.freeze

  on_macos do
    on_arm do
      url "@URL_DARWIN_ARM64@", headers: HEADERS
      sha256 "@SHA256_DARWIN_ARM64@"
    end
    on_intel do
      url "@URL_DARWIN_AMD64@", headers: HEADERS
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    on_arm do
      url "@URL_LINUX_ARM64@", headers: HEADERS
      sha256 "@SHA256_LINUX_ARM64@"
    end
    on_intel do
      url "@URL_LINUX_AMD64@", headers: HEADERS
      sha256 "@SHA256_LINUX_AMD64@"
    end
  end

  def install
    bin.install "savings-preview"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/savings-preview --version")
  end
end
