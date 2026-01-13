# Contributing

## Building from Source

```bash
cd host
go build -o pseudocoder ./cmd
./pseudocoder --version
```

Requirements:
- Go 1.24+

## Running Tests

```bash
# Unit tests
cd host && go test ./...

# Integration tests
cd host && go test -tags=integration ./...
```

## Code Style

- Standard `gofmt` formatting
- Run `go vet ./...` before committing

## Releasing

The project uses Homebrew for macOS distribution. When creating releases, these conventions must be maintained to avoid breaking the brew formula.

### Release Workflow

1. Push a tag starting with `v` (e.g., `v0.1.0`, `v0.2.0-beta`)
2. GitHub Actions builds binaries and creates a release
3. Update `Formula/pseudocoder.rb` with new version and SHA256 hashes

### Brew Formula Requirements

**Binary naming** - Release workflow must produce these exact names:
- `pseudocoder-darwin-arm64` (macOS Apple Silicon)
- `pseudocoder-darwin-amd64` (macOS Intel)
- `pseudocoder-linux-amd64` (Linux/WSL)

**Download URL pattern**:
```
https://github.com/diab-ma/pseudocoder-host/releases/download/v{version}/pseudocoder-{os}-{arch}
```

**Version format**:
- Git tag: `v0.1.0-alpha` (with `v` prefix)
- Formula version: `0.1.0-alpha` (without `v` prefix)

### After Creating a Release

Update `Formula/pseudocoder.rb`:

1. Update `version` field (no `v` prefix)
2. Update SHA256 hashes for each platform from `checksums.txt` in the release
3. Test locally: `brew install --build-from-source ./Formula/pseudocoder.rb`
4. Commit and push formula changes

### What NOT to Change

- Binary output names in `.github/workflows/release.yml`
- Download URL structure in `Formula/pseudocoder.rb`
- The `--version` flag (formula tests depend on it)
