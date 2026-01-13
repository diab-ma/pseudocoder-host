class Pseudocoder < Formula
  desc "Supervise AI coding sessions from your phone"
  homepage "https://github.com/diab-ma/pseudocoder-host"
  version "0.1.0-alpha"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-arm64"
      sha256 "1be47a002037064ff9fcda5a28599db5797be7cf691919476af409dbb15560db"
    end
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-amd64"
      sha256 "009b0b4e20cf7faf9e6b0405ac2b00fbf5f4022cb2f49ca19e5b0ba54c5358e3"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-linux-amd64"
      sha256 "f988dc09e090ed1753d10520cbb578972878bdb824f4f721be5ea22a262a5437"
    end
  end

  def install
    bin.install Dir["*"].first => "pseudocoder"
  end

  test do
    assert_match "pseudocoder", shell_output("#{bin}/pseudocoder --version")
  end
end
