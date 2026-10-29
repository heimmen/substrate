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
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePrefix(t *testing.T) {
	valid := []string{"st", "stress", "st2", "a-b"}
	for _, p := range valid {
		if err := validatePrefix(p); err != nil {
			t.Errorf("validatePrefix(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{"", "-st", "st-", "St", "st_1", "st.1"}
	for _, p := range invalid {
		if err := validatePrefix(p); err == nil {
			t.Errorf("validatePrefix(%q) = nil, want error", p)
		}
	}
}

func TestCredsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	want := &CredsFile{
		AdminURL:  "http://127.0.0.1:59681",
		Atespace:  "mfpi-test",
		Template:  "ate-demo-mf-pi-test/mf-pi-small",
		Namespace: "ate-demo-mf-pi-test",
		Users: []UserCred{
			{Name: "st-01", Password: "secret1"},
			{Name: "st-02", Password: "secret2"},
		},
	}
	if err := saveCreds(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	got, err := loadCreds(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 2 || got.Users[0] != want.Users[0] || got.Users[1] != want.Users[1] {
		t.Errorf("users = %+v, want %+v", got.Users, want.Users)
	}
}

func TestLoadCredsRejectsEmptyUsers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCreds(path); err == nil {
		t.Error("expected an error for an empty user list")
	}
}

func TestLoadCredsRejectsMissingPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{"users":[{"name":"st-01"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCreds(path); err == nil {
		t.Error("expected an error for a user without a password")
	}
}
