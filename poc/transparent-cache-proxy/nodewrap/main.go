// nodewrap stands in for the runner's bundled node binary and execs the real
// one with NODE_EXTRA_CA_CERTS pointing at the cache proxy's CA. It is a static
// binary rather than a shell script because POSIX sh drops environment
// variables whose names are not identifiers, and the runner passes action
// inputs as INPUT_<NAME> with the input's dashes intact.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodewrap:", err)
		os.Exit(127)
	}
	dir := filepath.Dir(self)
	env := os.Environ()
	if os.Getenv("NODE_EXTRA_CA_CERTS") == "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+filepath.Join(dir, "..", "..", "firerunner-ca.pem"))
	}
	real := filepath.Join(dir, "node.real")
	err = syscall.Exec(real, append([]string{real}, os.Args[1:]...), env)
	fmt.Fprintln(os.Stderr, "nodewrap: exec", real+":", err)
	os.Exit(127)
}
