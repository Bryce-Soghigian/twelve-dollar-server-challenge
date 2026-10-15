//go:build linux

package main

import (
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	// The box has 1 vCPU and 2 GB shared with the OS. Default to one P and a soft
	// heap ceiling unless the environment already set them.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(512 << 20)
	}

	path := env("SQLITE_PATH", "feed.db")
	secret := env("JWT_SECRET", "twelve-dollar-challenge")
	host, port := env("HOST", "127.0.0.1"), env("PORT", "3000")

	go checkpointLoop(path)

	mode := env("SERVER_MODE", "uring")
	log.Printf("mode=%s GOMAXPROCS=%d listen=%s:%s", mode, runtime.GOMAXPROCS(0), host, port)
	var err error
	if mode == "std" {
		err = runStd(host+":"+port, path, secret)
	} else {
		// Bind before opening the DB so a failure is reported early.
		lfd, lerr := listenTCP(host, port)
		if lerr != nil {
			log.Fatal(lerr)
		}
		d, derr := openDB(path, true)
		if derr != nil {
			log.Fatal(derr)
		}
		batch := 256
		if v, ok := parseID([]byte(env("WRITE_BATCH_MAX", "256"))); ok {
			batch = int(v)
		}
		err = runURing(lfd, newApp(d, secret), batch)
	}
	log.Fatal(err)
}

// checkpointLoop moves WAL checkpoints (and their fsyncs) off the request path:
// the serving connection runs with wal_autocheckpoint=0.
//
// PASSIVE never blocks the event loop, but under sustained writes it never fully
// catches up, and SQLite only rewinds the WAL after a complete checkpoint, so the
// WAL would grow forever. Past walRestartFrames we use RESTART, which briefly
// holds off the writer (the loop waits on busy_timeout) so the WAL can rewind.
func checkpointLoop(path string) {
	const walRestartFrames = 8192 // 32 MiB of 4 KiB pages
	d, err := openDB(path, false)
	if err != nil {
		log.Fatalf("checkpoint conn: %v", err)
	}
	for range time.Tick(200 * time.Millisecond) {
		t0 := time.Now()
		frames, _, err := d.checkpoint(false)
		if err == nil && frames > walRestartFrames {
			t1 := time.Now()
			_, _, err = d.checkpoint(true)
			log.Printf("checkpoint: %d frames, passive=%v restart=%v", frames, t1.Sub(t0).Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
		} else if dt := time.Since(t0); dt > 50*time.Millisecond {
			log.Printf("checkpoint: %d frames, passive=%v", frames, dt.Round(time.Millisecond))
		}
		if err != nil {
			log.Printf("checkpoint: %v", err)
		}
	}
}
