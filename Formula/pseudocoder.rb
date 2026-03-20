class Pseudocoder < Formula
  desc "Supervise AI coding sessions from your phone"
  homepage "https://github.com/diab-ma/pseudocoder-host"
  version "2.2.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-arm64"
      sha256 "c2224752b16f97e0c7f131ff2926b8acd3d6bb1715b5fbfcba5cce2ad6ddf63e"
    end
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-darwin-amd64"
      sha256 "f37423b07314a736c562e18d1fc22d7232e1b1cbbd4b2606d24e18279e792e29"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/diab-ma/pseudocoder-host/releases/download/v#{version}/pseudocoder-linux-amd64"
      sha256 "7f47f86fdc8c99649ebf45bab1884cb6e3b3c9b73dba4d07fa2b7300ac347db0"
    end
  end

  def install
    bin.install Dir["*"].first => "pseudocoder"
  end

  test do
    assert_match "pseudocoder", shell_output("#{bin}/pseudocoder --version")
  end
end
