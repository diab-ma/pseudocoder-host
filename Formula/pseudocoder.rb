class Pseudocoder < Formula
  desc "Supervise AI coding sessions from your phone"
  homepage "https://github.com/diab-ma/pseudocoder-host"
  version "0.1.0-beta"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-arm64"
      sha256 "db99ca19526807b5229d222216cc213337269b765b5ae8114963d6a154f09821"
    end
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-amd64"
      sha256 "922d0aae772fc5d385c29ea81d6ae4f7aa16a0db0ba109af2f66337ec70c01e8"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-linux-amd64"
      sha256 "5385830e42a4815651b34d97e7267c7cd2d45117ed5888c545e27ca541510898"
    end
  end

  def install
    bin.install Dir["*"].first => "pseudocoder"
  end

  test do
    assert_match "pseudocoder", shell_output("#{bin}/pseudocoder --version")
  end
end
