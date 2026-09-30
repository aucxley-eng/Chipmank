// Package config reads ingress configuration from the environment.
//
// Everything has a safe default or fails loudly. There is deliberately no
// "unset means guess" behaviour: an ingress service that starts with the wrong
// secret would reject real webhooks, and one that starts with no secret at all
// would accept unsigned ones.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// EnvAppSecret is the fallback single-app variable, convenient for local dev.
const EnvAppSecret = "META_APP_SECRET"

type Config struct {
	Addr                string
	MaxBodyBytes        int64
	Apps                map[string][]byte
	DatabaseURL         string
	AllowEphemeralStore bool
}

func Load() (Config, error) {
	cfg := Config{
		Addr:         env("INGRESS_ADDR", ":8080"),
		Apps:         map[string][]byte{},
		MaxBodyBytes: DefaultMaxBodyBytes,
	}

	if v := os.Getenv("INGRESS_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("config: INGRESS_MAX_BODY_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxBodyBytes = n
	}

	if v := os.Getenv("INGRESS_APPS"); v != "" {
		for pair := range strings.SplitSeq(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			id, secret, ok := strings.Cut(pair, "=")
			id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
			if !ok || id == "" || secret == "" {
				return cfg, fmt.Errorf("config: INGRESS_APPS entry %q must look like app_id=secret", pair)
			}
			cfg.Apps[id] = []byte(secret)
		}
	}

	cfg.AllowEphemeralStore = os.Getenv("INGRESS_ALLOW_EPHEMERAL_STORE") == "1"
	cfg.DatabaseURL = env("INGRESS_DATABASE_URL", os.Getenv("DATABASE_URL"))

	// Single-app convenience for the no-database path: the app id comes from the
	// URL Meta was configured with, the secret from a plain variable.
	//
	// META_APP_SECRET also serves a second, unrelated purpose when a database
	// IS configured: it is the environment variable that
	// channel_accounts.credential_ref points at. In that case no app id is
	// needed, because the reference resolves the app id from the database.
	if secret := strings.TrimSpace(os.Getenv(EnvAppSecret)); secret != "" {
		id := strings.TrimSpace(os.Getenv("META_APP_ID"))
		switch {
		case id != "":
			cfg.Apps[id] = []byte(secret)
		case cfg.DatabaseURL == "":
			return cfg, fmt.Errorf("config: %s set but META_APP_ID is empty", EnvAppSecret)
		default:
			// Reached through credential_ref; nothing to register here.
		}
	}

	// Without a database there is nowhere to resolve credential references
	// from, so at least one inline secret must be configured or nothing can be
	// verified.
	if len(cfg.Apps) == 0 && cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf(
			"config: no app secrets configured; set %s with META_APP_ID, or INGRESS_APPS=app_id=secret,...", EnvAppSecret)
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// AppIDs lists configured app ids, sorted, for startup logging.
func (c Config) AppIDs() []string {
	ids := make([]string, 0, len(c.Apps))
	for id := range c.Apps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DefaultMaxBodyBytes mirrors the handler default so config and handler agree.
const DefaultMaxBodyBytes = 8 << 20
