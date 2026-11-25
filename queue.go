package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type saved struct {
	ID       string `json:"id"`
	Magnet   string `json:"magnet,omitempty"`
	Metadata []byte `json:"metadata,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
}

type task struct {
	saved
	torrent  *torrent.Torrent
	opened   bool
	failure  string
	snapshot row
	lastDown int64
	sampled  time.Time
}

type row struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
	Done     int64   `json:"done"`
	Size     int64   `json:"size"`
	Down     float64 `json:"down"`
	Peers    int     `json:"peers"`
	Paused   bool    `json:"paused"`
	Complete bool    `json:"complete"`
	Folder   bool    `json:"folder"`
}

type queue struct {
	mu        sync.Mutex
	client    *torrent.Client
	storage   storage.ClientImpl
	path      string
	dir       string
	tasks     []*task
	dirty     bool
	saveError string
}

func (q *queue) load() error {
	data, err := os.ReadFile(q.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []saved
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	for _, item := range items {
		t := &task{saved: item}
		if err := q.attach(t); err != nil {
			t.failure = err.Error()
		}
		q.tasks = append(q.tasks, t)
	}
	q.sample()
	return nil
}

func (q *queue) attach(t *task) error {
	var spec *torrent.TorrentSpec
	var err error
	if len(t.Metadata) != 0 {
		var mi *metainfo.MetaInfo
		mi, err = metainfo.Load(bytes.NewReader(t.Metadata))
		if err == nil {
			spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
		}
		if err != nil {
			return fmt.Errorf("не удалось прочитать .torrent: %w", err)
		}
	}
	if t.Magnet != "" {
		magnet, err := torrent.TorrentSpecFromMagnetUri(t.Magnet)
		if err != nil {
			return fmt.Errorf("некорректная magnet-ссылка: %w", err)
		}
		if spec == nil {
			spec = magnet
		} else {
			spec.Trackers = append(spec.Trackers, magnet.Trackers...)
			spec.PeerAddrs = magnet.PeerAddrs
			spec.Webseeds = append(spec.Webseeds, magnet.Webseeds...)
		}
	}
	if spec == nil || (spec.InfoHash == (metainfo.Hash{}) && !spec.InfoHashV2.Ok) {
		return errors.New("в источнике отсутствует хеш BitTorrent")
	}
	for _, hash := range []metainfo.Hash{spec.InfoHash, metainfo.Hash(spec.InfoHashV2.Value[:20])} {
		if hash != (metainfo.Hash{}) {
			if _, exists := q.client.Torrent(hash); exists {
				return errors.New("этот торрент уже есть в очереди")
			}
		}
	}
	spec.DeferStorageOpen = true
	spec.DisallowDataDownload = t.Paused
	spec.Sources = nil
	bt, added, err := q.client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("не удалось добавить торрент: %w", err)
	}
	if !added {
		return errors.New("этот торрент уже есть в очереди")
	}
	t.torrent = bt
	bt.SetOnWriteChunkError(func(err error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		if t.torrent != bt {
			return
		}
		bt.DisallowDataDownload()
		t.failure = "Ошибка записи: " + err.Error()
		q.refresh(t, time.Now())
	})
	return nil
}

func (q *queue) add(item saved) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var id [12]byte
	rand.Read(id[:])
	item.ID = hex.EncodeToString(id[:])
	t := &task{saved: item}
	if err := q.attach(t); err != nil {
		return err
	}
	q.tasks = append([]*task{t}, q.tasks...)
	if err := q.save(); err != nil {
		q.tasks = q.tasks[1:]
		t.torrent.Drop()
		t.torrent = nil
		return err
	}
	q.refresh(t, time.Now())
	return nil
}

func (q *queue) action(id, action string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.tasks {
		if t.ID != id {
			continue
		}
		switch action {
		case "remove":
			tasks := q.tasks
			q.tasks = append(append([]*task{}, tasks[:i]...), tasks[i+1:]...)
			if err := q.save(); err != nil {
				q.tasks = tasks
				return err
			}
			if t.torrent != nil {
				t.torrent.Drop()
				t.torrent = nil
			}
		case "pause", "resume":
			old := t.Paused
			t.Paused = action == "pause"
			if err := q.save(); err != nil {
				t.Paused = old
				return err
			}
			if t.Paused {
				if t.torrent != nil {
					t.torrent.DisallowDataDownload()
				}
			} else {
				t.failure = ""
				if t.torrent == nil {
					if err := q.attach(t); err != nil {
						t.failure = err.Error()
					}
				} else {
					t.torrent.AllowDataDownload()
				}
			}
			q.refresh(t, time.Now())
		case "folder":
			if !t.opened {
				return errors.New("папка будет доступна после получения метаданных")
			}
			folder := filepath.Join(q.dir, t.torrent.InfoHash().HexString())
			if err := os.MkdirAll(folder, 0700); err != nil {
				return err
			}
			return open(folder, false)
		default:
			return errors.New("неизвестное действие")
		}
		return nil
	}
	return errors.New("торрент уже удалён")
}

func (q *queue) refresh(t *task, now time.Time) {
	r := row{ID: t.ID, Paused: t.Paused}
	bt := t.torrent
	if bt != nil {
		if name := bt.Name(); name != "" {
			r.Name = name
		} else {
			r.Name = bt.InfoHash().HexString()
		}
		if bt.Info() != nil {
			r.Size = bt.Length()
			if t.Magnet != "" && len(t.Metadata) == 0 {
				mi := bt.Metainfo()
				var data bytes.Buffer
				if mi.Write(&data) == nil && data.Len() <= maxMetainfo {
					t.Metadata = data.Bytes()
					q.dirty = true
				}
			}
			if !t.opened && t.failure == "" {
				if err := bt.OpenStorage(context.Background(), q.storage); err != nil {
					t.failure = "Не удалось открыть файлы: " + err.Error()
				} else {
					t.opened = true
					bt.DownloadAll()
				}
			}
			if t.opened {
				r.Done = bt.BytesCompleted()
				r.Complete = bt.Complete().Bool()
				r.Folder = true
			}
		}
		stats := bt.Stats()
		down := stats.BytesReadUsefulData.Int64()
		seconds := now.Sub(t.sampled).Seconds()
		if !t.sampled.IsZero() && seconds >= 0.7 {
			r.Down = max(0, float64(down-t.lastDown)/seconds)
		} else if !t.sampled.IsZero() {
			r.Down = t.snapshot.Down
		}
		if t.sampled.IsZero() || seconds >= 0.7 {
			t.lastDown, t.sampled = down, now
		}
		r.Peers = stats.ActivePeers
	}
	if r.Name == "" {
		r.Name = "Торрент"
	}
	switch {
	case t.failure != "":
		r.Status, r.Error = "error", t.failure
	case t.Paused:
		r.Status = "paused"
	case r.Complete:
		r.Status = "complete"
	case !t.opened:
		r.Status = "metadata"
	case r.Down > 0:
		r.Status = "downloading"
	default:
		r.Status = "waiting"
	}
	if t.Paused || t.failure != "" {
		r.Down, r.Peers = 0, 0
	}
	t.snapshot = r
}

func (q *queue) sample() {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for _, t := range q.tasks {
		q.refresh(t, now)
	}
	if q.dirty {
		q.save()
	}
}

func (q *queue) save() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("не удалось сохранить очередь: %w", err)
			q.saveError = err.Error()
		} else {
			q.dirty = false
			q.saveError = ""
		}
	}()
	items := make([]saved, 0, len(q.tasks))
	for _, t := range q.tasks {
		items = append(items, t.saved)
	}
	f, err := os.CreateTemp(filepath.Dir(q.path), ".queue-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(items)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), q.path)
}

func (q *queue) view() any {
	q.mu.Lock()
	defer q.mu.Unlock()
	rows := make([]row, 0, len(q.tasks))
	for _, t := range q.tasks {
		rows = append(rows, t.snapshot)
	}
	return struct {
		Rows      []row  `json:"rows"`
		Directory string `json:"directory"`
		Error     string `json:"error,omitempty"`
	}{rows, q.dir, q.saveError}
}
