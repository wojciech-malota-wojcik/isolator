package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/ridge/must"
	"github.com/ridge/parallel"
	"github.com/wojciech-malota-wojcik/isolator/executor/wire"
	"github.com/wojciech-malota-wojcik/libexec"
)

// Run runs isolator server
func Run(ctx context.Context, addr string) error {
	addrParts := strings.SplitN(addr, ":", 2)
	if len(addrParts) != 2 || addrParts[0] == "" || addrParts[1] == "" {
		return fmt.Errorf("invalid address: %s", addr)
	}

	listener, err := net.Listen(addrParts[0], addrParts[1])
	if err != nil {
		return err
	}
	defer listener.Close()

	var mu sync.Mutex
	var conn net.Conn
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("connection", parallel.Exit, func(ctx context.Context) error {
			var err error
			connTmp, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}

			mu.Lock()
			conn = connTmp
			mu.Unlock()

			decoder := json.NewDecoder(conn)
			for {
				var msg wire.RunMessage
				if err := decoder.Decode(&msg); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}

				var errStr string
				if err := libexec.Exec(ctx, exec.Command("/bin/sh", "-c", msg.Command)); err != nil {
					errStr = err.Error()
				}
				if _, err := conn.Write(must.Bytes(json.Marshal(wire.Ack{Error: errStr}))); err != nil {
					return err
				}
			}
		})
		spawn("watchdog", parallel.Exit, func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			defer mu.Unlock()

			if conn != nil {
				_ = conn.Close()
			}
			return nil
		})
		return nil
	})
}
