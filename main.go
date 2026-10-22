// Fleet Manager serves target BurmillaOS and User Docker versions and
// maintenance windows to fleet-client services, based on configuration
// stored in a Git repository.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
	"github.com/burmilla/os-fleet-manager/internal/gitsync"
	"github.com/burmilla/os-fleet-manager/internal/server"
)

var Version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func duration(key, def string) time.Duration {
	d, err := time.ParseDuration(env(key, def))
	if err != nil {
		log.Fatalf("invalid %s: %v", key, err)
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "validate" {
		validate(os.Args[2:])
		return
	}
	listen := env("FLEET_LISTEN", ":8080")
	dataDir := env("FLEET_DATA_DIR", "/data")
	configFile := env("FLEET_CONFIG_FILE", "fleet.yml")
	localDir := os.Getenv("FLEET_CONFIG_DIR")
	interval := duration("FLEET_SYNC_INTERVAL", "1m")
	lockTTL := duration("FLEET_LOCK_TTL", "30m")

	token := os.Getenv("FLEET_GIT_TOKEN")
	if f := os.Getenv("FLEET_GIT_TOKEN_FILE"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("read FLEET_GIT_TOKEN_FILE: %v", err)
		}
		token = string(trimNewline(data))
	}
	repo := &gitsync.Repo{
		URL:      os.Getenv("FLEET_GIT_URL"),
		Branch:   env("FLEET_GIT_BRANCH", "main"),
		Username: os.Getenv("FLEET_GIT_USERNAME"),
		Token:    token,
		Dir:      filepath.Join(dataDir, "repo"),
	}
	if repo.URL == "" && localDir == "" {
		log.Fatal("either FLEET_GIT_URL or FLEET_CONFIG_DIR must be set")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	srv := server.New(server.NewStore(dataDir, lockTTL))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sync := func() {
		cfg, err := load(ctx, repo, localDir, configFile)
		srv.SyncResult(err)
		if err != nil {
			log.Printf("configuration sync failed, keeping previous configuration: %v", err)
			return
		}
		if old := srv.Config(); old == nil || old.Revision != cfg.Revision {
			log.Printf("loaded fleet configuration revision %s", cfg.Revision)
		}
		srv.SetConfig(cfg)
	}
	sync()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sync()
			}
		}
	}()

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	cert, key := os.Getenv("FLEET_TLS_CERT"), os.Getenv("FLEET_TLS_KEY")
	log.Printf("BurmillaOS Fleet Manager %s listening on %s (TLS %t)", Version, listen, cert != "")
	var err error
	if cert != "" {
		err = httpSrv.ListenAndServeTLS(cert, key)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func load(ctx context.Context, repo *gitsync.Repo, localDir, configFile string) (*fleet.Config, error) {
	dir, revision := localDir, "local"
	if localDir == "" {
		syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		var err error
		if revision, err = repo.Sync(syncCtx); err != nil {
			return nil, err
		}
		dir = repo.Dir
	}
	cfg, err := fleet.Load(filepath.Join(dir, configFile))
	if err != nil {
		return nil, err
	}
	cfg.Revision = revision
	return cfg, nil
}

// validate checks a configuration checkout, e.g. in CI of the
// configuration repository: fleet-manager validate [path/to/fleet.yml]
func validate(args []string) {
	path := "fleet.yml"
	if len(args) > 0 {
		path = args[0]
	}
	cfg, err := fleet.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid fleet configuration: %v\n", err)
		os.Exit(1)
	}
	envs := 0
	for _, sw := range cfg.Swarms {
		envs += len(sw.Environments)
	}
	fmt.Printf("%s is valid: %d swarms, %d environments, %d servers\n", path, len(cfg.Swarms), envs, len(cfg.Servers))
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
