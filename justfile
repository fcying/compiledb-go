set unstable
set shell := ["nu", "-c"]
set script-interpreter := ["nu"]

[private]
default: build

[script]
build:
    go run ./cmd/compiledb/main.go -v --full-path -p ./tests/build.log

[script]
test:
    go test -count=1 ./...

[script]
release:
    go install ./cmd/compiledb
    GOOS=windows go install ./cmd/compiledb
    mv ~/go/bin/windows_amd64/compiledb.exe ~/workspace/go/bin
