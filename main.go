package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	Addr         string
	DataDir      string
	SiteURL      string
	SiteTitle    string
	SiteDesc     string
	SecurityTxt  string
	PassHash     []byte
	SecureCookie bool
	BehindProxy  bool
}

func loadConfig() (Config, error) {
	c := Config{
		Addr:        env("ADDR", ":8080"),
		DataDir:     env("DATA_DIR", "./data"),
		SiteURL:     strings.TrimRight(env("SITE_URL", "http://localhost:8080"), "/"),
		SiteTitle:   env("SITE_TITLE", "daans.cat"),
		SiteDesc:    env("SITE_DESC", "pictures of daan's cat"),
		SecurityTxt: env("SECURITY_TXT_URL", "https://dnbr.cloud/.well-known/security.txt"),
	}
	c.BehindProxy = env("BEHIND_PROXY", "false") == "true"

	switch env("SECURE_COOKIES", "auto") {
	case "true":
		c.SecureCookie = true
	case "false":
		c.SecureCookie = false
	default:
		c.SecureCookie = strings.HasPrefix(c.SiteURL, "https://")
	}

	h := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD_HASH"))
	if h == "" {
		return c, errors.New("ADMIN_PASSWORD_HASH is not set (generate one with: daans-cat hash)")
	}
	if _, err := bcrypt.Cost([]byte(h)); err != nil {
		return c, fmt.Errorf("ADMIN_PASSWORD_HASH is not a valid bcrypt hash: %w", err)
	}
	c.PassHash = []byte(h)
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash":
			if err := hashCommand(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		case "healthcheck":
			// The container has no shell or curl, so the binary probes itself.
			if err := healthCommand(); err != nil {
				fmt.Fprintln(os.Stderr, "unhealthy:", err)
				os.Exit(1)
			}
			return
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "media"), filepath.Join(cfg.DataDir, "originals")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create data dir:", err)
			os.Exit(1)
		}
	}

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open store:", err)
		os.Exit(1)
	}

	app, err := NewApp(cfg, store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start app:", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // uploads can be big and slow
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		log.Info("listening", "addr", cfg.Addr, "site", cfg.SiteURL, "posts", len(store.List()))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func healthCommand() error {
	addr := env("ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// hashCommand reads a password from stdin and prints a bcrypt hash for
// ADMIN_PASSWORD_HASH. Reading from stdin keeps the password out of shell
// history and out of the process table.
func hashCommand() error {
	fmt.Fprint(os.Stderr, "password: ")
	pw, err := readLine(os.Stdin)
	if err != nil {
		return err
	}
	if len(pw) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println(string(h))
	return nil
}

func readLine(f *os.File) (string, error) {
	buf := make([]byte, 0, 128)
	b := make([]byte, 1)
	for {
		n, err := f.Read(b)
		if n > 0 {
			if b[0] == '\n' {
				break
			}
			if b[0] != '\r' {
				buf = append(buf, b[0])
			}
		}
		if err != nil {
			if len(buf) > 0 {
				break
			}
			return "", err
		}
		if len(buf) > 1024 {
			return "", errors.New("password too long")
		}
	}
	return string(buf), nil
}
