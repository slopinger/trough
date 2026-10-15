package server

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// Name and version reported to other servers and clients.
const (
	SoftwareName    = "trough"
	SoftwareVersion = "0.1.0"
	maxStatusChars  = 5000
)

// Config is the runtime configuration. Everything can be set by flag or by
// TROUGH_* environment variable (flags win).
type Config struct {
	Host     string // public host name, e.g. fedi.freifick.net (may include :port in dev)
	Scheme   string // https (http by default with -insecure; -scheme https for a local TLS proxy)
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

// ParseConfig reads the common flags (and TROUGH_* variables) into a Config.
func ParseConfig(fs *flag.FlagSet, args []string) (*Config, error) {
	c := &Config{}
	fs.StringVar(&c.Host, "host", envOr("TROUGH_HOST", "localhost:8790"), "public host name of the instance")
	fs.StringVar(&c.Listen, "listen", envOr("TROUGH_LISTEN", "127.0.0.1:8790"), "address to listen on")
	fs.StringVar(&c.DataDir, "data", envOr("TROUGH_DATA", "./data"), "data directory")
	fs.BoolVar(&c.Insecure, "insecure", os.Getenv("TROUGH_INSECURE") == "1", "dev mode: plain http, allow private IPs")
	fs.StringVar(&c.Scheme, "scheme", os.Getenv("TROUGH_SCHEME"), "with -insecure: https when behind a local TLS proxy (default http)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.Host = strings.ToLower(strings.TrimSuffix(c.Host, "/"))
	switch {
	case !c.Insecure:
		c.Scheme = "https"
	case c.Scheme == "":
		c.Scheme = "http"
	case c.Scheme != "http" && c.Scheme != "https":
		return nil, fmt.Errorf("-scheme must be http or https")
	}
	return c, nil
}
