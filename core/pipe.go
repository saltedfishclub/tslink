package core

import (
	"errors"
	"io"
	"log/slog"
	"net"
)

func pipeConns(a, b net.Conn) (toA, toB int64) {
	done := make(chan struct{}, 2)
	var aToB, bToA int64

	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(a, b)
		aToB = n
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			slog.Debug("pipe copy error", "direction", "b->a", "error", err)
		}
		if tc, ok := a.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			a.Close()
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(b, a)
		bToA = n
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			slog.Debug("pipe copy error", "direction", "a->b", "error", err)
		}
		if tc, ok := b.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			b.Close()
		}
	}()

	<-done
	<-done
	return aToB, bToA
}
