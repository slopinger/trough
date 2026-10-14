// Command trough is a small single-binary ActivityPub (fediverse) server with a
// built-in server-rendered web interface. It is modelled loosely on GoToSocial.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	softwareName    = "trough"
	softwareVersion = "0.1.0"
	maxStatusChars  = 5000
)

// Config is the runtime configuration. Everything can be set by flag or by
// TROUGH_* environment variable (flags win).
type Config struct {
	Host     string // public host name, e.g. fedi.freifick.net (may include :port in dev)
	Scheme   string // https (http only with -insecure)
	Listen   string // listen address, e.g. 127.0.0.1:8790
	DataDir  string // database, media and cache live here
	Insecure bool   // dev mode: allow http and private addresses for federation
}

func (c *Config) BaseURL() string { return c.Scheme + "://" + c.Host }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseConfig(fs *flag.FlagSet, args []string) (*Config, error) {
	c := &Config{}
	fs.StringVar(&c.Host, "host", envOr("TROUGH_HOST", "localhost:8790"), "public host name of the instance")
	fs.StringVar(&c.Listen, "listen", envOr("TROUGH_LISTEN", "127.0.0.1:8790"), "address to listen on")
	fs.StringVar(&c.DataDir, "data", envOr("TROUGH_DATA", "./data"), "data directory")
	fs.BoolVar(&c.Insecure, "insecure", os.Getenv("TROUGH_INSECURE") == "1", "dev mode: plain http, allow private IPs")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.Host = strings.ToLower(strings.TrimSuffix(c.Host, "/"))
	c.Scheme = "https"
	if c.Insecure {
		c.Scheme = "http"
	}
	return c, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s - a small fediverse server

usage:
  trough serve [flags]                         run the server
  trough user create [flags] <name> [--admin]  create a local account (prints a password)
  trough user password [flags] <name>          set a new random password
  trough user admin [flags] <name> on|off      grant/revoke admin
  trough user list [flags]                     list local accounts
  trough fetch [flags] <url>                   signed GET of an ActivityPub object (debugging)

common flags: -host, -data, -listen, -insecure (or TROUGH_HOST, TROUGH_DATA, ...)
`, softwareName, softwareVersion)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(softwareName, softwareVersion)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	cfg, err := parseConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	app, err := NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("%s %s listening on %s as %s", softwareName, softwareVersion, cfg.Listen, cfg.BaseURL())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	cfg, err := parseConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: trough fetch <url>")
	}
	app, err := NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	m, _, err := app.fetchJSON(context.Background(), fs.Arg(0), apMimeType+", "+ldMimeType)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(out))
	return nil
}

func randomPassword() string {
	b := make([]byte, 15)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cmdUser(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	admin := fs.Bool("admin", false, "make the new user an admin")
	pwStdin := fs.Bool("password-stdin", false, "read the password from stdin instead of generating one")
	cfg, err := parseConfig(fs, args[1:])
	if err != nil {
		return err
	}
	app, err := NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	rest := fs.Args()
	readPw := func() (string, error) {
		if !*pwStdin {
			return randomPassword(), nil
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	switch sub {
	case "create":
		if len(rest) != 1 {
			return errors.New("usage: trough user create <name> [-admin]")
		}
		pw, err := readPw()
		if err != nil {
			return err
		}
		acc, err := app.CreateLocalUser(rest[0], pw, *admin)
		if err != nil {
			return err
		}
		fmt.Printf("created @%s@%s\n", acc.Username, cfg.Host)
		if !*pwStdin {
			fmt.Printf("password: %s\n", pw)
		}
	case "password":
		if len(rest) != 1 {
			return errors.New("usage: trough user password <name>")
		}
		pw, err := readPw()
		if err != nil {
			return err
		}
		acc, err := app.LocalAccount(rest[0])
		if err != nil {
			return err
		}
		if err := app.SetPassword(acc.ID, pw); err != nil {
			return err
		}
		if !*pwStdin {
			fmt.Printf("password: %s\n", pw)
		}
	case "admin":
		if len(rest) != 2 {
			return errors.New("usage: trough user admin <name> on|off")
		}
		acc, err := app.LocalAccount(rest[0])
		if err != nil {
			return err
		}
		_, err = app.db.Exec(`UPDATE users SET admin=? WHERE account_id=?`, rest[1] == "on", acc.ID)
		return err
	case "list":
		users, err := app.ListUsers()
		if err != nil {
			return err
		}
		for _, u := range users {
			flag := ""
			if u.Admin {
				flag = " (admin)"
			}
			fmt.Printf("@%s@%s%s\n", u.Account.Username, cfg.Host, flag)
		}
	default:
		usage()
		os.Exit(2)
	}
	return nil
}
