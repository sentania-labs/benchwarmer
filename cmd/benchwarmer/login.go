package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
	"github.com/sentania-labs/benchwarmer/internal/service"
)

// cmdLogin registers a single-use dashboard sign-in code (ADR 0012). It
// needs to read the management token, so it runs as an administrator: the
// tray starts it elevated with --code and opens the dashboard itself, so the
// browser never runs elevated. Run by hand, it prints the address to open.
// The code is redeemed for a session token that works only on this PC and
// expires; the management token never leaves this process.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	data := fs.String("data", service.DefaultDataDir(), "data directory")
	code := fs.String("code", "", "sign-in code to register (default: generate one and print the address)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *code != "" {
		// Started by the tray: the session must end up with the person who
		// approved it.
		if err := checkSameUser(); err != nil {
			return err
		}
	}
	base, tokenFile := managementEndpoint(*data)
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read the management token (run as administrator): %w", err)
	}
	c := *code
	if c == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		c = base64.RawURLEncoding.EncodeToString(b)
	}
	if err := registerSignIn(base, strings.TrimSpace(string(tok)), c); err != nil {
		return err
	}
	if *code == "" {
		fmt.Printf("Open within %s: %s/#signin=%s\n", api.SignInCodeTTL, base, c)
	}
	return nil
}

// managementEndpoint reads the configured management address and token file,
// falling back to the defaults when the config cannot be read.
func managementEndpoint(dataDir string) (base, tokenFile string) {
	c := config.Default()
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		if pc, err := config.Parse(b); err == nil {
			c = pc
		}
	}
	host, port, err := net.SplitHostPort(c.Listen.Management)
	if err != nil {
		host, port = "127.0.0.1", "8481"
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), secrets.Resolve(dataDir, secrets.FilesFrom(c.Security).Management)
}

func registerSignIn(base, token, code string) error {
	body, _ := json.Marshal(api.SignInCode{Code: code})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/signin/codes", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("reach the Benchwarmer service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e api.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New("sign-in refused: " + e.Error)
		}
		return fmt.Errorf("sign-in refused: HTTP %d", resp.StatusCode)
	}
	return nil
}
