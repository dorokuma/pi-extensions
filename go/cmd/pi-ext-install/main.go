package main

import (
	"os"
)

func main() {
	a := &app{
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
	}
	os.Exit(a.run(os.Args[1:]))
}
