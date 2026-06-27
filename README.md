# akari

A Go application.

## Requirements

- Go 1.26 or newer

## Usage

```sh
go run . [name]
```

Without an argument it greets the world; pass a name to greet someone specific:

```sh
$ go run . Ada
Hello, Ada!
```

## Development

```sh
go build ./...   # compile everything
go test ./...    # run the tests
go vet ./...     # static checks
```

## Layout

- `main.go` is the command entry point.
- `internal/greet` holds the greeting logic, kept in an `internal` package so it
  is importable only from within this module.
