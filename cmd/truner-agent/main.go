// Command truner-agent runs on every runner instance. It is downloaded by
// the instance's user data and receives its configuration from it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/codician-team/truner/internal/agent"
	"github.com/codician-team/truner/internal/cacheproxy"
	"github.com/codician-team/truner/internal/cachestore"
	"github.com/codician-team/truner/internal/version"
)

func logPath() string {
	if runtime.GOOS == "windows" {
		return `C:\truner\agent.log`
	}
	return "/var/log/truner/agent.log"
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version.Version)
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfgPath := fs.String("config", "", "path to the agent config (base64 or JSON)")
		noShutdown := fs.Bool("no-shutdown", false, "do not power off at the end (testing)")
		fs.Parse(os.Args[2:])
		if *cfgPath == "" {
			fmt.Fprintln(os.Stderr, "--config is required")
			os.Exit(2)
		}
		cfg, err := agent.DecodeConfigFile(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "truner-agent:", err)
			os.Exit(1)
		}
		a, _ := agent.New(cfg, logPath())
		a.NoShutdown = *noShutdown
		os.Exit(a.Main(ctx))
	case "boot":
		os.Exit(agent.Boot(ctx, logPath()))
	case "patch-runner":
		// truner-agent patch-runner <runner dir>: apply the cache patch (debugging).
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: truner-agent patch-runner <runner dir>")
			os.Exit(2)
		}
		if err := agent.PatchWorker(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "truner-agent:", err)
			os.Exit(1)
		}
		fmt.Println("patched")
	case "cache-proxy":
		// A standalone magic cache backed by a local directory, for testing
		// the cache protocol without AWS.
		fs := flag.NewFlagSet("cache-proxy", flag.ExitOnError)
		dir := fs.String("store-dir", filepath.Join(os.TempDir(), "truner-cache"), "directory to store cache entries in")
		listen := fs.String("listen", "127.0.0.1:28777", "listen address")
		prefix := fs.String("prefix", "cache/local/test/", "key prefix")
		fs.Parse(os.Args[2:])
		srv, err := cacheproxy.New(cacheproxy.Options{Store: cachestore.Dir{Root: *dir}, Prefix: *prefix, PublicURL: "http://" + *listen, Logger: slog.Default()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("magic cache listening on http://%s (store: %s)\n", *listen, *dir)
		if err := http.ListenAndServe(*listen, srv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `truner-agent: the truner runner-instance agent

Usage:
  truner-agent run --config FILE    run the lifecycle described by FILE
  truner-agent boot                 run the job in this instance's user data (warm pools)
  truner-agent cache-proxy          serve the magic cache from a local directory (testing)
  truner-agent patch-runner DIR     apply the cache patch to a runner installation
  truner-agent version`)
}
