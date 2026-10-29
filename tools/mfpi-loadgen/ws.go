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
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// startWS keeps a WebSocket open for the lifetime of ctx, reconnecting with a
// short backoff when the connection drops. It is intentionally forgiving: a
// real browser also reconnects, and measuring that churn is part of the test.
func startWS(ctx context.Context, c *userClient, path string, stats *Stats, scenario string) {
	go func() {
		first := true
		for ctx.Err() == nil {
			conn, err := c.dialWS(ctx, path)
			if err != nil {
				stats.RecordWS("error")
				stats.RecordError(fmt.Sprintf("ws dial: %v", err))
				if !sleepCtx(ctx, 2*time.Second) {
					return
				}
				continue
			}
			if first {
				stats.RecordWS("connect")
				first = false
			} else {
				stats.RecordWS("reconnect")
			}
			err = readWS(ctx, conn, stats)
			_ = conn.Close()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("[ws %s] %v", path, err)
				stats.RecordError(fmt.Sprintf("ws read: %v", trimErr(err)))
			}
			if !sleepCtx(ctx, time.Second) {
				return
			}
		}
	}()
}

func readWS(ctx context.Context, conn *websocket.Conn, stats *Stats) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		if _, _, err := conn.ReadMessage(); err != nil {
			return err
		}
		stats.RecordWS("message")
	}
}

// sleepCtx sleeps for d or returns false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
