// reviewd-command-host is a static Linux container helper, not a model tool or
// a host shell entry point. Packages pair its exact bytes with the native CLI.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/faustbrian/reviewd/internal/commandcapture"
	"os"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "--restricted-child" && os.Args[2] == "--" {
		_ = commandcapture.ExecRestricted(os.Args[3:])
		os.Exit(125)
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("reviewd command host version " + version)
		return
	}
	options := flag.NewFlagSet("reviewd-command-host", flag.ContinueOnError)
	options.SetOutput(os.Stderr)
	protocol := options.Int("protocol", 0, "capture protocol")
	nonce := options.String("nonce", "", "application request identity")
	if options.Parse(os.Args[1:]) != nil || *protocol != commandcapture.Protocol {
		os.Exit(125)
	}
	key, err := hex.DecodeString(os.Getenv(commandcapture.KeyEnvironment))
	if err != nil || len(key) != 32 {
		os.Exit(125)
	}
	exit, err := commandcapture.Execute(commandcapture.GuestDirectory, key, commandcapture.Request{Nonce: *nonce, Argv: options.Args()})
	if err != nil {
		os.Exit(125)
	}
	os.Exit(exit)
}
