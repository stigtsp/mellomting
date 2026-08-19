package main

import (
	"fmt"
	"io"
	"os"

	"mellomting/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Fprintln(os.Stdout, version.String())
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "mellomting: command not implemented yet: %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: mellomting <command>

commands:
  version    print version information
  help       print this help

commands being implemented per docs/PLAN.md:
  serve                              run the proxy daemon
  config check                       validate configuration
  config show-effective              show effective configuration
  key create|list|disable|enable|revoke
                                     offline API key management
  usage                              report token/request usage
  sandbox check                      report Landlock capability
`)
}
