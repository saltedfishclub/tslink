package core

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// These tests exist to give `go test -race` something to chew on. The GUI
// reads every one of these structures from its frame loop while background
// goroutines write to them, which is exactly the shape of bug that never
// shows up in a single-threaded test.

func TestLogBufferConcurrentAccess(t *testing.T) {
	buf := NewLogBuffer(128) // small, so eviction runs constantly
	logger := slog.New(buf.Handler(nil))

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	// Writers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			l := logger.With("from", "writer", "id", id)
			for ctx.Err() == nil {
				l.Info("message", "n", id, "auth_key", "tskey-auth-SECRETVALUE123")
				l.Debug("detail", slog.Group("g", slog.String("k", "v")))
			}
		}(i)
	}

	// Readers, mimicking the GUI's frame loop.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = buf.Tail(50)
				_ = buf.Filter(LogQuery{MinLevel: slog.LevelInfo, Text: "message", Limit: 20})
				_ = buf.Sources()
				_ = buf.Counts()
				_ = buf.Len()
				_ = buf.Dropped()
				_ = buf.LastSeq()
			}
		}()
	}

	// Subscribers churning in and out.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			ch, cancelSub := buf.Subscribe()
			select {
			case <-ch:
			case <-time.After(5 * time.Millisecond):
			}
			cancelSub()
		}
	}()

	// Exporter, which walks the whole ring and redacts.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			out := buf.ExportText(ExportOptions{Query: LogQuery{MinLevel: slog.LevelDebug, Limit: 100}})
			if len(out) > 0 && containsSecret(out) {
				t.Error("export leaked an auth key")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	if buf.Len() > 128 {
		t.Fatalf("ring exceeded its capacity: %d", buf.Len())
	}
	if buf.Dropped() == 0 {
		t.Fatal("expected eviction to have occurred")
	}
}

func containsSecret(s string) bool {
	return len(s) > 0 && (indexOf(s, "SECRETVALUE123") >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestLogBufferTailOrderAndBounds(t *testing.T) {
	buf := NewLogBuffer(4)
	logger := slog.New(buf.Handler(nil))
	for i := 0; i < 10; i++ {
		logger.Info("m", "i", i)
	}

	got := buf.Tail(3)
	if len(got) != 3 {
		t.Fatalf("Tail(3) returned %d entries", len(got))
	}
	// Oldest first, and the newest must be last.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("Tail is not in chronological order: %v", got)
		}
	}
	if got[len(got)-1].Seq != buf.LastSeq() {
		t.Fatalf("Tail did not end at the newest record")
	}
	if n := len(buf.Tail(100)); n != 4 {
		t.Fatalf("Tail beyond capacity returned %d, want 4", n)
	}
	if n := len(buf.Tail(0)); n != 0 {
		t.Fatalf("Tail(0) returned %d entries", n)
	}
}

func TestLogBufferRedactsOnExport(t *testing.T) {
	buf := NewLogBuffer(16)
	logger := slog.New(buf.Handler(nil))
	logger.Info("joining", "auth_key", "tskey-auth-kSomeRealLookingKey123")
	logger.Info("inline", "url", "https://x/?k=tskey-client-abcdefghijkl")

	out := buf.ExportText(ExportOptions{Query: LogQuery{MinLevel: slog.LevelDebug}})
	if indexOf(out, "kSomeRealLookingKey123") >= 0 {
		t.Error("attribute-named secret survived redaction")
	}
	if indexOf(out, "abcdefghijkl") >= 0 {
		t.Error("inline tskey survived redaction")
	}

	raw := buf.ExportText(ExportOptions{Query: LogQuery{MinLevel: slog.LevelDebug}, NoRedact: true})
	if indexOf(raw, "kSomeRealLookingKey123") < 0 {
		t.Error("NoRedact should preserve the original text")
	}
}

func TestLanScannerConcurrentAccess(t *testing.T) {
	s := NewLanScanner(slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Start(ctx)

	var wg sync.WaitGroup

	// Feed announcements the way the read loops do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			src := netip.AddrPortFrom(netip.MustParseAddr("192.168.1.50"), uint16(40000+i%3))
			s.handle(src, "[MOTD]§aTest §bServer[/MOTD][AD]25565[/AD]")
			s.handle(src, "malformed packet")
		}
	}()

	// Read like the GUI does.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				for _, srv := range s.Servers() {
					_ = srv.Motd
					_ = srv.Addr
				}
				_ = s.Err()
			}
		}()
	}

	// Reconfigure while it runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			s.SetSelfEntries([]LanEntry{{Motd: "Test Server", Port: 25565}})
			time.Sleep(time.Millisecond)
			s.SetSelfEntries(nil)
		}
	}()

	wg.Wait()

	servers := s.Servers()
	if len(servers) == 0 {
		t.Fatal("expected the synthetic announcements to be recorded")
	}
	for _, srv := range servers {
		if srv.Port != 25565 {
			t.Errorf("unexpected port %d", srv.Port)
		}
		// Colour codes must be stripped.
		if indexOf(srv.Motd, "§") >= 0 {
			t.Errorf("colour codes survived: %q", srv.Motd)
		}
		if srv.Motd != "Test Server" {
			t.Errorf("motd = %q, want %q", srv.Motd, "Test Server")
		}
	}
}

func TestPeerMonitorSnapshotIsIsolated(t *testing.T) {
	m := NewPeerMonitor(nil, nil, slog.New(slog.DiscardHandler), PeerMonitorOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// A nil server must not panic; the monitor should degrade to an invalid
	// snapshot with an error rather than taking the GUI down.
	m.Start(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				snap := m.Snapshot()
				for _, p := range snap.Peers {
					_ = p.DisplayName
					_ = len(p.Samples)
				}
				_ = m.History("nonexistent")
				m.RefreshNow()
			}
		}()
	}
	wg.Wait()

	snap := m.Snapshot()
	if snap.Valid {
		t.Error("snapshot from a nil tsnet server should not be valid")
	}
}
