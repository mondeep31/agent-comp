package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/mondeep31/agent-comp/internal/sandbox"
)

// wsKeepalive is well under Cloudflare's 100 s idle timeout.
const wsKeepalive = 20 * time.Second

type wsStream struct {
	ctx  context.Context //nolint:containedctx // bound to the connection's lifetime
	conn *websocket.Conn
	typ  string
}

func (s *wsStream) Write(p []byte) (int, error) {
	if err := wsjson.Write(s.ctx, s.conn, WSMessage{Type: s.typ, Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Server) execWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxJSONBody)
	ctx := r.Context()

	sendErr := func(err error) {
		_, code, public := classify(err)
		msg := err.Error()
		if !public {
			s.log.Error("exec failed", "path", r.URL.Path, "err", err)
			msg = "internal error"
		}
		_ = wsjson.Write(ctx, conn, WSMessage{Type: WSError, Error: &ErrorDetail{Code: code, Message: msg}})
		conn.Close(websocket.StatusNormalClosure, "")
	}

	var start WSMessage
	if err := wsjson.Read(ctx, conn, &start); err != nil {
		return
	}
	if start.Type != WSStart {
		sendErr(fmt.Errorf("%w: first message must be %q", sandbox.ErrInvalid, WSStart))
		return
	}
	req := &sandbox.ExecRequest{
		Argv: start.Argv, Command: start.Command, Env: start.Env, Cwd: start.Cwd, TimeoutSeconds: start.TimeoutSeconds,
	}
	proc, done, err := s.m.StartExec(ctx, Owner(ctx), r.PathValue("id"), req,
		&wsStream{ctx: ctx, conn: conn, typ: WSStdout}, &wsStream{ctx: ctx, conn: conn, typ: WSStderr})
	if err != nil {
		sendErr(err)
		return
	}
	defer done()
	if err := wsjson.Write(ctx, conn, WSMessage{Type: WSStarted}); err != nil {
		proc.Kill()
		return
	}

	go func() {
		for {
			var m WSMessage
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				// Client went away: agentd kills the process group.
				proc.Kill()
				return
			}
			switch m.Type {
			case WSStdin:
				_, _ = proc.Write(m.Data)
			case WSStdinEOF:
				_ = proc.CloseStdin()
			case WSSignal:
				_ = proc.Signal(m.Signal)
			}
		}
	}()
	go func() {
		t := time.NewTicker(wsKeepalive)
		defer t.Stop()
		for {
			select {
			case <-proc.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, wsKeepalive)
				_ = conn.Ping(pctx)
				cancel()
			}
		}
	}()

	exit, err := proc.Wait()
	if err != nil {
		sendErr(err)
		return
	}
	code := exit.GetExitCode()
	_ = wsjson.Write(ctx, conn, WSMessage{
		Type: WSExit, ExitCode: &code, Signal: exit.GetSignal(), TimedOut: exit.GetTimedOut(), DurationMs: exit.GetDurationMs(),
	})
	conn.Close(websocket.StatusNormalClosure, "")
}
