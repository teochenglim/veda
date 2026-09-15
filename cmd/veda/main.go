// Veda — the knowledge of you, owned by you, readable by every agent.
//
// One local memory file. Every MCP agent reads and writes it.
// Your machine, your key, your DB.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/teochenglim/veda/internal/audit"
	"github.com/teochenglim/veda/internal/config"
	"github.com/teochenglim/veda/internal/embed"
	"github.com/teochenglim/veda/internal/eval"
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
	case "eval":
		err = cmdEval(rest)
	case "audit":
		err = cmdAudit(rest)
	case "policy":
		err = cmdPolicy(rest)
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
  veda eval -f suite.json            Score recall scenarios against a throwaway store
  veda audit keygen|export|verify    Signed audit-log export (compliance)
  veda policy status|enforce         Org retention + redaction policies
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config.toml is invalid: %w — fix or delete it in %s", err, config.Home())
	}
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
	if n, err := applyPolicies(s, cfg.Policies); err != nil {
		return err
	} else if n > 0 {
		fmt.Printf("veda: retention policy retired %d memories\n", n)
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

// applyPolicies wires v0.7 org policies into a store: redaction patterns
// on write paths and a retention sweep. Returns the number of retention
// deletions (0 when no retention policy is set).
func applyPolicies(s *store.Store, cfg *config.PoliciesConfig) (int, error) {
	if cfg == nil {
		return 0, nil
	}
	if len(cfg.Redact) > 0 {
		res, err := store.CompileRedactors(cfg.Redact)
		if err != nil {
			return 0, err
		}
		s.SetRedactors(res)
	}
	if cfg.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays).Unix()
		ids, err := s.EnforceRetention(cutoff, "policy")
		if err != nil {
			return 0, err
		}
		return len(ids), nil
	}
	return 0, nil
}

// --- veda audit ----------------------------------------------------------------

func cmdAudit(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: veda audit <keygen|export|verify> [flags]")
	}
	keyPath := filepath.Join(config.Home(), "audit_signing_key")
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("o", keyPath, "private key output path")
		fs.Parse(args[1:])
		kp, err := audit.Generate()
		if err != nil {
			return err
		}
		if err := audit.SavePrivate(*out, kp.Private); err != nil {
			return err
		}
		fmt.Printf("private key: %s (0600 — keep it secret, back it up)\n", *out)
		fmt.Printf("public key:  %s\n", hex.EncodeToString(kp.Public))
		return nil
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		out := fs.String("o", "", "output file (default stdout)")
		key := fs.String("key", keyPath, "signing key path")
		fs.Parse(args[1:])
		priv, err := audit.LoadPrivate(*key)
		if err != nil {
			return fmt.Errorf("no signing key: %w — run `veda audit keygen` first", err)
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config.toml is invalid: %w — fix or delete it in %s", err, config.Home())
		}
		data, err := audit.BuildExport(s, cfg.Telemetry.InstallID, priv, time.Now())
		if err != nil {
			return err
		}
		if *out == "" {
			os.Stdout.Write(data)
			fmt.Println()
			return nil
		}
		return os.WriteFile(*out, data, 0o600)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		in := fs.String("f", "", "signed export file (required)")
		fs.Parse(args[1:])
		if *in == "" {
			return fmt.Errorf("usage: veda audit verify -f <file>")
		}
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		p, err := audit.VerifyExport(data)
		if err != nil {
			return err
		}
		fmt.Printf("SIGNATURE VALID — %d audit entries, exported %s (install %s)\n",
			len(p.Entries), time.Unix(p.ExportedAt, 0).Format(time.RFC3339), p.InstallID)
		return nil
	default:
		return fmt.Errorf("unknown audit command %q", args[0])
	}
}

// --- veda policy ----------------------------------------------------------------

func cmdPolicy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: veda policy <status|enforce>")
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
	pol := cfg.PoliciesOrZero()
	switch args[0] {
	case "status":
		fmt.Printf("retention_days: %d\n", pol.RetentionDays)
		if len(pol.Redact) == 0 {
			fmt.Println("redact:         (none)")
		} else {
			fmt.Println("redact:")
			for _, r := range pol.Redact {
				fmt.Printf("  - %s\n", r)
			}
		}
		if pol.RetentionDays == 0 && len(pol.Redact) == 0 {
			fmt.Println("\nNo org policies configured (defaults). Add a [policies] section to config.toml.")
		}
		return nil
	case "enforce":
		n, err := applyPolicies(s, cfg.Policies)
		if err != nil {
			return err
		}
		fmt.Printf("policy enforce: %d memories retired by retention\n", n)
		return nil
	default:
		return fmt.Errorf("unknown policy command %q", args[0])
	}
}

// --- veda sync ----------------------------------------------------------------

func syncEngine(s *store.Store, cfg *config.Config) *syncengine.Engine {
	return &syncengine.Engine{Store: s, Cfg: cfg.Sync}
}

// --- veda eval ----------------------------------------------------------------

// eval runs fixture scenarios against throwaway stores — the user's real
// ~/.veda is never opened.
func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	file := fs.String("f", "", "scenario file (single scenario or suite) (required)")
	report := fs.String("report", "", "write the full JSON report to this file")
	uploadURL := fs.String("upload-url", "", "upload anonymized aggregate scores (paid tier)")
	fs.Parse(args)
	if *file == "" {
		return fmt.Errorf("usage: veda eval -f <scenario-file> [--report out.json] [--upload-url url]")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	scenarios, err := eval.Load(data)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config.toml is invalid: %w — fix or delete it in %s", err, config.Home())
	}
	opts := eval.Options{}
	if cfg.Embed.Enabled && cfg.Embed.BaseURL != "" {
		opts.Embedder = embed.New(cfg.Embed.BaseURL, cfg.EmbedAPIKey(), cfg.Embed.Model)
	}
	if key := cfg.APIKey(); key != "" {
		opts.Distiller = llm.New(cfg.LLM.BaseURL, key, cfg.LLM.Model)
	}

	ctx := context.Background()
	aggregate := struct {
		Version   string         `json:"version"`
		Passed    bool           `json:"passed"`
		Scenarios int            `json:"scenarios"`
		Failed    int            `json:"failed"`
		Reports   []*eval.Report `json:"reports"`
	}{Version: version, Passed: true, Scenarios: len(scenarios)}
	for _, sc := range scenarios {
		rep, err := eval.Run(sc, opts)
		if err != nil {
			return fmt.Errorf("scenario %q: %w", sc.Name, err)
		}
		aggregate.Reports = append(aggregate.Reports, rep)
		status := "PASS"
		if !rep.Passed {
			status = "FAIL"
			aggregate.Failed++
		}
		fmt.Printf("%s  %s  (recall %.0f%%)", status, sc.Name, rep.Score*100)
		if rep.Interference != nil {
			fmt.Printf("  interference drop %.2f", rep.Interference.Drop)
		}
		if rep.Faithfulness != nil {
			fmt.Printf("  faithfulness avg %.2f (%d flagged)", rep.Faithfulness.AvgScore, rep.Faithfulness.Flagged)
		}
		fmt.Println()
		for _, c := range rep.Cases {
			if !c.Passed {
				fmt.Printf("       case %q: %s\n", c.Query, strings.Join(c.Failures, "; "))
			}
		}
		if !rep.Passed {
			aggregate.Passed = false
		}
	}
	if *report != "" {
		b, _ := json.MarshalIndent(aggregate, "", "  ")
		if err := os.WriteFile(*report, b, 0o600); err != nil {
			return err
		}
		fmt.Printf("report written to %s\n", *report)
	}
	if *uploadURL != "" {
		if err := eval.UploadAggregate(ctx, *uploadURL, cfg.SyncToken(), version, aggregate.Reports); err != nil {
			return err
		}
		fmt.Println("aggregate scores uploaded")
	}
	if !aggregate.Passed {
		return fmt.Errorf("%d/%d scenarios failed", aggregate.Failed, aggregate.Scenarios)
	}
	return nil
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config.toml is invalid: %w — fix or delete it in %s", err, config.Home())
	}
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config.toml is invalid: %w — fix or delete it in %s", err, config.Home())
	}
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
	if _, err := applyPolicies(s, cfg.Policies); err != nil {
		return err
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
