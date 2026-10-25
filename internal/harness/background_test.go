package harness

import (
	"sync/atomic"
	"testing"
	"time"
)

// A4: the background review fork runs async and writes through the gate, but
// only when [taste] background is on and a curator model exists. Off by default
// it must not even read the transcript.

func TestBackgroundCurateOffByDefault(t *testing.T) {
	app, calls := curatorApp(t, `[{"section":"state","op":"replace","content":"- x","confidence":"high"}]`)
	app.Cfg.Context.CuratorOnSessionEnd = false

	MaybeBackgroundCurate(app, testSession(t))
	time.Sleep(50 * time.Millisecond) // let a (wrong) goroutine fire

	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("background review fired with taste.background off: %d provider calls", n)
	}
}

func TestBackgroundCurateFiresWhenEnabled(t *testing.T) {
	app, calls := curatorApp(t, `[{"section":"state","op":"replace","content":"- refreshed","confidence":"high"}]`)
	app.Cfg.Taste.Background = true

	MaybeBackgroundCurate(app, testSession(t))
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := atomic.LoadInt32(calls); n == 0 {
		t.Fatal("background review did not reach the provider")
	}
}

func TestBackgroundCurateNeedsACuratorModel(t *testing.T) {
	app, calls := curatorApp(t, "[]")
	app.Cfg.Taste.Background = true
	app.Cfg.Context.CuratorModel = ""

	MaybeBackgroundCurate(app, testSession(t))
	time.Sleep(50 * time.Millisecond)

	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("background review ran without a curator model: %d calls", n)
	}
}

func TestBackgroundCurateNeverReturnsAnError(t *testing.T) {
	app, _ := curatorApp(t, "[]")
	app.Cfg.Taste.Background = true
	app.Cfg.Context.CuratorModel = ""
	// A nil app and a misconfigured app must both be no-ops, not panics.
	MaybeBackgroundCurate(nil, "")
	MaybeBackgroundCurate(app, "")
}
