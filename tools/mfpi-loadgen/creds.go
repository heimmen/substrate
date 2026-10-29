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
	"encoding/json"
	"fmt"
	"os"
)

// UserCred is one stress user: the actor name doubles as the pi-web login and
// the URL path prefix.
type UserCred struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// CredsFile is the on-disk format shared by the "users", "run" and "cleanup"
// subcommands.
type CredsFile struct {
	AdminURL  string     `json:"adminUrl,omitempty"`
	Atespace  string     `json:"atespace,omitempty"`
	Template  string     `json:"template,omitempty"`
	Namespace string     `json:"namespace,omitempty"`
	Users     []UserCred `json:"users"`
}

func loadCreds(path string) (*CredsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read creds %q: %w", path, err)
	}
	var c CredsFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse creds %q: %w", path, err)
	}
	if len(c.Users) == 0 {
		return nil, fmt.Errorf("creds %q contains no users", path)
	}
	for i, u := range c.Users {
		if u.Name == "" || u.Password == "" {
			return nil, fmt.Errorf("creds %q: user #%d is missing name or password", path, i)
		}
	}
	return &c, nil
}

func saveCreds(path string, c *CredsFile) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// 0600: the file contains plaintext passwords.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write creds %q: %w", path, err)
	}
	return nil
}
