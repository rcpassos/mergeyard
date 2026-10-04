// A phase process for integration tests: it reports signals and selectively
// exits, so tests observe the real tmux/process boundary.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	mode := os.Args[1]
	prefix := ""
	if mode == "child" {
		prefix = "child "
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if mode == "parent" {
		child := exec.Command(os.Args[0], "child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
	}
	fmt.Println(prefix + "ready")
	for sig := range signals {
		switch sig {
		case syscall.SIGINT:
			fmt.Println(prefix + "SIGINT")
			if mode == "interrupt" || mode == "parent" {
				os.Exit(0)
			}
		case syscall.SIGTERM:
			fmt.Println(prefix + "SIGTERM")
			if mode == "terminate" {
				os.Exit(0)
			}
		}
	}
}
