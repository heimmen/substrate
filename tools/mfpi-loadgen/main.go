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

// Command mfpi-loadgen is a load generator and mock LLM for the mf-pi demo.
//
// It is deliberately a single binary with subcommands so the same artifact can
// be run on the host (against the 58681/59681 gateway) and deployed in-cluster
// as the mock model provider.
//
// Subcommands:
//
//	users      create suspended stress users and assign them passwords
//	cleanup    delete stress users (and purge their persistent volumes)
//	run        run a load scenario against the mf-pi gateway
//	serve-mock run an OpenAI-compatible mock model server
//	analyze-log summarize an nginx access log (traffic model calibration)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usageText = `mfpi-loadgen - load generator + mock LLM for the mf-pi demo

Usage:
  mfpi-loadgen <command> [flags]

Commands:
  users       create suspended stress users and assign passwords
  cleanup     delete stress users created by "users"
  run         run a load scenario (idle-browser, agent-turn, admin)
  serve-mock  run an OpenAI-compatible mock model server
  analyze-log summarize an nginx access log

Run "mfpi-loadgen <command> -h" for the flags of a command.

Examples:
  # 1) create 16 suspended users in the test atespace and their passwords
  mfpi-loadgen users -admin-url http://127.0.0.1:59681 \
      -atespace mfpi-test -template ate-demo-mf-pi-test/mf-pi-small \
      -count 16 -creds creds-test.json

  # 2) one-user smoke test
  mfpi-loadgen run -base-url http://127.0.0.1:59681 -creds creds-test.json \
      -scenario idle-browser -vus 1 -duration 2m

  # 3) ramp to 16 concurrent browsers
  mfpi-loadgen run -base-url http://127.0.0.1:59681 -creds creds-test.json \
      -scenario idle-browser -vus 16 -ramp 60s -duration 10m -report idle.json

  # 4) agent turns against the in-cluster mock model
  mfpi-loadgen run -base-url http://127.0.0.1:59681 -creds creds-test.json \
      -scenario agent-turn -vus 8 -provider mock -model mock-1 \
      -turn-interval 60s -duration 10m

  # 5) clean up
  mfpi-loadgen cleanup -admin-url http://127.0.0.1:59681 -creds creds-test.json
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "users":
		err = runUsers(ctx, os.Args[2:])
	case "cleanup":
		err = runCleanup(ctx, os.Args[2:])
	case "run":
		err = runLoad(ctx, os.Args[2:])
	case "serve-mock":
		err = runMock(ctx, os.Args[2:])
	case "analyze-log":
		err = runAnalyzeLog(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usageText)
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
