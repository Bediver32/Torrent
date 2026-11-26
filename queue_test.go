package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestRestoredTorrentWaitsForResume(t *testing.T) {
	dir := t.TempDir()
	files := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, DisableMmap: true})
	t.Cleanup(func() { files.Close() })

	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = files
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.NoUpload = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	const pieceLength = 16 << 10
	hash := sha1.Sum(make([]byte, pieceLength))
	info, err := bencode.Marshal(metainfo.Info{
		Name: "pending.bin", Length: pieceLength, PieceLength: pieceLength, Pieces: hash[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: info}
	var metadata bytes.Buffer
	if err := mi.Write(&metadata); err != nil {
		t.Fatal(err)
	}

	q := &queue{client: client, storage: files, path: filepath.Join(dir, "queue.json"), dir: dir}
	data, err := json.Marshal([]saved{{ID: "pending", Metadata: metadata.Bytes()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(q.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := q.load(); err != nil {
		t.Fatal(err)
	}
	task := q.tasks[0]
	if !task.opened || task.snapshot.Status != "paused" {
		t.Fatalf("expected a restored task on pause, got %+v", task.snapshot)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		state := task.torrent.PieceState(0)
		if state.Ok && !state.Checking && !state.Marking {
			if state.Priority != torrent.PiecePriorityNone {
				t.Fatalf("restored torrent can request a piece before resume: %+v", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial piece check did not finish: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := q.action(task.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if p := task.torrent.PieceState(0).Priority; p != torrent.PiecePriorityNormal {
		t.Fatalf("resume should make the missing piece requestable, priority = %v", p)
	}
}
