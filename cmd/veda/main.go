// Veda — the knowledge of you, owned by you, readable by every agent.
//
// One local memory file. Every MCP agent reads and writes it.
// Your machine, your key, your DB.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/teochenglim/veda/internal/config"
	"github.com/teochenglim/veda/internal/embed"
	"github.com/teochenglim/veda/internal/llm"
	"github.com/teochenglim/veda/internal/mcpserver"
	"github.com/teochenglim/veda/internal/store"
	"github.com/teochenglim/veda/internal/syncengine"
	"github.com/teochenglim/veda/internal/telemetry"
	"github.com/teochenglim/veda/internal/ui"
	"github.com/teochenglim/veda/internal/wal"
	"github.com/teochenglim/veda/internal/worker"
)

// version is stamped at build time: -ldflags "-X main.version=v0.1.0"
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(rest)
	case "serve":
		err = cmdServe(rest)
	case "ui":
		err = cmdUI(rest)
	case "worker":
		err = cmdWorker(rest)
	case "export":
		err = cmdExport(rest)
	case "import":
		err = cmdImport(rest)
	case "telemetry":
		err = cmdTelemetry(rest)
	case "sync":
		err = cmdSync(rest)
	case "version", "--version", "-v":
		fmt.Println("veda " + version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "veda:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`Veda — one local memory file, every MCP agent reads and writes it.

Usage:
  veda init                          Create ~/.veda/ (veda.db + config.toml)
  veda serve --stdio                 Run the MCP server over stdio (for agent configs)
  veda ui                            Open the review UI at http://127.0.0.1:7331
  veda worker                        Run the async pipeline once and exit
  veda export [-o file]              Export all memories as JSON
  veda import -f file                Import memories from a JSON export
  veda telemetry status|disable|preview|export|forget   Manage anonymous stats (default: off)
  veda sync status|push|pull         Cross-device sync (paid tier, default: off)
  veda version                       Print the version

Learn more: README.md
`)
}

func openStore() (*store.Store, error) {
	if _, err := os.Stat(config.DBPath()); os.IsNotExist(err) {
		return nil, fmt.Errorf("no veda.db in %s — run `veda init` first", config.Home())
	}
	return store.Open(config.DBPath())
}

// --- veda init --------------------------------------------------------------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var assumeNo = fs.Bool("no-telemetry", false, "skip the telemetry prompt and keep telemetry off")
	fs.Parse(args)

	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	created := false
	if _, err := os.Stat(config.DBPath()); os.IsNotExist(err) {
		created = true
	}
	s, err := store.Open(config.DBPath())
	if err != nil {
		return err
	}
	defer s.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Telemetry.InstallID == "" {
		cfg.Telemetry.InstallID = config.NewInstallID()
	}
	if _, err := os.Stat(config.ConfigPath()); os.IsNotExist(err) {
		if !*assumeNo && isInteractive() {
			askTelemetry(cfg)
		}
		fmt.Printf("Telemetry is %s. Change anytime with `veda telemetry status/disable`.\n",
			telemetryWord(cfg.Telemetry.Enabled))
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	if created {
		fmt.Printf("Initialized %s (veda.db, config.toml)\n", config.Home())
	} else {
		fmt.Printf("Veda already initialized at %s\n", config.Home())
	}
	fmt.Println("\nNext: add Veda to your MCP client:")
	fmt.Println(`  {"mcpServers": {"veda": {"command": "veda", "args": ["serve", "--stdio"]}}}`)
	return nil
}

func telemetryWord(on bool) string {
	if on {
		return "ON"
	}
	return "off (default)"
}

func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func askTelemetry(cfg *config.Config) {
	fmt.Print(`Veda collects anonymous usage stats to improve recall quality.
What we send: counts, recall hit rate, error codes.
What we NEVER send: your memories, conversations, API keys, identity.
Share anonymous stats? [y/N]: `)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	cfg.Telemetry.Enabled = line == "y" || line == "yes"
}

// --- veda serve --stdio -----------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stdio := fs.Bool("stdio", true, "serve MCP over stdio")
	fs.Parse(args)
	if !*stdio {
		return fmt.Errorf("only --stdio transport is supported in v0.1")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()

	// The pipeline runs alongside the MCP server so captured turns are
	// drained and summarized without blocking tool responses.
	cfg, _ := config.Load()
	w, err := wal.Open(config.WALPath(), 1024)
	if err != nil {
		return err
	}
	defer w.Close()
	var lc *llm.Client
	if key := cfg.APIKey(); key != "" {
		lc = llm.New(cfg.LLM.BaseURL, key, cfg.LLM.Model)
	}
	var em store.Embedder
	if cfg.Embed.Enabled && cfg.Embed.BaseURL != "" {
		em = embed.New(cfg.Embed.BaseURL, cfg.EmbedAPIKey(), cfg.Embed.Model)
	}
	collector := telemetry.NewCollector()
	wk := &worker.Worker{Store: s, WAL: w, LLM: lc, Embedder: em,
		Interval: time.Duration(cfg.Worker.IntervalMinutes) * time.Minute, Batch: cfg.Worker.BatchSize,
		OnGateRejected: collector.AddGateRejected, OnError: collector.RecordError}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wk.Run(ctx)
	if cfg.Telemetry.Enabled && cfg.Telemetry.URL != "" {
		telemetry.StartFlusher(ctx, s, cfg.Telemetry.URL,
			time.Duration(cfg.Telemetry.FlushHours)*time.Hour, cfg.Telemetry.InstallID, version,
			func() *telemetry.Ext {
				return collector.Snapshot(config.DBPath(), em != nil, cfg.Sync.Enabled, telemetry.ProviderFromURL(cfg.Embed.BaseURL))
			})
	}
	if cfg.Sync.Enabled && cfg.Sync.URL != "" {
		syncEngine(s, cfg).RunBackgroundLoop(ctx)
	}
	err = mcpserver.Run(s, em, collector.AddClient)
	if cfg.Telemetry.Enabled {
		// persist the session's ext snapshot so `telemetry preview/export`
		// (other processes) can show exactly what this session collected
		ext := collector.Snapshot(config.DBPath(), em != nil, cfg.Sync.Enabled, telemetry.ProviderFromURL(cfg.Embed.BaseURL))
		_ = telemetry.PersistExt(s, ext)
	}
	return err
}

func syncEngine(s *store.Store, cfg *config.Config) *syncengine.Engine {
	return &syncengine.Engine{Store: s, Cfg: cfg.Sync}
}

// --- veda sync ----------------------------------------------------------------

func cmdSync(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: veda sync <status|push|pull>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	eng := syncEngine(s, cfg)
	switch args[0] {
	case "status":
		st, err := eng.Status()
		if err != nil {
			return err
		}
		fmt.Printf("sync:      %s\n", onOff(st.Enabled))
		fmt.Printf("endpoint:  %s\n", st.Endpoint)
		fmt.Printf("device_id: %s\n", st.DeviceID)
		fmt.Printf("token:     %s\n", configured(st.TokenConfigured))
		fmt.Printf("passphrase: %s\n", configured(st.PassphraseConfigured))
		fmt.Printf("last push seq: %d\n", st.LastPushSeq)
		fmt.Printf("pending:   %d memories, %d tombstones\n", st.PendingMemories, st.PendingTombstones)
		if !st.Enabled {
			fmt.Println("\nSync is the paid tier — set [sync] enabled/url in config.toml, then")
			fmt.Printf("export %s and %s. Local features stay free forever.\n",
				cfg.Sync.TokenEnv, cfg.Sync.PassphraseEnv)
		}
		return nil
	case "push":
		out, err := eng.Push(context.Background())
		fmt.Println(out)
		return err
	case "pull":
		out, err := eng.Pull(context.Background())
		fmt.Println(out)
		return err
	default:
		return fmt.Errorf("unknown sync command %q", args[0])
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off (default)"
}

func configured(yes bool) string {
	if yes {
		return "configured"
	}
	return "not set"
}

// --- veda ui ----------------------------------------------------------------

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	bind := fs.String("addr", "", "bind address (default 127.0.0.1:7331)")
	fs.Parse(args)
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _ := config.Load()
	addr := *bind
	if addr == "" {
		addr = cfg.UI.Bind
	}
	collector := telemetry.NewCollector()
	sv := &ui.Server{Store: s, OnOpen: collector.AddUIOpen}
	fmt.Printf("Veda review UI: http://%s  (ctrl-c to stop)\n", addr)
	srv := &http.Server{Addr: addr, Handler: sv.Handler()}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		srv.Close()
	}()
	return srv.ListenAndServe()
}

// --- veda worker --------------------------------------------------------------

// worker runs one pipeline pass (drain WAL → gate → summarize) and exits;
// handy for cron users who don't keep `serve` running between sessions.
func cmdWorker(args []string) error {
	flag.NewFlagSet("worker", flag.ExitOnError).Parse(args)
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _ := config.Load()
	w, err := wal.Open(config.WALPath(), 16)
	if err != nil {
		return err
	}
	defer w.Close()
	var lc *llm.Client
	if key := cfg.APIKey(); key != "" {
		lc = llm.New(cfg.LLM.BaseURL, key, cfg.LLM.Model)
	}
	var em store.Embedder
	if cfg.Embed.Enabled && cfg.Embed.BaseURL != "" {
		em = embed.New(cfg.Embed.BaseURL, cfg.EmbedAPIKey(), cfg.Embed.Model)
	}
	wk := &worker.Worker{Store: s, WAL: w, LLM: lc, Embedder: em, Batch: cfg.Worker.BatchSize}
	return wk.RunOnce(context.Background())
}

// --- veda export / import -----------------------------------------------------

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("o", "", "write to file instead of stdout")
	fs.Parse(args)
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := s.Export()
	if err != nil {
		return err
	}
	if *out == "" {
		os.Stdout.Write(data)
		fmt.Println()
		return nil
	}
	return os.WriteFile(*out, data, 0o600)
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	file := fs.String("f", "", "JSON export file to import (required)")
	fs.Parse(args)
	if *file == "" {
		return fmt.Errorf("usage: veda import -f <file>")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	n, err := s.Import(data)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %d memories (existing ids skipped)\n", n)
	return nil
}

// --- veda telemetry -------------------------------------------------------------

func cmdTelemetry(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: veda telemetry <status|disable|enable|preview|export|forget>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		fmt.Printf("telemetry: %s\n", telemetryWord(cfg.Telemetry.Enabled))
		fmt.Printf("endpoint:  %s\n", cfg.Telemetry.URL)
		fmt.Printf("install_id: %s\n", cfg.Telemetry.InstallID)
	case "enable":
		cfg.Telemetry.Enabled = true
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Println("Telemetry enabled. `veda telemetry preview` shows exactly what would be sent.")
	case "disable":
		cfg.Telemetry.Enabled = false
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Println("Telemetry disabled. Nothing further will be queued or sent.")
	case "preview":
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		payload, err := telemetry.Preview(s, cfg.Telemetry.InstallID, version, nil)
		if err != nil {
			return err
		}
		fmt.Println(payload)
	case "export":
		// The erasure handle: the install id plus everything queued locally.
		fmt.Printf("install_id: %s\n", cfg.Telemetry.InstallID)
		s, err := openStore()
		if err == nil {
			defer s.Close()
			if queued, err := s.ListTelemetry(); err == nil {
				fmt.Printf("queued payloads: %d\n", len(queued))
				for _, p := range queued {
					var pretty any
					if json.Unmarshal([]byte(p), &pretty) == nil {
						b, _ := json.MarshalIndent(pretty, "  ", "  ")
						fmt.Println("  " + string(b))
					}
				}
			}
		}
		fmt.Println("Remote erasure: DELETE <telemetry-url>/?install_id=<id> (`veda telemetry forget`)")
	case "forget":
		if cfg.Telemetry.URL == "" {
			return fmt.Errorf("no telemetry endpoint configured")
		}
		return telemetry.Delete(context.Background(), cfg.Telemetry.URL, cfg.Telemetry.InstallID)
	default:
		return fmt.Errorf("unknown telemetry command %q", args[0])
	}
	return nil
}
