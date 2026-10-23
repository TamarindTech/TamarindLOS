// Command tamarind runs the Tamarind LOS core service.
//
//	tamarind validate      check the tool catalogue and policy pack (used in CI)
//	tamarind migrate       apply database migrations (optionally --seed)
//	tamarind serve         run the REST + MCP gateway
//	tamarind verify-audit  verify a tenant's hash-chained event log
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/api"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/seed"
	"github.com/TamarindTech/TamarindLOS/services/core/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "migrate":
		err = cmdMigrate(ctx, os.Args[2:], log)
	case "serve":
		err = cmdServe(ctx, os.Args[2:], log)
	case "verify-audit":
		err = cmdVerifyAudit(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tamarind <validate|migrate|serve|verify-audit> [flags]")
}

type common struct {
	tools, pack, principals string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.tools, "tools", env("TAMARIND_TOOLS_DIR", "tools"), "tool catalogue directory")
	fs.StringVar(&c.pack, "pack", env("TAMARIND_POLICY_PACK", "policy-packs/baseline/sme-working-capital"), "policy pack directory")
	fs.StringVar(&c.principals, "principals", env("TAMARIND_PRINCIPALS", "deploy/compose/principals.yaml"), "development principals file")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func load(c common) (*catalogue.Catalogue, *policy.Pack, error) {
	cat, err := catalogue.Load(c.tools)
	if err != nil {
		return nil, nil, fmt.Errorf("tool catalogue: %w", err)
	}
	pack, err := policy.LoadPack(c.pack)
	if err != nil {
		return nil, nil, fmt.Errorf("policy pack: %w", err)
	}
	if problems := pack.Validate(cat); len(problems) > 0 {
		return nil, nil, fmt.Errorf("policy pack does not match the catalogue:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cat, pack, nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	cat, pack, err := load(c)
	if err != nil {
		return err
	}
	fmt.Printf("tool catalogue: %d tools valid\n", len(cat.Names))
	fmt.Printf("policy pack %s@%s: %d rules, %d state transitions, %d Cedar policies\n",
		pack.Name, pack.Version, len(pack.Rules), len(pack.StateMachine.Transitions), pack.Authz.PolicyCount())
	results, err := pack.RunRuleTests()
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		mark := "ok  "
		if !r.Passed {
			mark, failed = "FAIL", failed+1
		}
		fmt.Printf("  %s rule test %-30q got %s %v, want %s %v\n", mark, r.Name, r.Got, r.GotFailed, r.Want, r.WantFailed)
	}
	if failed > 0 {
		return fmt.Errorf("%d rule test(s) failed", failed)
	}
	if _, err := auth.LoadRegistry(c.principals); err != nil {
		return fmt.Errorf("principals: %w", err)
	}
	fmt.Println("all checks passed")
	return nil
}

func cmdMigrate(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	var c common
	c.register(fs)
	dsn := fs.String("database-url", env("TAMARIND_ADMIN_DATABASE_URL", ""), "administrator connection string")
	doSeed := fs.Bool("seed", false, "create the tenants from the principals file and their products from the policy pack")
	_ = fs.Parse(args)
	if *dsn == "" {
		return errors.New("--database-url (or TAMARIND_ADMIN_DATABASE_URL) is required")
	}
	conn, err := connectWithRetry(ctx, *dsn, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	applied, err := migrations.Apply(ctx, conn)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "new", applied)
	if pw := os.Getenv("TAMARIND_APP_DB_PASSWORD"); pw != "" {
		if _, err := conn.Exec(ctx, "ALTER ROLE tamarind_app PASSWORD '"+strings.ReplaceAll(pw, "'", "''")+"'"); err != nil {
			return err
		}
		log.Info("application role password set")
	}
	if *doSeed {
		_, pack, err := load(c)
		if err != nil {
			return err
		}
		reg, err := auth.LoadRegistry(c.principals)
		if err != nil {
			return err
		}
		if err := seed.Run(ctx, conn, reg, pack); err != nil {
			return err
		}
		log.Info("seed complete", "tenants", len(reg.Tenants), "product", pack.Product.Code)
	}
	return nil
}

func connectWithRetry(ctx context.Context, dsn string, wait time.Duration) (*pgx.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(time.Second)
	}
}

func cmdServe(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", env("TAMARIND_ADDR", ":8080"), "listen address")
	dsn := fs.String("database-url", env("TAMARIND_DATABASE_URL", ""), "application connection string (tamarind_app role)")
	_ = fs.Parse(args)
	if *dsn == "" {
		return errors.New("--database-url (or TAMARIND_DATABASE_URL) is required")
	}
	cat, pack, err := load(c)
	if err != nil {
		return err
	}
	reg, err := auth.LoadRegistry(c.principals)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	g, err := gateway.New(ctx, cat, pack, reg, pool, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: (&api.Server{G: g, Log: log}).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("tamarind listening", "addr", *addr, "tools", len(cat.Names), "pack", pack.Name+"@"+pack.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdVerifyAudit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-audit", flag.ExitOnError)
	dsn := fs.String("database-url", env("TAMARIND_DATABASE_URL", ""), "connection string")
	tenant := fs.String("tenant", "", "tenant slug")
	_ = fs.Parse(args)
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, *tenant).Scan(&id); err != nil {
		return fmt.Errorf("tenant %q: %w", *tenant, err)
	}
	rep, err := gateway.VerifyChain(ctx, pool, id)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if !rep.Valid {
		return errors.New("audit chain is broken")
	}
	return nil
}
