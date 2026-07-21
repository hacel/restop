export GOCACHE := env_var_or_default('GOCACHE', env_var_or_default('TMPDIR', '/tmp') + '/restop-go-cache')

# Rebuild and restart the server as source files change during development.
run:
    air --tmp_dir "/tmp" --log.main_only true --build.cmd "go build -o /tmp/restop-air ./cmd/restop" --build.entrypoint "/tmp/restop-air"

test:
    go fmt ./...
    git ls-files -z -- '*.go' | xargs -0 goimports -w
    git ls-files -z -- '*.go' | xargs -0 gopls check -severity=hint
    go vet ./...
    go test -race ./...
