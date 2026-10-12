class SavingsPreview < Formula
  desc "Read a cluster's usage history and show what Thoras would have saved"
  homepage "https://github.com/thoras-ai/savings-preview"
  version "@VERSION@"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "@URL_DARWIN_ARM64@"
      sha256 "@SHA256_DARWIN_ARM64@"
    end
    on_intel do
      url "@URL_DARWIN_AMD64@"
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    on_arm do
      url "@URL_LINUX_ARM64@"
      sha256 "@SHA256_LINUX_ARM64@"
    end
    on_intel do
      url "@URL_LINUX_AMD64@"
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
