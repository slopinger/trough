// Command trough is a small single-binary ActivityPub (fediverse) server with a
// built-in server-rendered web interface. It is modelled loosely on GoToSocial.
package main

import (
	"bufio"
	"context"
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

	"git.freifick.net/slop/trough/internal/server"
)

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s - a small fediverse server

usage:
  trough serve [flags]                         run the server
  trough user create [flags] <name> [-admin]   create a local account (prints a password)
  trough user password [flags] <name>          set a new random password
  trough user admin [flags] <name> on|off      grant/revoke admin
  trough user list [flags]                     list local accounts
  trough fetch [flags] <url>                   signed GET of an ActivityPub object (debugging)

common flags: -host, -data, -listen, -insecure, -scheme (or TROUGH_HOST, TROUGH_DATA, ...)
`, server.SoftwareName, server.SoftwareVersion)
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
		fmt.Println(server.SoftwareName, server.SoftwareVersion)
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
	cfg, err := server.ParseConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	app, err := server.NewApp(cfg)
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
	log.Printf("%s %s listening on %s as %s", server.SoftwareName, server.SoftwareVersion, cfg.Listen, cfg.BaseURL())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	cfg, err := server.ParseConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: trough fetch <url>")
	}
	app, err := server.NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	m, err := app.FetchObject(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(out))
	return nil
}

func cmdUser(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	sub := args[0]
	// allow flags after the user name (trough user create alice -admin)
	var flags, pos []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if (name == "host" || name == "listen" || name == "data" || name == "scheme") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	args = append([]string{sub}, append(flags, pos...)...)
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	admin := fs.Bool("admin", false, "make the new user an admin")
	pwStdin := fs.Bool("password-stdin", false, "read the password from stdin instead of generating one")
	cfg, err := server.ParseConfig(fs, args[1:])
	if err != nil {
		return err
	}
	app, err := server.NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	rest := fs.Args()
	readPw := func() (string, error) {
		if !*pwStdin {
			return server.RandomPassword(), nil
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
		return app.SetAdmin(acc.ID, rest[1] == "on")
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
