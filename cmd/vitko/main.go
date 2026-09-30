// Command vitko is the command-line tool for Vitko.
package main

import (
	"os"

	"github.com/vitko-inc/vitko/internal/commands"
)

func main() {
	os.Exit(commands.New(commands.DefaultEnv()).Run(os.Args[1:]))
}
