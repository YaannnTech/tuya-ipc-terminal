package main

import (
	"fmt"
	"os"

	"tuya-ipc-terminal/cmd"
	"tuya-ipc-terminal/cmd/auth"
	"tuya-ipc-terminal/pkg/core"
)

// VERSION is a var so release builds can override it with -ldflags "-X main.VERSION=...".
var VERSION = "0.3.1"

func main() {
	core.InitLogger()

	if err := cmd.Execute(VERSION); err != nil {
		fmt.Println("Command execution failed")
		os.Exit(auth.ExitCode(err))
	}
}
