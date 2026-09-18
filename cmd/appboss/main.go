package main

import (
	"os"

	"app-boss/internal/cli"
)

func main() { os.Exit((cli.CLI{Out: os.Stdout, Err: os.Stderr}).Run(os.Args[1:])) }
