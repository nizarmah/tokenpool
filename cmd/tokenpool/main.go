// Command tokenpool is an LLM proxy that pools API keys for Anthropic,
// xAI Grok or any other compatible endpoint, and fails over between them
// when one hits its limits.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/pool"
	"github.com/nizarmah/tokenpool/internal/proxy"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			fmt.Println(keygen())
			return
		case "version":
			fmt.Println(buildVersion())
			return
		}
	}
	configPath := flag.String("config", envOr("TOKENPOOL_CONFIG", "tokenpool.yaml"), "config file")
	check := flag.Bool("check", false, "validate the config, print the pool and exit")
	jsonLogs := flag.Bool("json-logs", false, "log as JSON")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `usage:
  tokenpool [-config tokenpool.yaml] [-check] [-json-logs]   run the proxy
  tokenpool keygen                                          print a new client key
  tokenpool version

`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := run(*configPath, *check, *jsonLogs); err != nil {
		fmt.Fprintln(os.Stderr, "tokenpool:", err)
		os.Exit(1)
	}
}

func run(configPath string, check, jsonLogs bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	p, err := pool.New(cfg.Strategy, cfg.Upstreams, cfg.PoolFile)
	if err != nil {
		return err
	}
	exposed := exposedConfig(configPath)
	if check {
		if exposed != "" {
			fmt.Fprintln(os.Stderr, "warning:", exposed)
		}
		printPool(os.Stdout, p.List())
		return nil
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if jsonLogs {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)
	if exposed != "" {
		log.Warn(exposed)
	}
	if cfg.AllowAnonymous {
		log.Warn("allow_anonymous is on: anyone who can reach tokenpool can spend its tokens")
	}
	if len(p.List()) == 0 {
		log.Warn("the pool is empty: add upstreams to the config file or through the admin API")
	}

	proxy.Version = buildVersion()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy.New(cfg, p, log).Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: streamed completions can run for many minutes.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("tokenpool listening", "addr", cfg.Listen, "upstreams", len(p.List()),
		"strategy", cfg.Strategy, "admin_api", cfg.AdminKey != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; waiting for open requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func printPool(w io.Writer, ups []pool.Status) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tPRIORITY\tNAME\tFORMAT\tURL\tMODEL\tTOKEN\tSOURCE\tSTATE")
	for _, u := range ups {
		model := u.Model
		if len(u.Models) > 0 {
			model = fmt.Sprintf("%s (+%d mappings)", model, len(u.Models))
		}
		if model == "" {
			model = "(as requested)"
		}
		role := "primary"
		if u.Fallback {
			role = "fallback"
		}
		token := u.Token
		if u.TokenFile != "" {
			token = "file:" + u.TokenFile
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			role, u.Priority, u.Name, u.Format, u.URL, model, token, u.Source, u.State)
	}
	tw.Flush()
}

// exposedConfig warns when the config file, which can hold API keys
// inline, is readable by users other than its owner.
func exposedConfig(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%s is readable by other users (mode %04o) and may hold API keys: chmod 600 %s",
		path, info.Mode().Perm(), path)
}

// buildVersion is the -ldflags version, or the module version for
// `go install …@version` builds.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func keygen() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return "tp-" + base64.RawURLEncoding.EncodeToString(b)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
