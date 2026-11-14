package ligusauth

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func osStderr() io.Writer { return os.Stderr }

const usage = `ligus-auth: sign in with any OIDC provider, get short-lived relay tokens

  ligus-auth [serve] [--config FILE]   run the service (default; LIGUS_AUTH_CONFIG)
  ligus-auth keygen --out FILE         create an issuer key (0600); prints only the public key
  ligus-auth pubkey [--config FILE]    print the issuer public key for LIGUS_RELAY_TOKEN_PUBKEY
  ligus-auth check-config [--config FILE]
  ligus-auth gen-token                 print a random token (for SCIM or the admin API)
  ligus-auth hash-token                read a token on stdin, print its SHA-256 (scim_token_sha256)

Secrets: the issuer key comes from signing_key_file, LIGUS_AUTH_SIGNING_KEY_FILE,
a systemd credential named ligus-auth-signing-key, or LIGUS_AUTH_SIGNING_KEY.
The admin API token comes from LIGUS_AUTH_ADMIN_TOKEN_FILE or LIGUS_AUTH_ADMIN_TOKEN.
`

// Main runs the ligus-auth binary.
func Main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "keygen":
		err = cmdKeygen(args)
	case "pubkey":
		err = cmdPubkey(args)
	case "check-config":
		err = cmdCheck(args)
	case "gen-token":
		fmt.Println(randToken(32))
	case "hash-token":
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			err = rerr
			break
		}
		if strings.TrimSpace(line) == "" {
			err = errors.New("no token on stdin")
			break
		}
		fmt.Println(sha256Hex(strings.TrimSpace(line)))
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ligus-auth:", err)
		os.Exit(1)
	}
}

func configFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	def := os.Getenv("LIGUS_AUTH_CONFIG")
	if def == "" {
		def = "/etc/ligus-auth/config.json"
	}
	path := fs.String("config", def, "configuration file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *path, nil
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to create (mode 0600; never overwritten)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out FILE is required (the key is never printed)")
	}
	pub, err := WriteNewSigningKey(*out)
	if err != nil {
		return err
	}
	fmt.Printf("public_key=%s\n# Set LIGUS_RELAY_TOKEN_PUBKEY=%s on the relay. The private key stays in %s.\n", pub, pub, *out)
	return nil
}

func cmdPubkey(args []string) error {
	path, err := configFlag("pubkey", args)
	if err != nil {
		return err
	}
	file := ""
	if c, err := LoadConfig(path); err == nil {
		file = c.SigningKeyFile
	}
	k, _, err := LoadSigningKey(file)
	if err != nil {
		return err
	}
	srvPub := k.Public()
	fmt.Printf("%x\n", srvPub)
	return nil
}

func cmdCheck(args []string) error {
	path, err := configFlag("check-config", args)
	if err != nil {
		return err
	}
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if _, _, err := LoadSigningKey(c.SigningKeyFile); err != nil {
		return err
	}
	fmt.Printf("ok: %d tenant(s)\n", len(c.Tenants))
	return nil
}

func adminTokenFromEnv() (string, error) {
	return readSecret(os.Getenv("LIGUS_AUTH_ADMIN_TOKEN_FILE"), envName("LIGUS_AUTH_ADMIN_TOKEN"))
}

// envName returns name if that env var is set, else "" (so readSecret treats
// an unset variable as "not configured").
func envName(name string) string {
	if _, ok := os.LookupEnv(name); ok {
		return name
	}
	return ""
}

func openStore(ctx context.Context, c *Config) (Store, error) {
	if c.Store.Driver == "postgres" {
		dsn := os.Getenv(c.Store.DSNEnv)
		if dsn == "" {
			return nil, fmt.Errorf("store postgres: %s is empty", c.Store.DSNEnv)
		}
		return NewPostgresStore(ctx, dsn)
	}
	if c.Store.Path == "" {
		slog.Warn("ligus-auth: store.path is empty; users and sessions are lost on restart")
	}
	return NewFileStore(c.Store.Path)
}

func cmdServe(args []string) error {
	path, err := configFlag("serve", args)
	if err != nil {
		return err
	}
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	key, source, err := LoadSigningKey(c.SigningKeyFile)
	if err != nil {
		return err
	}
	if source == "env" {
		slog.Warn("ligus-auth: issuer key read from LIGUS_AUTH_SIGNING_KEY; prefer a 0600 file or a systemd credential")
	}
	adminTok, err := adminTokenFromEnv()
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	defer store.Close()
	var audit io.Writer
	if c.AuditLog != "" {
		f, err := os.OpenFile(c.AuditLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		defer f.Close()
		audit = f
	}
	srv, err := New(ctx, Options{Config: c, Store: store, Key: key, AdminToken: adminTok, AuditWriter: audit})
	if err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if err := store.PruneRefresh(ctx, now.Unix()); err != nil {
					slog.Error("ligus-auth: prune refresh tokens", "error", err)
				}
			}
		}
	}()
	mk := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
	}
	servers := []*http.Server{mk(c.Listen, srv.Handler())}
	if c.AdminListen != "" && adminTok != "" {
		servers = append(servers, mk(c.AdminListen, srv.AdminHandler()))
	} else if c.AdminListen != "" {
		slog.Warn("ligus-auth: admin_listen is set but no admin token; the admin API stays off")
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) { errc <- s.ListenAndServe() }(s)
	}
	slog.Info("ligus-auth listening", "addr", c.Listen, "admin", c.AdminListen != "" && adminTok != "", "tenants", len(c.Tenants), "token_pubkey", srv.PublicKeyHex())
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}
