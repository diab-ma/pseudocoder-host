class Pseudocoder < Formula
  desc "Supervise AI coding sessions from your phone"
  homepage "https://github.com/diab-ma/pseudocoder-host"
  version "2.2.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-arm64"
      sha256 "84149423ade2b3b867b0005d8de6549292a23871105f96960d0b0483ac61fb12"
    end
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-amd64"
      sha256 "5481c1d37dbdc959353fa37f8901c2404a7b1000344578026e4efd8bbfe729b9"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-linux-amd64"
      sha256 "1b985726e93abf7c7edff790c4fe0a92fe83ba238b129c6e64c6ee5474b3606a"
    end
  end

  def install
    bin.install Dir["*"].first => "pseudocoder"
  end

  test do
    assert_match "pseudocoder", shell_output("#{bin}/pseudocoder --version")
  end
end
