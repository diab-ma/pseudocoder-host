class Pseudocoder < Formula
  desc "Supervise AI coding sessions from your phone"
  homepage "https://github.com/diab-ma/pseudocoder-host"
  version "0.2.0-beta"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-arm64"
      sha256 "e40ac2714c33cae492831e1d436b52de9791ae4327b5960bd8f01993561624af"
    end
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-amd64"
      sha256 "0305c5bc26d1082b97a790487375710aaf3ce33d118fdc57fe21f2fc670ee256"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-linux-amd64"
      sha256 "3323c962d198688ae32067d0c01e743d8a45eab3aa91c6574b16220179a112b1"
    end
  end

  def install
    bin.install Dir["*"].first => "pseudocoder"
  end

  test do
    assert_match "pseudocoder", shell_output("#{bin}/pseudocoder --version")
  end
end
