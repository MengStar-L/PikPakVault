package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gofrs/flock"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"pikpakvault/internal/update"
	"pikpakvault/internal/vault"
	"pikpakvault/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "version" {
		fmt.Println(vault.Version)
		return
	}
	if len(args) > 0 && args[0] == "maintenance" {
		if err := maintenance(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(args) > 0 && (args[0] == "backup" || args[0] == "restore" || args[0] == "serve" || args[0] == "reconcile-paths") {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	data := flags.String("data", env("VAULT_DATA", "./data"), "Persistent database and key directory")
	listen := flags.String("listen", env("VAULT_LISTEN", "127.0.0.1:5675"), "HTTP listening address")
	secure := flags.Bool("secure-cookies", env("VAULT_SECURE_COOKIES", "") == "true", "Use Secure session cookies behind HTTPS")
	archive := flags.String("archive", "", "Backup archive path")
	apply := flags.Bool("apply", false, "Apply remote paths to local records (reconcile-paths only)")
	reportPath := flags.String("report", "", "Write path reconciliation report as JSON")
	_ = flags.Parse(args)
	if command == "serve" || command == "reconcile-paths" {
		if err := os.MkdirAll(*data, 0700); err != nil {
			log.Fatal(err)
		}
		lock := flock.New(filepath.Join(*data, "service.lock"))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			log.Fatal("another PikPak Vault process is already using this data directory")
		}
		defer lock.Unlock()
	}
	if command == "restore" {
		if *archive == "" {
			log.Fatal("--archive is required")
		}
		if err := restore(*archive, *data); err != nil {
			log.Fatal(err)
		}
		log.Printf("Restored backup into %s", *data)
		return
	}
	s, err := vault.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	defer s.DB.Close()
	if command == "reconcile-paths" {
		app := vault.NewApp(s)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		result, err := app.ReconcilePaths(ctx, *apply)
		if err != nil {
			log.Fatal(err)
		}
		if *reportPath != "" {
			body, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				log.Fatal(err)
			}
			if err = os.WriteFile(*reportPath, body, 0600); err != nil {
				log.Fatal(err)
			}
		}
		log.Print(result.Summary())
		return
	}
	if command == "backup" {
		if *archive == "" {
			*archive = fmt.Sprintf("pikpak-vault-%s.zip", time.Now().Format("20060102-150405"))
		}
		if err = s.Backup(*archive); err != nil {
			log.Fatal(err)
		}
		log.Printf("Backup written to %s (contains database and master key)", *archive)
		return
	}
	app := vault.NewApp(s)
	app.SecureCookies = *secure
	if s.Get("password") == "" {
		app.SetupToken = env("VAULT_SETUP_TOKEN", vault.ID())
		log.Printf("First-run initialization code: %s", app.SetupToken)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	server := &http.Server{Addr: *listen, Handler: app.Handler(web.Assets()), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("PikPak Vault %s listening on %s", vault.Version, *listen)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	stop()
	<-done
}
func restore(archive, dir string) error {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		return fmt.Errorf("restore requires an empty destination; stop the service and retain the old directory first")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(dir)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, "vault-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	checked, err := vault.ExtractBackup(archive, temp)
	if err != nil {
		return err
	}
	checked.DB.Close()
	restored, err := vault.Open(temp)
	if err != nil {
		return err
	}
	_, err = restored.DB.Exec(`DELETE FROM sessions; UPDATE jobs SET state='paused',message='Restored backup; review before resuming' WHERE state IN ('queued','running','waiting','retry'); INSERT OR REPLACE INTO settings(key,value) VALUES('import_review_required','true');`)
	restored.DB.Close()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return os.Rename(temp, dir)
}
func maintenanceWorker() *update.Worker {
	w := update.NewWorker()
	w.Backup = func(dir, archive string) error {
		s, e := vault.Open(dir)
		if e != nil {
			return e
		}
		defer s.DB.Close()
		return s.Backup(archive)
	}
	w.Restore = func(archive, dir string) error {
		temp, e := os.MkdirTemp(filepath.Dir(archive), "restore-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(temp)
		checked, e := vault.ExtractBackup(archive, temp)
		if e != nil {
			return e
		}
		checked.DB.Close()
		failed, e := os.MkdirTemp(filepath.Dir(archive), "failed-data-")
		if e != nil {
			return e
		}
		for _, name := range []string{"vault.db", "vault.db-wal", "vault.db-shm", "master.key"} {
			if e = os.Rename(filepath.Join(dir, name), filepath.Join(failed, name)); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
		for _, name := range []string{"vault.db", "master.key"} {
			target := filepath.Join(dir, name)
			if e = os.Rename(filepath.Join(temp, name), target); e != nil {
				return e
			}
			if e = exec.Command("chown", "--reference="+dir, "--", target).Run(); e != nil {
				return e
			}
		}
		return nil
	}
	return w
}
func maintenance() error {
	w := maintenanceWorker()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return w.Run(ctx)
}
