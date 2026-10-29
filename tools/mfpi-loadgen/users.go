// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

func runUsers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("users", flag.ContinueOnError)
	count := fs.Int("count", 8, "number of users to create")
	prefix := fs.String("prefix", "st", "user name prefix (names become <prefix>-01, <prefix>-02, ...)")
	atespace := fs.String("atespace", envDefault("MFPI_ATESPACE", "mfpi"), "atespace to create the actors in")
	template := fs.String("template", envDefault("MFPI_TEMPLATE", "ate-demo-mf-pi/mf-pi-small"), "ActorTemplate <namespace>/<name>")
	adminURL := fs.String("admin-url", envDefault("MFPI_ENTRY", "http://127.0.0.1:58681"), "gateway base URL (same as -base-url for run)")
	adminUser := fs.String("admin-user", envDefault("MFPI_ADMIN_USER", "admin"), "admin username")
	adminPass := fs.String("admin-pass", envDefault("MFPI_ADMIN_PASSWORD", "mf@pass2026"), "admin password")
	credsPath := fs.String("creds", "creds.json", "output credentials file")
	kubectl := fs.String("kubectl", "kubectl", "kubectl binary")
	dryRun := fs.Bool("dry-run", false, "print the plan without touching the cluster")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *count < 1 {
		return fmt.Errorf("-count must be >= 1")
	}
	if err := validatePrefix(*prefix); err != nil {
		return err
	}

	creds := &CredsFile{
		AdminURL: strings.TrimRight(*adminURL, "/"),
		Atespace: *atespace,
		Template: *template,
		Users:    make([]UserCred, 0, *count),
	}
	width := len(fmt.Sprintf("%d", *count))
	if width < 2 {
		width = 2
	}

	hc := &http.Client{Timeout: *timeout}
	for i := 1; i <= *count; i++ {
		name := fmt.Sprintf("%s-%0*d", *prefix, width, i)
		if *dryRun {
			fmt.Printf("would create %s in atespace %s from %s\n", name, *atespace, *template)
			continue
		}
		created, err := ensureActor(ctx, *kubectl, *atespace, *template, name)
		if err != nil {
			return fmt.Errorf("ensure actor %s: %w", name, err)
		}
		password, err := resetPassword(ctx, hc, creds.AdminURL, *adminUser, *adminPass, name)
		if err != nil {
			return fmt.Errorf("reset password for %s: %w", name, err)
		}
		state := "reused"
		if created {
			state = "created"
		}
		fmt.Printf("%-8s %s\n", state, name)
		creds.Users = append(creds.Users, UserCred{Name: name, Password: password})
	}
	if *dryRun {
		return nil
	}
	if err := saveCreds(*credsPath, creds); err != nil {
		return err
	}
	fmt.Printf("\nwrote %d users to %s (0600)\n", len(creds.Users), *credsPath)
	fmt.Printf("next: mfpi-loadgen run -base-url %s -creds %s -scenario idle-browser -vus 1 -duration 2m\n",
		creds.AdminURL, *credsPath)
	return nil
}

func runCleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envDefault("MFPI_ENTRY", "http://127.0.0.1:58681"), "gateway base URL; overrides creds.adminUrl when set")
	adminUser := fs.String("admin-user", envDefault("MFPI_ADMIN_USER", "admin"), "admin username")
	adminPass := fs.String("admin-pass", envDefault("MFPI_ADMIN_PASSWORD", "mf@pass2026"), "admin password")
	credsPath := fs.String("creds", "creds.json", "credentials file produced by the users subcommand")
	prefix := fs.String("prefix", "", "only delete users whose name starts with this prefix (safety guard)")
	kubectl := fs.String("kubectl", "kubectl", "kubectl binary")
	atespace := fs.String("atespace", "", "atespace (overrides creds.atespace)")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	dryRun := fs.Bool("dry-run", false, "print the plan without deleting anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	creds, err := loadCreds(*credsPath)
	if err != nil {
		return err
	}
	base := creds.AdminURL
	if isFlagSet(fs, "admin-url") {
		base = *adminURL
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return fmt.Errorf("no admin URL: pass -admin-url or set adminUrl in the creds file")
	}
	as := creds.Atespace
	if *atespace != "" {
		as = *atespace
	}

	hc := &http.Client{Timeout: *timeout}
	var failed int
	for _, u := range creds.Users {
		if *prefix != "" && !strings.HasPrefix(u.Name, *prefix) {
			fmt.Printf("skip     %s (does not match -prefix %q)\n", u.Name, *prefix)
			continue
		}
		if *dryRun {
			fmt.Printf("would delete %s\n", u.Name)
			continue
		}
		err := deleteUser(ctx, hc, base, *adminUser, *adminPass, u.Name)
		if err == nil {
			fmt.Printf("deleted  %s\n", u.Name)
			continue
		}
		// Fall back to deleting the actor directly (keeps the PV, but removes
		// the consumer of a worker); report it as a partial cleanup.
		if _, kerr := ensureActorAbsent(ctx, *kubectl, as, u.Name); kerr == nil {
			fmt.Printf("deleted  %s (actor only; PV not purged)\n", u.Name)
			continue
		}
		failed++
		fmt.Printf("FAILED   %s: %v\n", u.Name, err)
	}
	if *dryRun {
		return nil
	}
	if failed > 0 {
		return fmt.Errorf("%d user(s) could not be deleted", failed)
	}
	return nil
}

func validatePrefix(p string) error {
	if p == "" {
		return fmt.Errorf("-prefix must not be empty")
	}
	if strings.HasPrefix(p, "-") || strings.HasSuffix(p, "-") {
		return fmt.Errorf("-prefix %q must not start or end with '-'", p)
	}
	for _, r := range p {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("-prefix %q must be a DNS-1123 label prefix (a-z, 0-9, '-')", p)
		}
	}
	return nil
}

// ensureActor creates the actor if it does not exist. Actors created this way
// are SUSPENDED, so preparing 64 users does not consume any worker.
func ensureActor(ctx context.Context, kubectl, atespace, template, name string) (bool, error) {
	if err := exec.CommandContext(ctx, kubectl, "ate", "get", "actor", name, "-a", atespace).Run(); err == nil {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, kubectl, "ate", "create", "actor", name, "-a", atespace, "--template", template)
	out, err := cmd.CombinedOutput()
	if err != nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "already exists") || strings.Contains(low, "alreadyexists") {
			return false, nil
		}
		return false, fmt.Errorf("kubectl ate create actor: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func ensureActorAbsent(ctx context.Context, kubectl, atespace, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, kubectl, "ate", "delete", "actor", name, "-a", atespace)
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "not found") {
		return false, fmt.Errorf("kubectl ate delete actor: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func resetPassword(ctx context.Context, hc *http.Client, base, adminUser, adminPass, name string) (string, error) {
	u := base + "/usermanagement/api/users/" + url.PathEscape(name) + "/password"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(adminUser, adminPass)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Password == "" {
		return "", fmt.Errorf("POST %s: unexpected response: %s", u, strings.TrimSpace(string(body)))
	}
	return out.Password, nil
}

func deleteUser(ctx context.Context, hc *http.Client, base, adminUser, adminPass, name string) error {
	u := base + "/usermanagement/api/users/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(adminUser, adminPass)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// isFlagSet reports whether the named flag was explicitly passed. The standard
// flag package does not expose this, so we use Visit after Parse.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
