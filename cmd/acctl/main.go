// Command acctl is the command-line client for the agent-comp API.
package main

import (
	"fmt"
	"os"

	"github.com/mondeep31/agent-comp/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: acctl version")
	os.Exit(2)
}
