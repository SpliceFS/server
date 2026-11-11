// Command splicefsd is the SpliceFS server.
package main

import (
	"os"

	"github.com/BlueRival/splicefs/internal/cli"
)

// exit is replaced in tests.
var exit = os.Exit

func main() {
	exit(cli.Run("splicefsd", os.Args[1:], os.Stdout, os.Stderr))
}
