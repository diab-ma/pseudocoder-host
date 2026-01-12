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
