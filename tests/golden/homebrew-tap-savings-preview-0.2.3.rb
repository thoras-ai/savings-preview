class SavingsPreview < Formula
  desc "Read a cluster's usage history and show what Thoras would have saved"
  homepage "https://github.com/thoras-ai/savings-preview"
  version "0.2.3"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/thoras-ai/savings-preview/releases/download/v0.2.3/savings-preview_v0.2.3_darwin_arm64.tar.gz"
      sha256 "908e2eb822d45ebb422835d2c2f3fc793c325896185ec78049a941a2cd55667a"
    end
    on_intel do
      url "https://github.com/thoras-ai/savings-preview/releases/download/v0.2.3/savings-preview_v0.2.3_darwin_amd64.tar.gz"
      sha256 "e6a2d1187f9baed4aa6444a2d07fc3b91389b0a8e8352bc6f77efd325dfc9aaa"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/thoras-ai/savings-preview/releases/download/v0.2.3/savings-preview_v0.2.3_linux_arm64.tar.gz"
      sha256 "d811df141bcbca233675a738474ad576d422caf7797978ae704e61158ff846a5"
    end
    on_intel do
      url "https://github.com/thoras-ai/savings-preview/releases/download/v0.2.3/savings-preview_v0.2.3_linux_amd64.tar.gz"
      sha256 "fbc6986a46e566c7dbda39c8163d086a820e233b939e13cbd3d23e433acc11b6"
    end
  end

  def install
    bin.install "savings-preview"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/savings-preview --version")
  end
end
