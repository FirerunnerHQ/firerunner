package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// daemonEnvFile is where the systemd units in deploy/ and the control VM's
// bootstrap keep the daemon's FR_* settings (EnvironmentFile=). Tests point it
// elsewhere.
var daemonEnvFile = "/etc/firerunner/firerunner.env"

// diagEnv decides which environment file status and doctor read, and loads
// it. The daemon gets its settings from a systemd EnvironmentFile, so a shell
// on the same host has none of them, and status and doctor would describe
// some other, default deployment: on a control VM, a Firecracker host
// with no KVM. So:
//
//   - --env-file FILE reads FILE; --env-file none reads nothing;
//   - otherwise, when the environment sets no FR_* variable and daemonEnvFile
//     exists, it is read, and stderr says so.
//
// Variables already set win over the file's. It returns args without the
// --env-file option.
func diagEnv(cmd string, args []string, stderr io.Writer) ([]string, error) {
	file, explicit := "", false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--env-file" || a == "-env-file":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a file, or none", a)
			}
			file, explicit = args[i+1], true
			i++
		case strings.HasPrefix(a, "--env-file="), strings.HasPrefix(a, "-env-file="):
			file, explicit = a[strings.IndexByte(a, '=')+1:], true
		default:
			rest = append(rest, a)
		}
	}
	if explicit {
		if file == "none" || file == "" {
			return rest, nil
		}
		n, err := loadEnvFile(file)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "%s: read %d settings from %s\n", cmd, n, file)
		return rest, nil
	}
	if anyFREnv() {
		return rest, nil
	}
	n, err := loadEnvFile(daemonEnvFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rest, nil
	case err != nil:
		// Usually: not root. Say so, and report on the defaults.
		fmt.Fprintf(stderr, "%s: cannot read the daemon's settings (%v); rerun as root, or pass --env-file\n", cmd, err)
		return rest, nil
	}
	fmt.Fprintf(stderr, "%s: read %d settings from %s (the daemon's); --env-file none to skip\n", cmd, n, daemonEnvFile)
	return rest, nil
}

// anyFREnv reports whether the environment sets any FR_* variable.
func anyFREnv() bool {
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FR_") {
			return true
		}
	}
	return false
}

// loadEnvFile sets each KEY=VALUE line of a systemd EnvironmentFile that the
// environment does not already set, and returns how many it set. Blank lines
// and lines starting with # or ; are skipped; a value in matching single or
// double quotes loses them.
func loadEnvFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		key, val, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return n, fmt.Errorf("%s:%d: not KEY=VALUE", path, line)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}
