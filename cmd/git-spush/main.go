package main

import (
	"context"
	"os"

	"github.com/ryotarai/git-spush/internal/spush"
)

func main() {
	os.Exit(spush.Main(context.Background(), os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}
