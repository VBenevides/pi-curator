package main

import (
	"os"

	"pi-curator/curator"
	"pi-curator/curator/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, curator.Version))
}
