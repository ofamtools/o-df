// Command o-df finds and clears the usual disk hogs, on Ochazuke v2. Each
// culprit (a cache, a store, a download folder) is a data row: the
// embedded defaults plus ~/.config/o-df/culprits.json. o-df --help lists
// the commands (cli.go); at a terminal, o-df opens the culprits page
// (page.go, view.go).
package main

import (
	"errors"
	"fmt"
	"os"

	"ofam.dev/oden"
)

func main() { os.Exit(exitCode(Main(os.Args[1:], DefaultEnv(), os.Stdin, os.Stdout, os.Stderr))) }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var oee *oden.ExitError
	if errors.As(err, &oee) {
		if oee.Err != nil {
			fmt.Fprintln(os.Stderr, "o-df:", oee.Err)
		}
		return oee.Code
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			fmt.Fprintln(os.Stderr, "o-df:", ee.Err)
		}
		return ee.Code
	}
	fmt.Fprintln(os.Stderr, "o-df:", err)
	return 1
}
