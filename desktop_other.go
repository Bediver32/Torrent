//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func startDesktop(address string, noBrowser bool, _ chan<- struct{}) (func(), error) {
	if !noBrowser {
		if err := open(address, true); err != nil {
			fmt.Fprintln(os.Stderr, "Откройте адрес в браузере:", address)
		}
	}
	return func() {}, nil
}

func showDesktopError(error) {}

func open(target string, _ bool) error {
	program := "xdg-open"
	if runtime.GOOS == "darwin" {
		program = "open"
	}
	cmd := exec.Command(program, target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
