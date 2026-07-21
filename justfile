export GOCACHE := env_var_or_default('GOCACHE', env_var_or_default('TMPDIR', '/tmp') + '/restop-go-cache')

# Rebuild and restart the server as source files change during development.
run:
    air --build.cmd "go build -o /tmp/restop-air ./cmd/restop" --build.bin "/tmp/restop-air"

test:
    go fmt ./...
    git ls-files -z -- '*.go' | xargs -0 gopls check -severity=hint
    go vet ./...
    go test -race ./...
