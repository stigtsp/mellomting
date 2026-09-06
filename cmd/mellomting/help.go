package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
)

const configFlagHelp = "configuration `PATH` (default /etc/mellomting/config.yaml)"

// commandFlags keeps command help consistent with the documented long flags.
func commandFlags(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet("mellomting "+name, flag.ContinueOnError)
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "Usage: %s [flags]\n\n%s\n\nFlags:\n", fs.Name(), summary)
		fs.VisitAll(func(f *flag.Flag) {
			value, description := flag.UnquoteUsage(f)
			label := "--" + f.Name
			if value != "" {
				label += " " + value
			}
			fmt.Fprintf(w, "  %-28s %s", label, description)
			if f.DefValue != "" && f.DefValue != "0" && f.DefValue != "false" {
				fmt.Fprintf(w, " (default %s)", f.DefValue)
			}
			fmt.Fprintln(w)
		})
	}
	return fs
}

// Help is successful stdout output; malformed arguments remain stderr errors.
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := fs.Parse(args)
	var w io.Writer = os.Stderr
	if err == flag.ErrHelp {
		w = os.Stdout
	}
	fmt.Fprint(w, out.String())
	return err
}

func flagExitCode(err error) int {
	if err == flag.ErrHelp {
		return 0
	}
	return 2
}

func groupHelp(args []string, name, commands string) bool {
	if len(args) != 1 || (args[0] != "--help" && args[0] != "-h") {
		return false
	}
	fmt.Fprintf(os.Stdout, "Usage: mellomting %s <command> [flags]\n\n%s\n\nRun 'mellomting %s <command> --help' for details.\n",
		name, commands, name)
	return true
}
