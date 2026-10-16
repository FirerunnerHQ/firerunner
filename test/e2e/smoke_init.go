//go:build ignore

// Static PID1 for real-KVM boot and cache isolation tests. With no fr_cache_mode
// it only proves boot/self-destruct. Cache modes exercise HTTP from the real
// guest address; identity is supplied by the host fixture, never by this guest.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

func main() {
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	fmt.Println("firerunner-smoke: PID1 up")
	args, _ := os.ReadFile("/proc/cmdline")
	params := map[string]string{}
	for _, arg := range strings.Fields(string(args)) {
		if k, v, ok := strings.Cut(arg, "="); ok {
			params[k] = v
		}
	}
	mode := params["fr_cache_mode"]
	if mode != "" {
		base := "http://" + params["fr_cache_gateway"] + ":" + params["fr_cache_port"]
		if err := exercise(base, mode); err != nil {
			fmt.Printf("firerunner-smoke: FAIL %s: %v\n", mode, err)
		} else {
			fmt.Printf("firerunner-smoke: PASS %s\n", mode)
		}
	} else {
		fmt.Println("firerunner-smoke: PASS boot")
	}
	time.Sleep(500 * time.Millisecond)
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	for {
		time.Sleep(time.Second)
	}
}

var client = &http.Client{Timeout: 5 * time.Second}

const rpcPath = "/twirp/github.actions.results.api.v1.CacheService/"

func rpc(base, method string, body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r, err := client.Post(base+rpcPath+method, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("%s HTTP %d", method, r.StatusCode)
	}
	var out map[string]any
	err = json.NewDecoder(r.Body).Decode(&out)
	return out, err
}
func create(base, key string) (map[string]any, error) {
	// Deliberately forged guest scope metadata must not select the write ref.
	return rpc(base, "CreateCacheEntry", map[string]any{"key": key, "version": "e2e-v1", "metadata": map[string]any{"repository_id": "forged/repo", "ref": "refs/heads/main"}})
}
func save(base, key, data string) error {
	c, err := create(base, key)
	if err != nil {
		return err
	}
	if c["ok"] != true {
		return fmt.Errorf("reserve %s: %v", key, c)
	}
	u, _ := c["signed_upload_url"].(string)
	req, err := http.NewRequest("PUT", u, strings.NewReader(data))
	if err != nil {
		return err
	}
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	r.Body.Close()
	if r.StatusCode != 201 {
		return fmt.Errorf("upload HTTP %d", r.StatusCode)
	}
	f, err := rpc(base, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": "e2e-v1", "size_bytes": fmt.Sprint(len(data))})
	if err != nil {
		return err
	}
	if f["ok"] != true {
		return fmt.Errorf("finalize: %v", f)
	}
	return nil
}
func restore(base, key, want string, keys ...string) error {
	g, err := rpc(base, "GetCacheEntryDownloadURL", map[string]any{"key": key, "version": "e2e-v1", "restore_keys": keys})
	if err != nil {
		return err
	}
	if want == "" {
		if g["ok"] == true {
			return fmt.Errorf("unexpected hit for %s", key)
		}
		return nil
	}
	if g["ok"] != true {
		return fmt.Errorf("unexpected miss for %s: %v", key, g)
	}
	u, _ := g["signed_download_url"].(string)
	r, err := client.Get(u)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if r.StatusCode != 200 || string(b) != want {
		return fmt.Errorf("restore %s: HTTP %d %q, want %q", key, r.StatusCode, b, want)
	}
	return nil
}
func exercise(base, mode string) error {
	switch mode {
	case "main":
		if err := save(base, "shared", "MAIN"); err != nil {
			return err
		}
		return save(base, "default-only", "DEFAULT")
	case "base":
		if err := restore(base, "shared", "MAIN"); err != nil {
			return err
		}
		return save(base, "shared", "BASE")
	case "pr":
		if err := restore(base, "shared", "BASE"); err != nil {
			return err
		}
		if err := restore(base, "default-only", "DEFAULT"); err != nil {
			return err
		}
		if err := save(base, "shared", "PR"); err != nil {
			return err
		}
		if err := save(base, "pr-private", "PRIVATE"); err != nil {
			return err
		}
		return restore(base, "shared", "PR")
	case "sibling":
		if err := restore(base, "shared", "BASE"); err != nil {
			return err
		}
		if err := restore(base, "pr-private", ""); err != nil {
			return err
		}
		// Empty prefix searches must also stay inside the readable refs.
		return restore(base, "missing", "BASE", "")
	case "rerun":
		return restore(base, "shared", "PR")
	case "main-after":
		if err := restore(base, "pr-private", ""); err != nil {
			return err
		}
		return restore(base, "shared", "MAIN")
	case "repo-b":
		if err := restore(base, "shared", ""); err != nil {
			return err
		}
		if err := save(base, "shared", "REPO B"); err != nil {
			return err
		}
		return restore(base, "shared", "REPO B")
	case "repo-a-again":
		return restore(base, "shared", "MAIN")
	case "read-only":
		c, err := create(base, "read-only-write")
		if err != nil {
			return err
		}
		if c["ok"] == true {
			return fmt.Errorf("read-only identity wrote cache")
		}
		return restore(base, "shared", "MAIN")
	case "unknown":
		c, err := create(base, "unknown-write")
		if err != nil {
			return err
		}
		if c["ok"] == true {
			return fmt.Errorf("unknown VM can write")
		}
		if err := restore(base, "pr-private", ""); err != nil {
			return err
		}
		return restore(base, "shared", "")
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}
